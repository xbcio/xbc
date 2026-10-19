package saml

import (
	"errors"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication/federation"
)

func TestRegistryStoresReplacesAndRemovesProviders(t *testing.T) {
	registry := NewRegistry()
	first := newTestProvider(t, nil)
	if err := registry.Put("corp", first); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stored, ok := registry.Get("corp")
	if !ok || stored != first {
		t.Fatal("Get did not return the stored provider")
	}

	second := newTestProvider(t, func(config *Config) {
		config.Issuer = "https://idp2.example.com/entity"
	})
	if err := registry.Put("corp", second); err != nil {
		t.Fatalf("Put replacement: %v", err)
	}
	stored, ok = registry.Get("corp")
	if !ok || stored != second {
		t.Fatal("Put did not replace the provider atomically")
	}

	if !registry.Remove("corp") {
		t.Fatal("Remove reported no removal")
	}
	if _, ok := registry.Get("corp"); ok {
		t.Fatal("Get returned a removed provider")
	}
	if registry.Remove("corp") {
		t.Fatal("Remove reported removing an absent provider")
	}
}

func TestRegistryRejectsInvalidProviders(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Put("corp", nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Put(nil) = %v, want ErrInvalidConfig", err)
	}
	if err := registry.Put("corp", &Provider{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Put(uninitialized) = %v, want ErrInvalidConfig", err)
	}
	provider := newTestProvider(t, nil)
	if err := registry.Put("other", provider); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Put(name mismatch) = %v, want ErrInvalidConfig", err)
	}
	if err := registry.Put("", provider); !errors.Is(err, federation.ErrInvalidProviderName) {
		t.Fatalf("Put(empty name) = %v, want ErrInvalidProviderName", err)
	}
}

func TestRegistryFailedPutKeepsPreviousProvider(t *testing.T) {
	registry := NewRegistry()
	previous := newTestProvider(t, nil)
	if err := registry.Put("corp", previous); err != nil {
		t.Fatalf("Put: %v", err)
	}
	mismatched := newTestProvider(t, func(config *Config) {
		config.Name = "other"
	})
	if err := registry.Put("corp", mismatched); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Put(mismatched) = %v, want ErrInvalidConfig", err)
	}
	stored, ok := registry.Get("corp")
	if !ok || stored != previous {
		t.Fatal("a rejected Put replaced the previous provider")
	}
}

func TestValidateProviderAcceptsValidProvider(t *testing.T) {
	provider := newTestProvider(t, nil)
	if err := ValidateProvider("corp", provider); err != nil {
		t.Fatalf("ValidateProvider: %v", err)
	}
	if err := ValidateProvider("corp", nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("ValidateProvider(nil) = %v, want ErrInvalidConfig", err)
	}
	if err := ValidateProvider("corp", &Provider{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("ValidateProvider(uninitialized) = %v, want ErrInvalidConfig", err)
	}
}
