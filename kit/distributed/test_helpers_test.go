package distributed

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// testRecord is the local value type the tests decode into.
type testRecord struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// writeRecord has the same JSON shape as testRecord but is a different Go
// type. It is what a writer uses to prove that Set accepts a source type that
// differs from the map's V and that Decode, not the writer's type, decides the
// local value.
type writeRecord struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// recordDecoder decodes raw JSON into testRecord with the caller's own tags.
func recordDecoder(_ string, raw json.RawMessage) (testRecord, error) {
	var record testRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return testRecord{}, err
	}
	return record, nil
}

// newTestClient returns a client to server whose failures surface quickly: a
// miniredis that has been closed must make a Redis call fail rather than wait
// on the client's default retry and timeout budget.
func newTestClient(t *testing.T, server *miniredis.Miniredis) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:         server.Addr(),
		DialTimeout:  500 * time.Millisecond,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		MaxRetries:   -1,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// newTestMap opens one map over server and registers its cleanup. NewMap
// performs startup synchronously, so a returned map is ready to read.
func newTestMap(t *testing.T, server *miniredis.Miniredis, name string, options MapOptions[testRecord]) *Map[testRecord] {
	t.Helper()
	created, err := NewMap(context.Background(), newTestClient(t, server), name, options)
	if err != nil {
		t.Fatalf("NewMap(%q): %v", name, err)
	}
	t.Cleanup(func() { _ = created.Close() })
	return created
}

// waitFor polls condition until it holds or the budget expires.
func waitFor(t *testing.T, budget time.Duration, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatal(message)
	}
}

// eventuallyLoaded waits until the map holds the expected string field of a
// decoded record value.
func eventuallyLoaded(t *testing.T, target *Map[testRecord], key, name string) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		value, ok := target.Load(key)
		return ok && value.Name == name
	}, "map did not load key "+key)
}
