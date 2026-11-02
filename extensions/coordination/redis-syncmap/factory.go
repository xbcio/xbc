package redissyncmap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/extensions/coordination/syncmap"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// factory owns every map one configured section creates. It is the Definition's
// primary value and the value exported as syncmap.Factory.
type factory struct {
	client redis.UniversalClient
	config Config
	log    log.Logger

	mu      sync.Mutex
	maps    map[string]*syncMap
	started bool
	closed  bool
}

var _ syncmap.Factory = (*factory)(nil)

func newFactory(logger log.Logger, client redis.UniversalClient, config Config) (*factory, error) {
	if client == nil {
		return nil, errors.New("redis-syncmap: factory requires a non-nil redis client")
	}
	return &factory{
		client: client,
		config: config,
		log:    logger,
		maps:   make(map[string]*syncMap),
	}, nil
}

// Map creates one named map. Maps must be created before the factory starts:
// their subscriptions are launched as managed Start tasks, and a factory that
// has already started cannot adopt another subscriber.
func (f *factory) Map(name string, options ...syncmap.Option) (syncmap.Map, error) {
	if f == nil {
		return nil, syncmap.ErrClosed
	}
	if !validMapName(name) {
		return nil, fmt.Errorf("%w: %q", syncmap.ErrInvalidName, name)
	}
	resolved, err := f.resolveOptions(options)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, syncmap.ErrClosed
	}
	if f.started {
		return nil, syncmap.ErrStarted
	}
	if _, exists := f.maps[name]; exists {
		return nil, fmt.Errorf("%w: %q", syncmap.ErrDuplicate, name)
	}
	created, err := newSyncMap(f.client, f.log, f.config.Namespace, name, resolved)
	if err != nil {
		return nil, err
	}
	f.maps[name] = created
	return created, nil
}

// resolveOptions applies the factory defaults, then the caller's options, and
// validates the result. Every rule here is decidable without I/O.
func (f *factory) resolveOptions(options []syncmap.Option) (syncmap.Options, error) {
	resolved := syncmap.Options{
		Persist: f.config.mode(),
		TTL:     f.config.TTL,
		MaxSize: f.config.MaxSize,
	}
	for index, option := range options {
		if option == nil {
			return syncmap.Options{}, fmt.Errorf("%w: option %d is nil", syncmap.ErrInvalidOption, index)
		}
		option(&resolved)
	}
	switch resolved.Persist {
	case syncmap.PersistNone:
	case syncmap.PersistTTL:
		if resolved.TTL <= 0 {
			return syncmap.Options{}, fmt.Errorf("%w: persist ttl requires a positive ttl", syncmap.ErrInvalidOption)
		}
	default:
		return syncmap.Options{}, fmt.Errorf("%w: unknown persist mode %q", syncmap.ErrInvalidOption, resolved.Persist)
	}
	if resolved.MaxSize < 0 {
		return syncmap.Options{}, fmt.Errorf("%w: max size cannot be negative, got %d", syncmap.ErrInvalidOption, resolved.MaxSize)
	}
	return resolved, nil
}

// sortedMaps returns the factory's maps in name order. Determinism matters on
// the start path: the first failing map is the one the error names, so an
// unordered map would make that error vary between runs.
func (f *factory) sortedMaps() []*syncMap {
	names := make([]string, 0, len(f.maps))
	for name := range f.maps {
		names = append(names, name)
	}
	sort.Strings(names)
	maps := make([]*syncMap, len(names))
	for index, name := range names {
		maps[index] = f.maps[name]
	}
	return maps
}

func initFactory(f *factory, ctx *plugin.Context) error {
	if f == nil {
		return errors.New("redis-syncmap: Init requires a non-nil factory")
	}
	if ctx == nil {
		return errors.New("redis-syncmap: Init requires a non-nil plugin context")
	}
	f.mu.Lock()
	f.log = ctx.Log()
	f.mu.Unlock()
	return nil
}

func startFactory(f *factory, ctx *plugin.Context) error {
	if f == nil {
		return errors.New("redis-syncmap: Start requires a non-nil factory")
	}
	if ctx == nil {
		return errors.New("redis-syncmap: Start requires a non-nil plugin context")
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return syncmap.ErrClosed
	}
	f.started = true
	maps := f.sortedMaps()
	f.mu.Unlock()

	for _, created := range maps {
		if err := created.start(ctx); err != nil {
			return fmt.Errorf("redis-syncmap: start map %q: %w", created.name, err)
		}
	}
	return nil
}

// closeFactory is shared by Drain and Stop and is idempotent. Drain already
// retracts the factory's externally visible participation: the subscription
// loops are its only external activity, and a drained map must not keep
// applying peer writes behind a dependent's back. Stop therefore has nothing
// left to do after a successful Drain.
func closeFactory(f *factory) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	maps := f.sortedMaps()
	f.mu.Unlock()

	var failures []error
	for _, created := range maps {
		if err := created.Destroy(); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func drainFactory(f *factory, _ context.Context) error { return closeFactory(f) }

func stopFactory(f *factory, _ context.Context) error { return closeFactory(f) }
