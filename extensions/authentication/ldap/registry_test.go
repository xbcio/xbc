package ldap

import (
	"errors"
	"sync"
	"testing"
)

func TestRegistryStoresListsAndRemovesProviders(t *testing.T) {
	names := []string{"alpha", "beta"}
	providers := make(map[string]*Provider, len(names))
	for _, name := range names {
		config := testConfig("ldaps://directory.example.com")
		config.Name = name
		providers[name] = newProvider(t, config)
	}

	registry := NewRegistry()
	for _, name := range names {
		if err := registry.Put(name, providers[name]); err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
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
	provider := newProvider(t, testConfig("ldaps://directory.example.com"))
	registry := NewRegistry()
	if err := registry.Put(testProviderName, provider); err != nil {
		t.Fatalf("initial Put: %v", err)
	}

	otherConfig := testConfig("ldaps://directory.example.com")
	otherConfig.Name = "other"
	otherProvider := newProvider(t, otherConfig)
	if err := registry.Put(testProviderName, otherProvider); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Put with a mismatched name = %v, want ErrInvalidConfig", err)
	}

	stored, ok := registry.Get(testProviderName)
	if !ok || stored != provider {
		t.Fatal("a rejected Put replaced the registered provider")
	}
}

func TestRegistryRejectsNilAndUnconstructedProviders(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Put(testProviderName, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Put(nil) = %v, want ErrInvalidConfig", err)
	}
	var zero Provider
	if err := registry.Put(testProviderName, &zero); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Put(zero provider) = %v, want ErrInvalidConfig", err)
	}
	if _, ok := registry.Get(testProviderName); ok {
		t.Fatal("an invalid provider was stored")
	}
}

func TestRegistryIsConcurrencySafe(t *testing.T) {
	registry := NewRegistry()
	providers := make([]*Provider, 4)
	for index := range providers {
		providers[index] = newProvider(t, testConfig("ldaps://directory.example.com"))
	}
	names := []string{"alpha", "beta", "gamma", "delta"}

	var waitGroup sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		waitGroup.Add(1)
		go func(worker int) {
			defer waitGroup.Done()
			for round := 0; round < 200; round++ {
				name := names[(worker+round)%len(names)]
				_ = registry.Put(name, providers[(worker+round)%len(providers)])
				_, _ = registry.Get(name)
				_ = registry.List()
				if round%7 == 0 {
					registry.Remove(name)
				}
			}
		}(worker)
	}
	waitGroup.Wait()

	// A concurrent workload must leave the registry usable and consistent.
	finalConfig := testConfig("ldaps://directory.example.com")
	finalConfig.Name = "final"
	final := newProvider(t, finalConfig)
	if err := registry.Put("final", final); err != nil {
		t.Fatalf("Put after concurrent workload: %v", err)
	}
	if _, ok := registry.Get("final"); !ok {
		t.Fatal("the registry lost a provider stored after the concurrent workload")
	}
}
