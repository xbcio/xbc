package federation

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// testProvider is a stand-in for a protocol module's configuration type.
type testProvider struct {
	Endpoint string
	Enabled  bool
}

// testProviderValidator is the static validation a protocol module supplies.
func testProviderValidator(_ string, value testProvider) error {
	if value.Endpoint == "" {
		return errors.New("endpoint is required")
	}
	if !value.Enabled {
		return errors.New("provider is disabled")
	}
	return nil
}

func TestRegistryPutGetListRemove(t *testing.T) {
	registry := NewRegistry(testProviderValidator)
	for name, value := range map[string]testProvider{
		"zeta":  {Endpoint: "https://zeta.test", Enabled: true},
		"alpha": {Endpoint: "https://alpha.test", Enabled: true},
		"mike":  {Endpoint: "https://mike.test", Enabled: true},
	} {
		if err := registry.Put(name, value); err != nil {
			t.Fatalf("Put(%q) error = %v", name, err)
		}
	}

	value, ok := registry.Get("alpha")
	if !ok || value.Endpoint != "https://alpha.test" {
		t.Fatalf("Get(alpha) = (%+v, %v)", value, ok)
	}
	if _, ok := registry.Get("missing"); ok {
		t.Fatal("Get(missing) ok = true, want false")
	}

	entries := registry.List()
	if len(entries) != 3 {
		t.Fatalf("List() returned %d entries, want 3", len(entries))
	}
	for index, want := range []string{"alpha", "mike", "zeta"} {
		if entries[index].Name != want {
			t.Fatalf("List()[%d].Name = %q, want %q", index, entries[index].Name, want)
		}
	}

	if !registry.Remove("mike") {
		t.Fatal("Remove(mike) = false, want true")
	}
	if registry.Remove("mike") {
		t.Fatal("Remove(mike) second call = true, want false")
	}
	if _, ok := registry.Get("mike"); ok {
		t.Fatal("Get(mike) ok = true after Remove, want false")
	}
	if len(registry.List()) != 2 {
		t.Fatalf("List() after Remove returned %d entries, want 2", len(registry.List()))
	}
}

func TestRegistryReplacesValueAtomically(t *testing.T) {
	registry := NewRegistry(testProviderValidator)
	if err := registry.Put("corp", testProvider{Endpoint: "https://first.test", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Put("corp", testProvider{Endpoint: "https://second.test", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	value, ok := registry.Get("corp")
	if !ok || value.Endpoint != "https://second.test" {
		t.Fatalf("Get(corp) = (%+v, %v), want the replacement", value, ok)
	}
	if entries := registry.List(); len(entries) != 1 {
		t.Fatalf("List() returned %d entries, want 1", len(entries))
	}
}

func TestRegistryFailedPutKeepsPreviousValue(t *testing.T) {
	registry := NewRegistry(testProviderValidator)
	previous := testProvider{Endpoint: "https://good.test", Enabled: true}
	if err := registry.Put("corp", previous); err != nil {
		t.Fatal(err)
	}

	invalid := testProvider{Endpoint: "https://bad.test", Enabled: false}
	if err := registry.Put("corp", invalid); err == nil {
		t.Fatal("Put(invalid) error = nil, want a validation error")
	}
	value, ok := registry.Get("corp")
	if !ok || value != previous {
		t.Fatalf("Get(corp) = (%+v, %v), want the previous configuration", value, ok)
	}
}

func TestRegistryRejectsInvalidProviderNames(t *testing.T) {
	registry := NewRegistry(func(string, int) error { return nil })
	for _, name := range []string{
		"", "-leading", "_leading", "with space", "with/slash", "with.dot",
		"with\nnewline", "caf\u00e9", strings.Repeat("a", MaxProviderNameLength+1),
	} {
		if err := registry.Put(name, 1); !errors.Is(err, ErrInvalidProviderName) {
			t.Fatalf("Put(%q) error = %v, want ErrInvalidProviderName", name, err)
		}
		if _, ok := registry.Get(name); ok {
			t.Fatalf("Get(%q) ok = true after rejected Put, want false", name)
		}
	}
}

func TestRegistryAcceptsValidProviderNames(t *testing.T) {
	registry := NewRegistry(func(string, int) error { return nil })
	for _, name := range []string{
		"a", "A", "0", "corp-idp_2", "ABC-def_123", strings.Repeat("a", MaxProviderNameLength),
	} {
		if err := registry.Put(name, 1); err != nil {
			t.Fatalf("Put(%q) error = %v, want nil", name, err)
		}
	}
}

func TestRegistryRequiresValidator(t *testing.T) {
	if err := NewRegistry[int](nil).Put("corp", 1); !errors.Is(err, ErrRegistryValidator) {
		t.Fatalf("Put() with nil validator error = %v, want ErrRegistryValidator", err)
	}

	var zero Registry[int]
	if err := zero.Put("corp", 1); !errors.Is(err, ErrRegistryValidator) {
		t.Fatalf("zero Registry.Put() error = %v, want ErrRegistryValidator", err)
	}
	if _, ok := zero.Get("corp"); ok {
		t.Fatal("zero Registry.Get() ok = true, want false")
	}
	if entries := zero.List(); len(entries) != 0 {
		t.Fatalf("zero Registry.List() = %v, want no entries", entries)
	}
	if zero.Remove("corp") {
		t.Fatal("zero Registry.Remove() = true, want false")
	}
}

func TestRegistryNilReceiver(t *testing.T) {
	var registry *Registry[int]
	if err := registry.Put("corp", 1); !errors.Is(err, ErrNilRegistry) {
		t.Fatalf("nil Registry.Put() error = %v, want ErrNilRegistry", err)
	}
	if _, ok := registry.Get("corp"); ok {
		t.Fatal("nil Registry.Get() ok = true, want false")
	}
	if entries := registry.List(); entries != nil {
		t.Fatalf("nil Registry.List() = %v, want nil", entries)
	}
	if registry.Remove("corp") {
		t.Fatal("nil Registry.Remove() = true, want false")
	}
}

func TestRegistryListReturnsCopy(t *testing.T) {
	registry := NewRegistry(func(string, int) error { return nil })
	if err := registry.Put("corp", 1); err != nil {
		t.Fatal(err)
	}
	entries := registry.List()
	entries[0].Name = "mutated"
	entries[0].Value = 99
	entries = append(entries, Entry[int]{Name: "extra", Value: 2})

	again := registry.List()
	if len(again) != 1 || again[0].Name != "corp" || again[0].Value != 1 {
		t.Fatalf("List() after mutating a returned slice = %v", again)
	}
}

func TestRegistryValidatorSeesNameAndValue(t *testing.T) {
	var gotName string
	var gotValue int
	registry := NewRegistry(func(name string, value int) error {
		gotName, gotValue = name, value
		return nil
	})
	if err := registry.Put("corp", 7); err != nil {
		t.Fatal(err)
	}
	if gotName != "corp" || gotValue != 7 {
		t.Fatalf("validator saw (%q, %d), want (corp, 7)", gotName, gotValue)
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	registry := NewRegistry(func(_ string, value int) error {
		if value < 0 {
			return errors.New("negative value")
		}
		return nil
	})
	const workers = 8
	const iterations = 200

	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				name := fmt.Sprintf("provider-%d", (worker+iteration)%4)
				switch iteration % 4 {
				case 0, 1:
					_ = registry.Put(name, iteration)
				case 2:
					registry.Get(name)
				case 3:
					if iteration%8 == 3 {
						registry.Remove(name)
					} else {
						registry.List()
					}
				}
			}
		}(worker)
	}
	wait.Wait()

	if err := registry.Put("final", 1); err != nil {
		t.Fatalf("Put(final) error = %v", err)
	}
	value, ok := registry.Get("final")
	if !ok || value != 1 {
		t.Fatalf("Get(final) = (%d, %v), want (1, true)", value, ok)
	}
	if entries := registry.List(); len(entries) == 0 {
		t.Fatal("List() is empty after concurrent access")
	}
}
