package elasticsearch

import (
	"context"
	"errors"
	"fmt"

	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and runtime identity of this integration.
const Key plugin.Key = "elasticsearch"

// Option customizes a Client created directly with New. Definition-based
// construction intentionally uses only configuration and per-item observers.
type Option func(*clientOptions)

type clientOptions struct {
	bulkObserver BulkObserver
}

// WithBulkObserver installs an instance-wide observer before the bulk worker
// starts. Per-item observers can still be supplied on BulkItem.
func WithBulkObserver(observer BulkObserver) Option {
	return func(options *clientOptions) { options.bulkObserver = observer }
}

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	newPrimaryClient,
	plugin.Options[*Client]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.elasticsearch"),
		Inputs:     plugin.Inputs(),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(client *Client) BulkIndexer { return client }),
		),
		Lifecycle: plugin.Lifecycle[*Client]{
			Init:  initClient,
			Start: startClient,
			Stop:  stopClient,
		},
	},
)

// Definition returns this integration's canonical Definition handle.
func Definition() plugin.Definition { return definition }

// Bundle returns this integration's side-effect-free composition bundle.
func Bundle() plugin.Bundle { return plugin.BundleOf(definition) }

func prepareConfig(config Config) (Config, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return Config{}, err
	}
	return normalized.Config, nil
}

// New constructs a ready standalone Client with the same defaults, validation,
// health probe, and bounded bulk worker as Definition-based construction. Close
// the client when it is no longer needed.
func New(config Config, options ...Option) (*Client, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	settings := clientOptions{}
	for _, option := range options {
		if option != nil {
			option(&settings)
		}
	}
	client, err := newConfiguredClient(normalized, elasticFactory{}, settings.bulkObserver)
	if err != nil {
		return nil, err
	}
	if err := initializeClient(client, context.Background()); err != nil {
		return nil, errors.Join(err, client.closeTransport(context.Background()))
	}
	client.lifecycleMu.Lock()
	client.bulk = newAsyncBulkIndexer(client, client.bulkConfig, client.bulkObserver)
	client.lifecycleMu.Unlock()
	return client, nil
}

func newPrimaryClient(_ plugin.BuildContext, config Config) (*Client, error) {
	return newConfiguredClient(normalizedConfig{Config: config}, elasticFactory{}, nil)
}

// newConfiguredClient acquires only the transport-backed primary value. Init
// performs the health probe and Start owns the long-lived bulk worker.
func newConfiguredClient(config normalizedConfig, factory clientFactory, observer BulkObserver) (*Client, error) {
	client, err := factory.New(config)
	if err != nil {
		if client != nil {
			err = errors.Join(err, client.closeTransport(context.Background()))
		}
		return nil, fmt.Errorf("elasticsearch: initialize client: %w", err)
	}
	if client == nil {
		return nil, errors.New("elasticsearch: initialize client returned nil")
	}
	client.lifecycleMu.Lock()
	client.bulkConfig = config.Bulk
	client.bulkObserver = observer
	client.healthProbe = config.HealthProbe
	client.lifecycleMu.Unlock()
	return client, nil
}

func initClient(client *Client, ctx *plugin.Context) error {
	if client == nil {
		return errors.New("elasticsearch: Init requires a non-nil client")
	}
	if ctx == nil {
		return errors.New("elasticsearch: Init requires a non-nil plugin context")
	}
	return initializeClient(client, ctx)
}

func initializeClient(client *Client, ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	client.lifecycleMu.Lock()
	healthProbe := client.healthProbe
	client.lifecycleMu.Unlock()
	if healthProbe {
		if err := client.Health(ctx); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func startClient(client *Client, ctx *plugin.Context) error {
	if client == nil {
		return errors.New("elasticsearch: Start requires a non-nil client")
	}
	if ctx == nil {
		return errors.New("elasticsearch: Start requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	client.lifecycleMu.Lock()
	defer client.lifecycleMu.Unlock()
	if client.stopDone != nil || client.closed.Load() {
		return errors.New("elasticsearch: cannot start a closed client")
	}
	if client.bulk != nil {
		return errors.New("elasticsearch: client is already started")
	}
	bulk, accepted := newManagedAsyncBulkIndexer(
		client,
		client.bulkConfig,
		client.bulkObserver,
		ctx.Go,
	)
	if !accepted {
		return errors.New("elasticsearch: bulk worker task admission was rejected")
	}
	client.bulk = bulk
	return nil
}

func stopClient(client *Client, ctx context.Context) error {
	if client == nil {
		return nil
	}
	return client.Close(ctx)
}

// SetBulkObserver installs an instance-wide observer before Start. It is useful
// when a program constructs and wires the primary value itself; per-item
// observers remain available on BulkItem.
func (client *Client) SetBulkObserver(observer BulkObserver) error {
	if client == nil {
		return errors.New("elasticsearch: cannot configure a nil client")
	}
	client.lifecycleMu.Lock()
	defer client.lifecycleMu.Unlock()
	if client.bulk != nil || client.stopDone != nil || client.closed.Load() {
		return errors.New("elasticsearch: bulk observer is frozen after Start begins")
	}
	client.bulkObserver = observer
	return nil
}

// Add submits one item to this client's bounded bulk worker.
func (client *Client) Add(ctx context.Context, item BulkItem) error {
	bulk := client.currentBulk()
	if bulk == nil {
		return ErrBulkClosed
	}
	return bulk.Add(ctx, item)
}

// Flush waits for every item admitted before the call to receive a result.
func (client *Client) Flush(ctx context.Context) error {
	bulk := client.currentBulk()
	if bulk == nil {
		return ErrBulkClosed
	}
	return bulk.Flush(ctx)
}

// Stats returns a lock-free snapshot of this client's bulk worker counters.
func (client *Client) Stats() BulkStats {
	bulk := client.currentBulk()
	if bulk == nil {
		return BulkStats{}
	}
	return bulk.Stats()
}

func (client *Client) currentBulk() *asyncBulkIndexer {
	if client == nil {
		return nil
	}
	client.lifecycleMu.Lock()
	bulk := client.bulk
	client.lifecycleMu.Unlock()
	return bulk
}

// Close starts one shared cleanup operation. It first stops bulk admission and
// drains accepted work, then closes the transport. Cleanup continues after a
// caller's context expires; a later call can retrieve the shared final result.
func (client *Client) Close(ctx context.Context) error {
	if client == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	client.lifecycleMu.Lock()
	if client.stopDone == nil {
		client.stopDone = make(chan struct{})
		bulk := client.bulk
		client.bulk = nil
		done := client.stopDone
		go client.finishClose(bulk, done)
	}
	done := client.stopDone
	client.lifecycleMu.Unlock()

	select {
	case <-done:
		client.lifecycleMu.Lock()
		err := client.stopErr
		client.lifecycleMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (client *Client) finishClose(bulk *asyncBulkIndexer, done chan struct{}) {
	var errs []error
	if bulk != nil {
		if err := bulk.Close(context.Background()); err != nil {
			errs = append(errs, err)
		}
	}
	if err := client.closeTransport(context.Background()); err != nil {
		errs = append(errs, err)
	}

	client.lifecycleMu.Lock()
	client.stopErr = errors.Join(errs...)
	close(done)
	client.lifecycleMu.Unlock()
}

var _ BulkIndexer = (*Client)(nil)
