package oidc

import (
	"sync"
	"testing"
)

func TestRegistryRejectsAnInvalidPutAndKeepsThePreviousProvider(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	registry := NewRegistry()
	requireNoError(t, registry.Put("test", provider), "initial Put")

	// A provider whose own name does not match the registry key would record
	// the wrong provider in every ExternalIdentity it produced.
	other := idp.providerConfig()
	other.Name = "other"
	otherProvider, err := New(other)
	requireNoError(t, err, "New other provider")
	requireErrorIs(t, registry.Put("test", otherProvider), ErrInvalidConfig, "mismatched provider name")

	stored, ok := registry.Get("test")
	if !ok || stored != provider {
		t.Fatal("a rejected Put replaced the registered provider")
	}
	identity, err := idp.exchange(t, stored)
	requireNoError(t, err, "the previous provider still works")
	if identity.Provider != "test" {
		t.Fatalf("Provider = %q, want test", identity.Provider)
	}
}

func TestRegistryRejectsNilAndUnconstructedProviders(t *testing.T) {
	registry := NewRegistry()
	requireErrorIs(t, registry.Put("test", nil), ErrInvalidConfig, "nil provider")

	var zero Provider
	requireErrorIs(t, registry.Put("test", &zero), ErrInvalidConfig, "zero provider")
}

func TestRegistryConcurrentPutGetRemove(t *testing.T) {
	idp := newFakeIDP(t)
	names := []string{"alpha", "beta", "gamma"}
	providers := make(map[string]*Provider, len(names))
	for _, name := range names {
		config := idp.providerConfig()
		config.Name = name
		provider, err := New(config)
		requireNoError(t, err, "New "+name)
		providers[name] = provider
	}

	registry := NewRegistry()
	const goroutines = 16
	var wait sync.WaitGroup
	wait.Add(goroutines)
	for index := 0; index < goroutines; index++ {
		go func(worker int) {
			defer wait.Done()
			name := names[worker%len(names)]
			for iteration := 0; iteration < 50; iteration++ {
				_ = registry.Put(name, providers[name])
				_, _ = registry.Get(name)
				_ = registry.List()
				if iteration%7 == 0 {
					registry.Remove(name)
				}
			}
		}(index)
	}
	wait.Wait()
}

func TestValidateProviderRejectsAMismatchedName(t *testing.T) {
	idp := newFakeIDP(t)
	provider, err := New(idp.providerConfig())
	requireNoError(t, err, "New")
	requireNoError(t, ValidateProvider("test", provider), "matching name")
	requireErrorIs(t, ValidateProvider("other", provider), ErrInvalidConfig, "mismatched name")
}
