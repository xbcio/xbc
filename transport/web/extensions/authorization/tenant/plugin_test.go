package tenant

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xbcio/xbc/transport/web"
)

func TestPluginInitFailureAndDuplicateLifecycle(t *testing.T) {
	if err := New().Init(nil); err == nil {
		t.Fatal("Init(nil) succeeded")
	}
	invalid := New()
	invalid.cfg.Header = "bad header"
	if err := invalid.Init(testContext()); err == nil {
		t.Fatal("invalid config initialized")
	}
	if invalid.state.Load() != nil {
		t.Fatal("failed Init published runtime state")
	}
	if err := invalid.Init(testContext()); err == nil {
		t.Fatal("unchanged invalid config initialized on retry")
	}

	p := New()
	if err := p.Init(testContext()); err != nil {
		t.Fatal(err)
	}
	if err := p.Init(testContext()); err == nil {
		t.Fatal("second Init succeeded")
	}
}

func TestConcurrentInitPublishesExactlyOneImmutableState(t *testing.T) {
	var resolverCalls atomic.Int32
	resolver := ResolverFunc(func(context.Context, web.Principal, string) (Tenant, bool, error) {
		resolverCalls.Add(1)
		return Tenant{ID: "acme"}, true, nil
	})
	p := New(WithResolver(resolver))
	const attempts = 32
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Init(testContext()); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || p.state.Load() == nil {
		t.Fatalf("successful Init calls=%d state=%#v", successes.Load(), p.state.Load())
	}
	if resolverCalls.Load() != 0 {
		t.Fatal("Init unexpectedly invoked Resolver")
	}
}

func TestResolverFuncPropagatesErrors(t *testing.T) {
	want := errors.New("lookup unavailable")
	resolver := ResolverFunc(func(context.Context, web.Principal, string) (Tenant, bool, error) {
		return Tenant{}, false, want
	})
	_, _, err := resolver.ResolveTenant(context.Background(), web.Principal{Subject: "alice"}, "acme")
	if !errors.Is(err, want) {
		t.Fatalf("ResolverFunc error=%v", err)
	}
}
