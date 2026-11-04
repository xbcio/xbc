package distributed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// TestNewMapAppliesDefaults pins the zero-option behaviour: the default
// prefix, no persistence, an unlimited cache, the JSON decoder, and non-nil
// logger.
func TestNewMapAppliesDefaults(t *testing.T) {
	server := miniredis.RunT(t)
	created, err := newMap(newTestClient(t, server), "rules", MapOptions[testRecord]{})
	if err != nil {
		t.Fatalf("newMap: %v", err)
	}

	if created.hashKey != DefaultPrefix+":map:rules" {
		t.Fatalf("hash key = %q, want the default-prefix key", created.hashKey)
	}
	if created.channel != created.hashKey+":events" {
		t.Fatalf("channel = %q, want %q", created.channel, created.hashKey+":events")
	}
	if created.persist != PersistNone {
		t.Fatalf("persist = %q, want none", created.persist)
	}
	if created.store.maxSize != 0 {
		t.Fatalf("max size = %d, want unlimited", created.store.maxSize)
	}
	if created.decode == nil {
		t.Fatal("Decode defaulted to nil")
	}
	if created.log == nil {
		t.Fatal("Logger defaulted to nil")
	}
	if err := created.Close(); err != nil {
		t.Fatalf("Close before start: %v", err)
	}
}

func TestNewMapRejectsInvalidConfiguration(t *testing.T) {
	server := miniredis.RunT(t)
	client := newTestClient(t, server)

	for name, testCase := range map[string]struct {
		options MapOptions[testRecord]
		want    error
	}{
		"empty name":                    {options: MapOptions[testRecord]{}, want: ErrInvalidName},
		"name with a space":             {options: MapOptions[testRecord]{}, want: ErrInvalidName},
		"malformed prefix":              {options: MapOptions[testRecord]{Prefix: "Has Space"}, want: ErrInvalidOption},
		"ttl persistence without a ttl": {options: MapOptions[testRecord]{Persist: PersistTTL}, want: ErrInvalidOption},
		"unknown persistence mode":      {options: MapOptions[testRecord]{Persist: "forever"}, want: ErrInvalidOption},
		"negative max size":             {options: MapOptions[testRecord]{MaxSize: -1}, want: ErrInvalidOption},
		"negative ttl in ttl mode":      {options: MapOptions[testRecord]{Persist: PersistTTL, TTL: -time.Second}, want: ErrInvalidOption},
	} {
		name := name
		testCase := testCase
		t.Run(name, func(t *testing.T) {
			mapName := "rules"
			if name == "empty name" {
				mapName = ""
			}
			if name == "name with a space" {
				mapName = "has space"
			}
			created, err := NewMap(context.Background(), client, mapName, testCase.options)
			if !errors.Is(err, testCase.want) {
				t.Fatalf("NewMap(%q, %+v) = %v, want %v", mapName, testCase.options, err, testCase.want)
			}
			if created != nil {
				t.Fatalf("NewMap returned a map alongside %v", err)
			}
		})
	}
}

func TestNewMapRequiresAClient(t *testing.T) {
	if _, err := NewMap(context.Background(), nil, "rules", MapOptions[testRecord]{}); err == nil {
		t.Fatal("NewMap accepted a nil client")
	}
}

// TestDefaultPrefixNamesTheRedisKeys proves the documented key shape against
// the store rather than against struct fields.
func TestDefaultPrefixNamesTheRedisKeys(t *testing.T) {
	server := miniredis.RunT(t)
	created := newTestMap(t, server, "rules", persistentOptions())

	if err := created.Set(context.Background(), "one", testRecord{Name: "one"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	keys, err := server.HKeys("distributed:map:rules")
	if err != nil {
		t.Fatalf("HKeys: %v", err)
	}
	if len(keys) != 1 || keys[0] != "one" {
		t.Fatalf("default-prefix key holds %v, want [one]", keys)
	}
}

func TestCustomPrefixNamesTheRedisKeys(t *testing.T) {
	server := miniredis.RunT(t)
	created := newTestMap(t, server, "rules", MapOptions[testRecord]{
		Prefix:  "tenant-a",
		Persist: PersistTTL,
		TTL:     time.Hour,
	})

	if err := created.Set(context.Background(), "one", testRecord{Name: "one"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	keys, err := server.HKeys("tenant-a:map:rules")
	if err != nil {
		t.Fatalf("HKeys: %v", err)
	}
	if len(keys) != 1 || keys[0] != "one" {
		t.Fatalf("custom-prefix key holds %v, want [one]", keys)
	}
	if created.channel != "tenant-a:map:rules:events" {
		t.Fatalf("channel = %q, want the prefixed channel", created.channel)
	}
}
