package objectstorage

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and dependency identity of this integration.
const Key plugin.Key = "objectstorage"

// managedStore is the one framework-owned primary value. It presents the
// backend-neutral Store contract while retaining sole ownership of the selected
// backend and making Close idempotent even if a future backend is not.
type managedStore struct {
	backend Store
	// probeReachability is set only for a remote backend whose operator did not
	// opt out. A local directory has no peer to probe.
	probeReachability bool

	closeOnce sync.Once
	closeErr  error
}

var _ Store = (*managedStore)(nil)

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: defaultConfig,
		Prepare:  prepareConfig,
	},
	newStore,
	plugin.Options[*managedStore]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.objectstorage"),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(store *managedStore) Store { return store }),
			plugin.ExportAs(func(store *managedStore) health.Contributor { return store }),
		),
		Lifecycle: plugin.Lifecycle[*managedStore]{
			Stop: stopStore,
		},
	},
)

// Definition returns this package's canonical declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free object storage composition.
func Bundle() plugin.Bundle { return plugin.BundleOf(definition) }

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func newStore(_ plugin.BuildContext, cfg Config) (*managedStore, error) {
	var (
		backend Store
		err     error
	)
	switch cfg.Backend {
	case BackendLocal:
		backend, err = newLocalStore(cfg)
	case BackendS3:
		backend, err = newS3Store(cfg)
	default:
		err = fmt.Errorf("objectstorage: unsupported backend %q", cfg.Backend)
	}
	if err != nil {
		return nil, err
	}
	return &managedStore{backend: backend, probeReachability: cfg.Backend == BackendS3 && cfg.S3.HealthProbe}, nil
}

func (s *managedStore) Put(ctx context.Context, key string, body io.Reader, options PutOptions) (ObjectInfo, error) {
	return s.backend.Put(ctx, key, body, options)
}

func (s *managedStore) Get(ctx context.Context, key string) (*Object, error) {
	return s.backend.Get(ctx, key)
}

func (s *managedStore) Delete(ctx context.Context, key string) error {
	return s.backend.Delete(ctx, key)
}

func (s *managedStore) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	return s.backend.Stat(ctx, key)
}

func (s *managedStore) List(ctx context.Context, options ListOptions) (ListResult, error) {
	return s.backend.List(ctx, options)
}

// Close releases the selected backend once. Lifecycle shutdown uses stopStore
// so the caller's shutdown deadline bounds its wait.
func (s *managedStore) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.backend != nil {
			s.closeErr = s.backend.Close()
		}
	})
	return s.closeErr
}

// stopStore starts the shared idempotent close and bounds only this caller's
// wait. Cleanup continues after a timed-out caller; a later call observes the
// backend's final close result. It is safe before any optional lifecycle stage.
func stopStore(store *managedStore, ctx context.Context) error {
	if store == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	done := make(chan error, 1)
	go func() { done <- store.Close() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
