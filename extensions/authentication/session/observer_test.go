package session

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

type observerFunc func(context.Context, Event)

func (f observerFunc) Observe(ctx context.Context, event Event) { f(ctx, event) }

func testManagerWithObservers(t *testing.T, store Store, now time.Time, observers ...Observer) *manager {
	t.Helper()
	m := testManager(t, store, now)
	m.observers = observers
	return m
}

// Every operation reports its kind, both observers are notified in
// registration order for each event, and the per-kind fields are populated.
func TestManagerNotifiesObserversOfEveryKindInOrder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	var first, second []Event
	var interleaved []string
	m := testManagerWithObservers(t, store, now,
		observerFunc(func(_ context.Context, event Event) {
			first = append(first, event)
			interleaved = append(interleaved, "first:"+event.Kind.String())
		}),
		observerFunc(func(_ context.Context, event Event) {
			second = append(second, event)
			interleaved = append(interleaved, "second:"+event.Kind.String())
		}),
	)
	ctx := context.Background()

	created, err := m.Create(ctx, "alice", map[string]any{"role": "admin"})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := m.Rotate(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.UpdateAttributes(ctx, rotated.ID, map[string]any{"role": "reader"})
	if err != nil {
		t.Fatal(err)
	}
	patched, err := m.PatchAttributes(ctx, updated.ID, map[string]any{"role": "reader", "extra": true}, []string{"nested"})
	if err != nil {
		t.Fatal(err)
	}
	if _, verdict, err := m.Verify(ctx, patched.ID); err != nil || verdict != VerdictValid {
		t.Fatalf("Verify() verdict=%v err=%v", verdict, err)
	}
	if _, verdict, err := m.Verify(ctx, testID(100, 32)); err != nil || verdict != VerdictNotFound {
		t.Fatalf("Verify(unknown) verdict=%v err=%v", verdict, err)
	}
	if err := m.Revoke(ctx, patched.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, "bob", nil); err != nil {
		t.Fatal(err)
	}
	if revoked, err := m.RevokeBySubject(ctx, "bob"); err != nil || revoked != 1 {
		t.Fatalf("RevokeBySubject() = %d, %v", revoked, err)
	}
	carol, err := m.Create(ctx, "carol", nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted, err := m.RevokeByDigest(ctx, "carol", IDDigest(carol.ID)); err != nil || !deleted {
		t.Fatalf("RevokeByDigest() = %v, %v", deleted, err)
	}

	wantKinds := []EventKind{
		EventCreated, EventRotated, EventAttributesUpdated, EventAttributesPatched,
		EventVerified, EventRejected, EventRevoked, EventCreated, EventRevokedBySubject,
		EventCreated, EventRevokedByDigest,
	}
	if len(first) != len(wantKinds) {
		t.Fatalf("first observer saw %d events, want %d: %v", len(first), len(wantKinds), eventKinds(first))
	}
	for i, want := range wantKinds {
		if first[i].Kind != want {
			t.Fatalf("event %d = %v, want %v (sequence %v)", i, first[i].Kind, want, eventKinds(first))
		}
		if !reflect.DeepEqual(second[i], first[i]) {
			t.Fatalf("second observer event %d = %#v, want %#v", i, second[i], first[i])
		}
	}
	for i := range wantKinds {
		if got, want := interleaved[i*2], "first:"+wantKinds[i].String(); got != want {
			t.Fatalf("interleaved[%d] = %q, want %q", i*2, got, want)
		}
		if got, want := interleaved[i*2+1], "second:"+wantKinds[i].String(); got != want {
			t.Fatalf("interleaved[%d] = %q, want %q", i*2+1, got, want)
		}
	}

	if first[0].Subject != "alice" || first[0].Digest != IDDigest(created.ID) || first[0].Verdict != VerdictValid {
		t.Fatalf("created event = %#v", first[0])
	}
	if first[1].Digest != IDDigest(rotated.ID) || first[1].Subject != "alice" {
		t.Fatalf("rotated event = %#v", first[1])
	}
	if first[2].Subject != "alice" || first[2].Digest != IDDigest(updated.ID) {
		t.Fatalf("attributes event = %#v", first[2])
	}
	if first[3].Subject != "alice" || first[3].Digest != IDDigest(patched.ID) || first[3].Count != 3 {
		t.Fatalf("patch event = %#v, want the patched digest and Count 3", first[3])
	}
	if first[4].Verdict != VerdictValid || first[5].Verdict != VerdictNotFound {
		t.Fatalf("verification events = %#v, %#v", first[4], first[5])
	}
	if first[6].Digest != IDDigest(patched.ID) {
		t.Fatalf("revoked event = %#v", first[6])
	}
	if first[8].Subject != "bob" || first[8].Count != 1 {
		t.Fatalf("revoked-by-subject event = %#v", first[8])
	}
	if first[10].Subject != "carol" || first[10].Digest != IDDigest(carol.ID) || first[10].Count != 1 {
		t.Fatalf("revoked-by-digest event = %#v", first[10])
	}
}

func eventKinds(events []Event) []EventKind {
	kinds := make([]EventKind, len(events))
	for i, event := range events {
		kinds[i] = event.Kind
	}
	return kinds
}

// The Event shape is pinned and its rendering never contains a session ID, an
// attribute value, or an error message carrying either -- including the events
// the patch and expiry paths emit.
func TestObserverEventsNeverCarrySessionIDsAttributesOrCredentials(t *testing.T) {
	const (
		secret        = "attribute-value-that-must-never-leak"
		secretUpdated = secret + "-updated"
		secretPatched = secret + "-patched"
		secretExpired = secret + "-expired"
	)
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	var events []Event
	m := testManagerWithObservers(t, store, now, observerFunc(func(_ context.Context, event Event) {
		events = append(events, event)
	}))
	ctx := context.Background()

	created, err := m.Create(ctx, "alice", map[string]any{"token": secret})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := m.Rotate(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := m.UpdateAttributes(ctx, rotated.ID, map[string]any{"token": secretUpdated})
	if err != nil {
		t.Fatal(err)
	}
	patched, err := m.PatchAttributes(ctx, updated.ID, map[string]any{"token": secretPatched, "extra": secretPatched}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, verdict, err := m.Verify(ctx, patched.ID); err != nil || verdict != VerdictValid {
		t.Fatalf("Verify() verdict=%v err=%v", verdict, err)
	}
	if _, verdict, err := m.Verify(ctx, "not-an-opaque-id"); err != nil || verdict != VerdictMalformed {
		t.Fatalf("Verify(malformed) verdict=%v err=%v", verdict, err)
	}
	if err := m.Revoke(ctx, patched.ID); err != nil {
		t.Fatal(err)
	}
	expiring, err := m.Create(ctx, "dave", map[string]any{"token": secretExpired})
	if err != nil {
		t.Fatal(err)
	}
	store.setObservers(m.observers)
	store.cleanup(now.Add(25 * time.Hour))

	wantFields := []string{"Kind", "Subject", "Digest", "Verdict", "Count", "Err"}
	typ := reflect.TypeOf(Event{})
	if typ.NumField() != len(wantFields) {
		t.Fatalf("Event has %d fields, want exactly %v", typ.NumField(), wantFields)
	}
	for i, want := range wantFields {
		if got := typ.Field(i).Name; got != want {
			t.Fatalf("Event field %d = %q, want %q", i, got, want)
		}
	}

	forbidden := []string{
		created.ID, rotated.ID, patched.ID, expiring.ID,
		secret, secretUpdated, secretPatched, secretExpired,
	}
	for i, event := range events {
		rendered := fmt.Sprintf("%v|%+v|%#v|%s", event, event, event, event.Kind.String())
		for _, value := range forbidden {
			if strings.Contains(rendered, value) {
				t.Fatalf("event %d leaked %q: %s", i, value, rendered)
			}
		}
		if event.Digest == created.ID || event.Digest == rotated.ID || event.Digest == patched.ID {
			t.Fatalf("event %d Digest is the session ID itself: %#v", i, event)
		}
	}
	if events[0].Digest != IDDigest(created.ID) || events[1].Digest != IDDigest(rotated.ID) {
		t.Fatalf("digests = %q, %q; want the documented IDDigest handles", events[0].Digest, events[1].Digest)
	}
	if events[5].Digest != IDDigest("not-an-opaque-id") || events[5].Subject != "" {
		t.Fatalf("malformed rejection event = %#v", events[5])
	}
	last := events[len(events)-1]
	if last.Kind != EventExpired || last.Digest != IDDigest(expiring.ID) || last.Subject != "dave" || last.Count != 1 {
		t.Fatalf("expiry event = %#v, want dave's swept digest with Count 1", last)
	}
}

// A panicking observer neither fails the operation nor stops the observers
// registered after it.
func TestObserverPanicIsContainedAndLaterObserversStillRun(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := newControlledMemoryStore(t, &now)
	var panics, delivered int
	var mu sync.Mutex
	m := testManagerWithObservers(t, store, now,
		observerFunc(func(context.Context, Event) {
			mu.Lock()
			panics++
			mu.Unlock()
			panic("observer bug")
		}),
		observerFunc(func(context.Context, Event) {
			mu.Lock()
			delivered++
			mu.Unlock()
		}),
	)
	ctx := context.Background()

	created, err := m.Create(ctx, "alice", nil)
	if err != nil {
		t.Fatalf("Create() inside a panicking observer = %v", err)
	}
	if _, verdict, err := m.Verify(ctx, created.ID); err != nil || verdict != VerdictValid {
		t.Fatalf("Verify() verdict=%v err=%v", verdict, err)
	}
	if err := m.Revoke(ctx, created.ID); err != nil {
		t.Fatalf("Revoke() inside a panicking observer = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if panics != 3 || delivered != 3 {
		t.Fatalf("panics=%d delivered=%d, want 3 and 3", panics, delivered)
	}
}

// WithObserver accepts several observers, ignores nil, and notifies in the
// order supplied.
func TestNewNotifiesObserversInTheOrderSupplied(t *testing.T) {
	var order []string
	p, err := New(DefaultConfig(),
		WithObserver(nil),
		WithObserver(observerFunc(func(context.Context, Event) { order = append(order, "first") })),
		WithObserver(observerFunc(func(context.Context, Event) { order = append(order, "second") })),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	if _, err := p.Manager().Create(context.Background(), "alice", nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("notification order = %v", order)
	}
}

type observerFixture struct {
	observer Observer
}

func (f *observerFixture) Observers() []Observer { return []Observer{f.observer} }

type panickingContributor struct{}

func (panickingContributor) Observers() []Observer { panic("contributor bug") }

// An ObserverContributor exported by another Definition supplies its observers
// through the plugin graph.
func TestObserverContributorObserversReachTheSessionManager(t *testing.T) {
	var mu sync.Mutex
	var kinds []EventKind
	fixture := plugin.Define(
		"session-observer-fixture",
		func(plugin.BuildContext) (*observerFixture, error) {
			return &observerFixture{observer: observerFunc(func(_ context.Context, event Event) {
				mu.Lock()
				defer mu.Unlock()
				kinds = append(kinds, event.Kind)
			})}, nil
		},
		plugin.Options[*observerFixture]{
			Exports: plugin.Contracts(
				plugin.ExportAs[ObserverContributor](func(value *observerFixture) ObserverContributor { return value }),
			),
		},
	)
	environment := sessionEnvironment(t, nil, map[string]any{})
	built, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(fixture), Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(built, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, found := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !found {
		t.Fatal("session instance not constructed")
	}
	p := instance.Primary().(*Plugin)
	t.Cleanup(func() { _ = p.Stop(context.Background()) })

	ctx := context.Background()
	created, err := p.Manager().Create(ctx, "alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Manager().Rotate(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(kinds, []EventKind{EventCreated, EventRotated}) {
		t.Fatalf("contributed observer saw %v, want created then rotated", kinds)
	}
}

// collectObservers is best-effort: a nil contributor, a nil observer, and a
// panicking contributor are skipped, and the remaining observers are retained.
func TestCollectObserversSkipsNilAndPanickingContributors(t *testing.T) {
	var delivered int
	good := &observerFixture{observer: observerFunc(func(context.Context, Event) { delivered++ })}
	observers := collectObservers([]plugin.Entry[ObserverContributor]{
		{Identity: plugin.Identity{Plugin: "nil-contributor"}, Value: (*observerFixture)(nil)},
		{Identity: plugin.Identity{Plugin: "panicking"}, Value: panickingContributor{}},
		{Identity: plugin.Identity{Plugin: "nil-observer"}, Value: &observerFixture{}},
		{Identity: plugin.Identity{Plugin: "good"}, Value: good},
	})
	if len(observers) != 1 {
		t.Fatalf("observers = %d, want only the valid one", len(observers))
	}
	observers[0].Observe(context.Background(), Event{Kind: EventCreated})
	if delivered != 1 {
		t.Fatalf("delivered = %d", delivered)
	}
}
