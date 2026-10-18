package oauth2

import (
	"sync"
	"testing"
)

func TestRegistryStoresListsAndRemovesProviders(t *testing.T) {
	idp := newFakeIDP(t)
	names := []string{"alpha", "beta"}
	providers := make(map[string]*Provider, len(names))
	for _, name := range names {
		config := idp.providerConfig()
		config.Name = name
		provider, err := New(config)
		requireNoError(t, err, "New "+name)
		providers[name] = provider
	}

	registry := NewRegistry()
	for _, name := range names {
		requireNoError(t, registry.Put(name, providers[name]), "Put "+name)
	}

	stored, ok := registry.Get("beta")
	if !ok || stored != providers["beta"] {
		t.Fatal("Get did not return the registered provider")
	}
	if _, ok := registry.Get("unknown"); ok {
		t.Fatal("Get returned a provider for an unknown name")
	}

	entries := registry.List()
	if len(entries) != len(names) {
		t.Fatalf("List returned %d entries, want %d", len(entries), len(names))
	}
	for index, entry := range entries {
		if entry.Name != names[index] {
			t.Fatalf("List[%d].Name = %q, want %q", index, entry.Name, names[index])
		}
	}

	if !registry.Remove("alpha") {
		t.Fatal("Remove reported an absent provider")
	}
	if registry.Remove("alpha") {
		t.Fatal("Remove reported a provider twice")
	}
	if _, ok := registry.Get("alpha"); ok {
		t.Fatal("Get returned a removed provider")
	}
}

func TestRegistryRejectsAnInvalidPutAndKeepsThePreviousProvider(t *testing.T) {
	idp := newFakeIDP(t)
	provider := idp.newProvider(t)
	registry := NewRegistry()
	requireNoError(t, registry.Put(testProvider, provider), "initial Put")

	// A provider whose own name does not match the registry key would record
	// the wrong provider in every ExternalIdentity it produced.
	other := idp.providerConfig()
	other.Name = "other"
	otherProvider, err := New(other)
	requireNoError(t, err, "New other provider")
	requireErrorIs(t, registry.Put(testProvider, otherProvider), ErrInvalidConfig, "mismatched provider name")

	stored, ok := registry.Get(testProvider)
	if !ok || stored != provider {
		t.Fatal("a rejected Put replaced the registered provider")
	}
	identity, err := idp.exchange(t, stored)
	requireNoError(t, err, "the previous provider still works")
	if identity.Provider != testProvider {
		t.Fatalf("Provider = %q, want %q", identity.Provider, testProvider)
	}
}

func TestRegistryRejectsNilAndUnconstructedProviders(t *testing.T) {
	registry := NewRegistry()
	requireErrorIs(t, registry.Put(testProvider, nil), ErrInvalidConfig, "nil provider")

	var zero Provider
	requireErrorIs(t, registry.Put(testProvider, &zero), ErrInvalidConfig, "zero provider")
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
	requireNoError(t, ValidateProvider(testProvider, provider), "matching name")
	requireErrorIs(t, ValidateProvider("other", provider), ErrInvalidConfig, "mismatched name")
	requireErrorIs(t, ValidateProvider(testProvider, nil), ErrInvalidConfig, "nil provider")
}
