package apikey

import (
	"context"
	"sync"
	"testing"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

type fixedRepository struct{}

func (fixedRepository) Lookup(context.Context, string, KeyDigest) (Credential, bool, error) {
	return Credential{Subject: "fixed"}, true, nil
}

func staticConfig(secret string) Config {
	cfg := DefaultConfig()
	cfg.Static = []StaticCredential{{
		ID:      "service",
		Subject: "svc:one",
		SHA256:  HashKey(secret).String(),
	}}
	return cfg
}

func TestDefinitionAndMiddlewareContract(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	p, err := New(staticConfig("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	order := p.Order()
	if order.Phase != web.PhaseAuth || len(order.After) != 0 || len(order.Before) != 0 || p.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
}

func TestNewSynchronouslyBuildsDigestOnlyStaticState(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	cfg := staticConfig(secret)
	cfg.Header = " X-API-Key "

	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.state == nil || p.state.static == nil || p.state.repo != p.state.static {
		t.Fatal("static runtime state was not built before New returned")
	}
	if p.state.config.header != defaultHeader {
		t.Fatalf("normalized header = %q", p.state.config.header)
	}
	stored := p.state.static.state.Load().credentials
	if len(stored) != 1 || stored[0].digest != HashKey(secret) {
		t.Fatalf("compiled digest = %#v", stored)
	}
}

func TestNewPreservesInjectedRepositoryOption(t *testing.T) {
	cfg := DefaultConfig()
	repository := fixedRepository{}
	p, err := New(cfg, WithRepository(repository))
	if err != nil {
		t.Fatal(err)
	}
	if p.state == nil || p.state.static != nil || p.state.repo != repository {
		t.Fatalf("injected repository state = %#v", p.state)
	}

	cfg.Static = staticConfig("0123456789abcdef0123456789abcdef").Static
	if _, err := New(cfg, WithRepository(repository)); err == nil {
		t.Fatal("New accepted both static credentials and an injected repository")
	}
	if _, err := New(DefaultConfig(), WithRepository(nil)); err == nil {
		t.Fatal("New accepted a nil injected repository")
	}
}

func TestNewRejectsInvalidConfigurationBeforeReturningPlugin(t *testing.T) {
	cfg := staticConfig("0123456789abcdef0123456789abcdef")
	cfg.MinKeyBytes = 8
	if p, err := New(cfg); err == nil || p != nil {
		t.Fatalf("New() = %#v, %v; want nil plugin and error", p, err)
	}

	cfg = DefaultConfig()
	if p, err := New(cfg); err == nil || p != nil {
		t.Fatalf("New() without repository = %#v, %v; want nil plugin and error", p, err)
	}
}

func TestStaticRepositoryRotationIsAtomicForReaders(t *testing.T) {
	oldKey := "old-0123456789abcdef0123456789abcdef"
	newKey := "new-0123456789abcdef0123456789abcdef"
	repository, err := NewStaticRepository([]StaticCredential{{Subject: "old", SHA256: HashKey(oldKey).String()}})
	if err != nil {
		t.Fatal(err)
	}
	oldDigest := HashKey(oldKey)
	newDigest := HashKey(newKey)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_, _, _ = repository.Lookup(context.Background(), "", oldDigest)
				_, _, _ = repository.Lookup(context.Background(), "", newDigest)
			}
		}()
	}
	if err := repository.Replace([]StaticCredential{{Subject: "new", SHA256: HashKey(newKey).String()}}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if _, ok, _ := repository.Lookup(context.Background(), "", oldDigest); ok {
		t.Fatal("old key survived rotation")
	}
	credential, ok, err := repository.Lookup(context.Background(), "", newDigest)
	if err != nil || !ok || credential.Subject != "new" {
		t.Fatalf("new lookup = %#v, %v, %v", credential, ok, err)
	}
}
