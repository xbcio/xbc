package outbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/xbcio/xbc/plugin"
	"gorm.io/gorm"
)

// Key is the stable Definition and configuration identity.
const Key plugin.Key = "outbox"

// gormPluginKey is intentionally local. Outbox depends on the *gorm.DB
// contract and its stable producer identity, not on the GORM integration's
// implementation package.
const gormPluginKey plugin.Key = "gorm"

var definition = plugin.DefinePlanned(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	plan,
	plugin.Options[*Service]{
		Activation: plugin.WhenConfigured("plugins.outbox"),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(service *Service) Dispatcher { return service }),
		),
		Lifecycle: plugin.Lifecycle[*Service]{
			Migrate:     (*Service).migrate,
			Start:       (*Service).start,
			OpenTraffic: (*Service).openTraffic,
			Stop:        (*Service).stop,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns the package's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns side-effect-free composition data for the outbox feature.
func Bundle() plugin.Bundle { return bundle }

func prepareConfig(config Config) (Config, error) { return config.normalized() }

// plan is pure: configuration selects immutable input tokens and the factory
// closure, but no primary value or runtime resource is created here.
func plan(config Config) (plugin.Plan[*Service], error) {
	database := plugin.RefToInstance[*gorm.DB](gormPluginKey, config.DBInstance)
	publisher := plugin.OptionalOne[Publisher]()
	inputs := plugin.Inputs(database)
	if config.Worker.Enabled {
		inputs = plugin.Inputs(database, publisher)
	}

	return plugin.PlanOf(inputs, func(context plugin.BuildContext) (*Service, error) {
		db := database.Get(context).Value
		if db == nil {
			return nil, fmt.Errorf("outbox: GORM database instance %q is nil", config.DBInstance)
		}

		var selected Publisher
		if config.Worker.Enabled {
			entry, ok := publisher.Get(context)
			if !ok || entry.Value == nil {
				return nil, errors.New("outbox: worker is enabled but no Publisher is available")
			}
			selected = entry.Value
		}

		store, err := NewSQLStore(db, config.Table)
		if err != nil {
			return nil, err
		}
		return newConfiguredService(config, store, selected), nil
	}), nil
}

// migrate is intentionally inert unless migrate=true. It is called only from
// XBC's explicit migration stage and never from the primary factory.
func (service *Service) migrate(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("outbox: Migrate requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	service.lifecycleMu.Lock()
	if service.stopping || service.stopped {
		service.lifecycleMu.Unlock()
		return ErrClosed
	}
	enabled, store := service.config.Migrate, service.store
	service.lifecycleMu.Unlock()
	if !enabled {
		return nil
	}
	return store.Migrate(ctx)
}

// start prepares the optional dispatcher and admits its managed task. The task
// waits on XBC's global traffic gate, so Start performs no polling or publishing.
func (service *Service) start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("outbox: Start requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	service.lifecycleMu.Lock()
	switch {
	case service.stopping || service.stopped:
		service.lifecycleMu.Unlock()
		return ErrClosed
	case service.starting || service.started:
		service.lifecycleMu.Unlock()
		return errors.New("outbox: Start called more than once")
	}
	service.starting = true
	config, store, publisher := service.config, service.store, service.publisher
	trafficGate := ctx.TrafficGate()
	service.lifecycleMu.Unlock()

	var dispatch *worker
	var err error
	if config.Worker.Enabled {
		dispatch, err = newWorker(store, publisher, config.Worker)
		if err != nil {
			service.finishFailedStart(nil)
			return fmt.Errorf("outbox: prepare worker: %w", err)
		}

		service.lifecycleMu.Lock()
		if service.stopping || service.stopped {
			service.starting = false
			service.lifecycleMu.Unlock()
			dispatch.requestStop()
			return ErrClosed
		}
		service.worker = dispatch
		service.lifecycleMu.Unlock()

		accepted := ctx.GoCritical(func(runContext context.Context) {
			select {
			case <-trafficGate:
				dispatch.run(runContext)
			case <-dispatch.stop:
			case <-runContext.Done():
				dispatch.requestStop()
			}
		})
		if !accepted {
			service.finishFailedStart(dispatch)
			return errors.New("outbox: runtime is not accepting worker tasks")
		}
	}

	service.lifecycleMu.Lock()
	if service.stopping || service.stopped {
		service.starting = false
		service.lifecycleMu.Unlock()
		if dispatch != nil {
			dispatch.requestStop()
		}
		return ErrClosed
	}
	service.starting = false
	service.started = true
	service.lifecycleMu.Unlock()
	return nil
}

func (service *Service) finishFailedStart(dispatch *worker) {
	if dispatch != nil {
		dispatch.requestStop()
	}
	service.lifecycleMu.Lock()
	if service.worker == dispatch {
		service.worker = nil
	}
	service.starting = false
	service.lifecycleMu.Unlock()
}

// openTraffic releases the prepared worker only after every Plugin has
// completed Start. Task admission happened synchronously during Start.
func (service *Service) openTraffic(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("outbox: OpenTraffic requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	service.lifecycleMu.Lock()
	defer service.lifecycleMu.Unlock()
	switch {
	case service.stopping || service.stopped:
		return ErrClosed
	case !service.started:
		return errors.New("outbox: OpenTraffic called before Start")
	case service.opened:
		return errors.New("outbox: OpenTraffic called more than once")
	}
	if service.worker != nil {
		if err := service.worker.schedule(); err != nil {
			return err
		}
	}
	service.opened = true
	return nil
}

// stop closes enqueue and claim admission exactly once. Shared cleanup is
// detached from each caller's deadline, so a timed-out caller cannot poison a
// later call. It is safe before Start, after a failed or partial Start, and
// after traffic has opened.
func (service *Service) stop(ctx context.Context) error {
	if service == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	service.lifecycleMu.Lock()
	if service.stopDone == nil {
		service.stopping = true
		service.stopDone = make(chan struct{})
		admissionDone := service.stopAdmission()
		var workerDone <-chan struct{}
		if service.worker != nil {
			workerDone = service.worker.requestStop()
		}
		done := service.stopDone
		go service.finishStop(admissionDone, workerDone, done)
	}
	done := service.stopDone
	service.lifecycleMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-done:
		service.lifecycleMu.Lock()
		err := service.stopErr
		service.lifecycleMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (service *Service) finishStop(admissionDone, workerDone <-chan struct{}, done chan struct{}) {
	if admissionDone != nil {
		<-admissionDone
	}
	if workerDone != nil {
		<-workerDone
	}

	service.lifecycleMu.Lock()
	service.stopErr = nil
	service.starting = false
	service.started = false
	service.opened = false
	service.stopping = false
	service.stopped = true
	service.worker = nil
	service.publisher = nil
	close(done)
	service.lifecycleMu.Unlock()
}
