package casbin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInlineModelPolicyAndRoleInheritance(t *testing.T) {
	p, _ := initializedPlugin(t, func(cfg *Config) {
		cfg.Model = permissionModel
		cfg.Policy = "p, reader, reports:read\ng, alice, reader"
	})
	enforcer, ok := p.Enforcer()
	if !ok {
		t.Fatal("missing enforcer")
	}
	allowed, err := enforcer.Enforce("alice", "reports:read")
	if err != nil || !allowed {
		t.Fatalf("Enforce inherited permission = (%v, %v), want true", allowed, err)
	}
	allowed, err = enforcer.Enforce("alice", "reports:write")
	if err != nil || allowed {
		t.Fatalf("Enforce absent permission = (%v, %v), want false", allowed, err)
	}
}

func TestPolicyValidationRejectsUnknownAndWrongArityRules(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy string
		want   string
	}{
		{name: "unknown type", policy: "x, alice, read", want: "unknown policy section"},
		{name: "wrong arity", policy: "p, alice", want: "has 1 fields, want 2"},
		{name: "bad csv", policy: `p, "alice, read`, want: "policy line 1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Policy = test.policy
			_, err := New(cfg)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("New() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadPolicyAtomicallyReplacesFilePolicyAndPreservesLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.csv")
	writePolicy(t, path, "p, alice, reports:read\n")
	p, _ := initializedPlugin(t, func(cfg *Config) { cfg.PolicyFile = path })
	enforcer, _ := p.Enforcer()
	allowed, err := enforcer.Enforce("alice", "reports:read")
	assertEnforce(t, allowed, err, true)

	writePolicy(t, path, "p, bob, reports:read\n")
	if err := p.LoadPolicy(); err != nil {
		t.Fatalf("LoadPolicy() error = %v", err)
	}
	allowed, err = enforcer.Enforce("alice", "reports:read")
	assertEnforce(t, allowed, err, false)
	allowed, err = enforcer.Enforce("bob", "reports:read")
	assertEnforce(t, allowed, err, true)

	writePolicy(t, path, "p, malformed\n")
	if err := p.LoadPolicy(); err == nil {
		t.Fatal("LoadPolicy() malformed policy error = nil")
	}
	allowed, err = enforcer.Enforce("bob", "reports:read")
	assertEnforce(t, allowed, err, true)
}

func TestPeriodicReloadAndStop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.csv")
	writePolicy(t, path, "p, alice, reports:read\n")
	p, host := initializedPlugin(t, func(cfg *Config) {
		cfg.PolicyFile = path
		cfg.ReloadInterval = 5 * time.Millisecond
	})
	enforcer, _ := p.Enforcer()

	writePolicy(t, path, "p, bob, reports:read\n")
	waitFor(t, time.Second, func() bool {
		allowed, err := enforcer.Enforce("bob", "reports:read")
		return err == nil && allowed
	}, "periodic reload did not publish changed policy")

	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	host.close()
	writePolicy(t, path, "p, carol, reports:read\n")
	time.Sleep(25 * time.Millisecond)
	allowed, err := enforcer.Enforce("bob", "reports:read")
	assertEnforce(t, allowed, err, true)
	allowed, err = enforcer.Enforce("carol", "reports:read")
	assertEnforce(t, allowed, err, false)
}

func TestConcurrentEnforceAndProgrammaticReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.csv")
	writePolicy(t, path, "p, alice, reports:read\n")
	p, _ := initializedPlugin(t, func(cfg *Config) { cfg.PolicyFile = path })
	enforcer, _ := p.Enforcer()

	const readers = 12
	const iterations = 80
	start := make(chan struct{})
	errCh := make(chan error, readers+1)
	var wg sync.WaitGroup
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				if _, err := enforcer.Enforce("alice", "reports:read"); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := range iterations {
			subject := "alice"
			if i%2 == 1 {
				subject = "bob"
			}
			if err := replacePolicy(path, "p, "+subject+", reports:read\n"); err != nil {
				errCh <- err
				return
			}
			if err := p.LoadPolicy(); err != nil {
				errCh <- err
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
}

func TestStopHonorsCancelledContextWhenReloadTaskCannotExit(t *testing.T) {
	// A normal managed task always exits on cancellation. This test covers the
	// already-cancelled deadline branch directly without manufacturing a leaked
	// goroutine: an unclosed done channel represents a host that has not yet
	// delivered task cancellation.
	p, err := New(DefaultConfig())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	p.state = &runtimeState{
		cancel: func() {},
		done:   make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = p.Stop(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop(cancelled) error = %v, want context.Canceled", err)
	}
}

func writePolicy(t *testing.T, path, policy string) {
	t.Helper()
	if err := replacePolicy(path, policy); err != nil {
		t.Fatalf("write policy: %v", err)
	}
}

func replacePolicy(path, policy string) error {
	tmp := path + ".next"
	if err := os.WriteFile(tmp, []byte(policy), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func assertEnforce(t *testing.T, allowed bool, err error, want bool) {
	t.Helper()
	if err != nil || allowed != want {
		t.Fatalf("Enforce() = (%v, %v), want (%v, nil)", allowed, err, want)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatal(message)
	}
}
