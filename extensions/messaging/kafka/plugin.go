package kafka

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable Definition and configuration identity.
const Key plugin.Key = "kafka"

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Client, error) {
		return newManagedClient(cfg, kafkaFactory{})
	},
	plugin.Options[*Client]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.kafka"),
		Inputs:     plugin.Inputs(),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(client *Client) Producer { return client }),
		),
		Lifecycle: plugin.Lifecycle[*Client]{
			Start: (*Client).start,
			Stop:  (*Client).stop,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns Kafka's canonical immutable Definition handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free Kafka composition bundle.
func Bundle() plugin.Bundle { return bundle }

func prepareConfig(cfg Config) (Config, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return Config{}, err
	}
	return normalized.Config, nil
}

func newManagedClient(cfg Config, factory backendFactory) (*Client, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	writer, err := factory.NewWriter(normalized)
	if err != nil {
		if writer != nil {
			err = errors.Join(err, writer.Close())
		}
		return nil, fmt.Errorf("kafka: initialize producer: %w", err)
	}
	if writer == nil {
		return nil, errors.New("kafka: initialize producer returned a nil writer")
	}
	client := newClient(writer)
	client.normalized = normalized
	client.factory = factory
	return client, nil
}

type runningConsumer struct {
	name    string
	config  ConsumerConfig
	handler Handler
	reader  messageReader
}

// RegisterHandler binds a configured consumer name to application behavior.
// Applications normally call it from the factory of a Plugin that declares a
// typed Ref to this Client. All factories run before lifecycle Start, where the
// registrations are frozen.
func (c *Client) RegisterHandler(name string, handler Handler) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("kafka: consumer handler name is required")
	}
	if isNilHandler(handler) {
		return fmt.Errorf("kafka: consumer handler %q is nil", name)
	}
	c.handlerMu.Lock()
	defer c.handlerMu.Unlock()
	if c.frozen {
		return ErrHandlersFrozen
	}
	c.handlers[name] = handler
	return nil
}

func isNilHandler(handler Handler) bool {
	if handler == nil {
		return true
	}
	value := reflect.ValueOf(handler)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// start freezes registrations, creates all readers transactionally, and
// submits each long-running consumer task while XBC's Start admission window is
// open. Tasks wait on the runtime-owned traffic gate before the first fetch.
func (c *Client) start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("kafka: Start requires a non-nil plugin context")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("kafka: Start canceled: %w", err)
	}
	gate := ctx.TrafficGate()
	if gate == nil {
		return errors.New("kafka: Start requires a runtime traffic gate")
	}

	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.handlerMu.Lock()
	if c.frozen {
		c.handlerMu.Unlock()
		return errors.New("kafka: client has already started")
	}
	c.frozen = true
	handlers := make(map[string]Handler, len(c.handlers))
	for name, handler := range c.handlers {
		handlers[name] = handler
	}
	c.handlerMu.Unlock()

	started := false
	defer func() {
		if started {
			return
		}
		c.handlerMu.Lock()
		c.frozen = false
		c.handlerMu.Unlock()
	}()

	c.stateMu.Lock()
	if c.stopping || c.closing.Load() || c.writerClosed() {
		c.stateMu.Unlock()
		return errors.New("kafka: client is stopped")
	}
	if c.started || len(c.consumers) != 0 || c.runCancel != nil {
		c.stateMu.Unlock()
		return errors.New("kafka: client has already started")
	}
	cfg := c.normalized
	factory := c.factory
	c.stateMu.Unlock()

	names := make([]string, 0, len(cfg.consumers))
	for name := range cfg.consumers {
		names = append(names, name)
	}
	sort.Strings(names)
	created := make([]*runningConsumer, 0, len(names))
	rollbackReaders := func() error {
		var errs []error
		for index := len(created) - 1; index >= 0; index-- {
			if err := created[index].reader.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close consumer %q during rollback: %w", created[index].name, err))
			}
		}
		return errors.Join(errs...)
	}
	for _, name := range names {
		handler := handlers[name]
		if handler == nil {
			return errors.Join(fmt.Errorf("kafka: no handler registered for consumer %q", name), rollbackReaders())
		}
		if factory == nil {
			return errors.Join(errors.New("kafka: consumer backend is unavailable"), rollbackReaders())
		}
		reader, err := factory.NewReader(cfg, name, cfg.consumers[name])
		if err != nil {
			if reader != nil {
				created = append(created, &runningConsumer{name: name, reader: reader})
			}
			return errors.Join(fmt.Errorf("kafka: create consumer %q: %w", name, err), rollbackReaders())
		}
		if reader == nil {
			return errors.Join(fmt.Errorf("kafka: create consumer %q returned a nil reader", name), rollbackReaders())
		}
		created = append(created, &runningConsumer{
			name:    name,
			config:  cfg.consumers[name],
			handler: handler,
			reader:  reader,
		})
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	c.stateMu.Lock()
	if c.stopping || c.closing.Load() {
		c.stateMu.Unlock()
		runCancel()
		return errors.Join(errors.New("kafka: client is stopping"), rollbackReaders())
	}
	c.consumers = created
	c.runCancel = runCancel
	c.stateMu.Unlock()

	logger := ctx.Log()
	for _, consumer := range created {
		consumer := consumer
		c.loops.Add(1)
		accepted := ctx.GoCritical(func(taskCtx context.Context) {
			defer c.loops.Done()
			c.runConsumer(taskCtx, runCtx, gate, logger, consumer)
		})
		if !accepted {
			c.loops.Done()
			runCancel()
			closeErr := rollbackReaders()
			c.loops.Wait()
			c.stateMu.Lock()
			c.consumers = nil
			c.runCancel = nil
			c.stateMu.Unlock()
			return errors.Join(errors.New("kafka: runtime rejected consumer task during Start"), closeErr)
		}
	}

	c.stateMu.Lock()
	c.started = true
	c.stateMu.Unlock()
	started = true
	return nil
}

func (c *Client) writerClosed() bool {
	c.produceMu.RLock()
	defer c.produceMu.RUnlock()
	return c.closed || c.writer == nil
}

func (c *Client) runConsumer(taskCtx, runCtx context.Context, gate <-chan struct{}, logger log.Logger, consumer *runningConsumer) {
	consumerCtx, cancel := context.WithCancel(taskCtx)
	stopLink := context.AfterFunc(runCtx, cancel)
	defer stopLink()
	defer cancel()

	select {
	case <-gate:
	case <-consumerCtx.Done():
		return
	}
	c.consume(consumerCtx, logger, consumer)
}

func (c *Client) consume(ctx context.Context, logger log.Logger, consumer *runningConsumer) {
	for {
		message, err := consumer.reader.Fetch(ctx)
		if err != nil {
			if ctx.Err() != nil || c.isStopping() {
				return
			}
			logger.Warn("kafka consumer fetch failed", "consumer", consumer.name, "error", err)
			if !sleepContext(ctx, consumer.config.FetchErrorBackoff) {
				return
			}
			continue
		}
		err = c.dispatch(ctx, consumer, message.message)
		if err != nil {
			logger.Error("kafka consumer handler exhausted retries", "consumer", consumer.name, "topic", message.message.Topic, "partition", message.message.Partition, "offset", message.message.Offset, "error", err)
			if consumer.config.ErrorPolicy == ErrorPolicyStop {
				return
			}
		}
		if err := c.commit(ctx, consumer, message); err != nil {
			if ctx.Err() != nil || c.isStopping() {
				return
			}
			logger.Error("kafka consumer commit failed", "consumer", consumer.name, "topic", message.message.Topic, "partition", message.message.Partition, "offset", message.message.Offset, "error", err)
			if consumer.config.ErrorPolicy == ErrorPolicyStop {
				return
			}
		}
	}
}

func (c *Client) dispatch(ctx context.Context, consumer *runningConsumer, message Message) error {
	var err error
	for attempt := 1; attempt <= consumer.config.HandlerMaxAttempts; attempt++ {
		if err = consumer.handler.Handle(ctx, message); err == nil {
			return nil
		}
		if attempt < consumer.config.HandlerMaxAttempts && !sleepContext(ctx, consumer.config.HandlerRetryBackoff) {
			return ctx.Err()
		}
	}
	return err
}

func (c *Client) commit(ctx context.Context, consumer *runningConsumer, message fetchedMessage) error {
	var err error
	for attempt := 1; attempt <= consumer.config.HandlerMaxAttempts; attempt++ {
		if err = consumer.reader.Commit(ctx, message); err == nil {
			return nil
		}
		if attempt < consumer.config.HandlerMaxAttempts && !sleepContext(ctx, consumer.config.HandlerRetryBackoff) {
			return ctx.Err()
		}
	}
	return err
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) isStopping() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.stopping
}

// stop rejects new production and starts exactly one cleanup. It is safe from
// every owned partial lifecycle state and all callers observe the same cleanup
// result, subject to their own context deadline.
func (c *Client) stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	c.lifecycleMu.Lock()
	c.stateMu.Lock()
	owner := !c.stopping
	var consumers []*runningConsumer
	var cancel context.CancelFunc
	if owner {
		c.stopping = true
		c.closing.Store(true)
		consumers = append([]*runningConsumer(nil), c.consumers...)
		c.consumers = nil
		cancel = c.runCancel
		c.runCancel = nil
		if c.stopDone == nil {
			c.stopDone = make(chan struct{})
		}
	}
	done := c.stopDone
	c.stateMu.Unlock()
	c.lifecycleMu.Unlock()

	if done == nil {
		return nil
	}
	if owner {
		c.finishStop(consumers, cancel, done)
	} else {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.stateMu.Lock()
	err := c.stopErr
	c.stateMu.Unlock()
	return err
}

func (c *Client) finishStop(consumers []*runningConsumer, cancel context.CancelFunc, done chan struct{}) {
	if cancel != nil {
		cancel()
	}
	var errs []error
	for index := len(consumers) - 1; index >= 0; index-- {
		if err := consumers[index].reader.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close consumer %q: %w", consumers[index].name, err))
		}
	}
	c.loops.Wait()
	if err := c.close(); err != nil {
		errs = append(errs, fmt.Errorf("close producer: %w", err))
	}
	result := errors.Join(errs...)
	c.stateMu.Lock()
	c.stopErr = result
	close(done)
	c.stateMu.Unlock()
}
