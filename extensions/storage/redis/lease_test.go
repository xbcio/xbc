package redis

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/extensions/coordination/lease"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

func newLockerTest(t *testing.T) (*miniredis.Miniredis, *goredis.Client, lease.Locker) {
	t.Helper()
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	locker, err := NewLocker(client)
	if err != nil {
		t.Fatalf("NewLocker() error = %v", err)
	}
	return server, client, locker
}

func TestLeaseDefinitionIsDistinctFromTheOtherDefinitions(t *testing.T) {
	var zero plugin.Definition
	if leaseDefinition == zero {
		t.Fatal("leaseDefinition is a zero handle")
	}
	if leaseDefinition == definition {
		t.Fatal("the lease capability must be a distinct Definition from the client")
	}
	if leaseDefinition == healthDefinition {
		t.Fatal("the lease capability must be a distinct Definition from the readiness probe")
	}
	if LeaseKey == Key || LeaseKey == HealthKey {
		t.Fatalf("the lease Key %q must differ from the client Key %q and the probe Key %q", LeaseKey, Key, HealthKey)
	}
}

func TestNewLockerRejectsANilClient(t *testing.T) {
	locker, err := NewLocker(nil)
	if err == nil {
		t.Fatal("a nil client must fail construction rather than panic on the first acquisition")
	}
	if locker != nil {
		t.Fatalf("NewLocker(nil) = %v, want a nil locker", locker)
	}
}

func TestPrepareLeaseConfigDefaultsToTheUnnamedInstanceAndRejectsBadNames(t *testing.T) {
	prepared, err := prepareLeaseConfig(LeaseConfig{})
	if err != nil {
		t.Fatalf("prepareLeaseConfig(LeaseConfig{}) error = %v", err)
	}
	if prepared.Instance != plugin.DefaultInstance {
		t.Fatalf("default instance = %q, want %q", prepared.Instance, plugin.DefaultInstance)
	}

	if _, err := prepareLeaseConfig(LeaseConfig{Instance: "Cache"}); err == nil {
		t.Fatal("an invalid instance name must be rejected during preparation")
	} else if !strings.Contains(err.Error(), "instance name") {
		t.Fatalf("error = %v, want it to name the invalid identifier kind", err)
	}
}

// TestLockerContentionAndOwnerSafeRelease is the contract's load-bearing
// behaviour: a superseded lease must neither renew nor delete its successor's
// key. Losing that comparison would let a paused process delete the lock a
// healthy one is holding.
func TestLockerContentionAndOwnerSafeRelease(t *testing.T) {
	server, client, locker := newLockerTest(t)
	ctx := context.Background()
	first, acquired, err := locker.TryAcquire(ctx, "placement:sast:0", "scanner-1", time.Second)
	if err != nil || !acquired {
		t.Fatalf("first TryAcquire() = (%v, %v), error = %v", first, acquired, err)
	}
	if _, acquired, err := locker.TryAcquire(ctx, "placement:sast:0", "scanner-1", time.Second); err != nil || acquired {
		t.Fatalf("contending TryAcquire() acquired = %v, error = %v", acquired, err)
	}

	// Simulate expiry and takeover. The stale lease must neither renew nor
	// delete the successor's token.
	server.FastForward(2 * time.Second)
	second, acquired, err := locker.TryAcquire(ctx, "placement:sast:0", "scanner-1", time.Second)
	if err != nil || !acquired {
		t.Fatalf("takeover TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	if first.Owner() == second.Owner() {
		t.Fatal("two acquisitions reused an owner token")
	}
	if owned, err := first.Renew(ctx, 3*time.Second); err != nil || owned {
		t.Fatalf("stale Renew() owned = %v, error = %v", owned, err)
	}
	if released, err := first.Release(ctx); err != nil || released {
		t.Fatalf("stale Release() released = %v, error = %v", released, err)
	}
	if got, err := client.Get(ctx, "placement:sast:0").Result(); err != nil || got != second.Owner() {
		t.Fatalf("successor value = %q, error = %v; stale owner modified it", got, err)
	}
	if released, err := second.Release(ctx); err != nil || !released {
		t.Fatalf("owner Release() released = %v, error = %v", released, err)
	}
	if server.Exists("placement:sast:0") {
		t.Fatal("lock still exists after owner release")
	}
}

// TestTheStoredValueNamesTheProcessHoldingTheKey is why TryAcquire takes a
// claimant at all. Without it the value under a held key is sixteen random
// bytes, so an operator who finds a slot occupied learns that someone holds it
// and has no way to find out who -- the one question worth asking when a slot
// is held by a process that should have given it up.
//
// The uniqueness half is checked from the same place because the two are a pair:
// the claimant is what makes the value readable, and the random half is what
// keeps it from being reusable. See lease.Locker for why a value that were only
// the claimant would let a restarted process's stale lease renew a successor's
// lock.
func TestTheStoredValueNamesTheProcessHoldingTheKey(t *testing.T) {
	_, client, locker := newLockerTest(t)
	ctx := context.Background()

	held, acquired, err := locker.TryAcquire(ctx, "placement:workload:sast:0", "scanner-2", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	stored, err := client.Get(ctx, "placement:workload:sast:0").Result()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored != held.Owner() {
		t.Fatalf("stored value = %q, want the lease's own token %q", stored, held.Owner())
	}
	if !strings.HasPrefix(stored, "scanner-2/") {
		t.Fatalf("stored value = %q, want it to name the claiming process", stored)
	}
	if strings.TrimPrefix(stored, "scanner-2/") == "" {
		t.Fatalf("stored value = %q, want a part unique to this acquisition after the claimant", stored)
	}

	// The same process, restarted, claims the same key under the same name. A
	// token that were only the claimant would be identical here.
	if released, err := held.Release(ctx); err != nil || !released {
		t.Fatalf("Release() released = %v, error = %v", released, err)
	}
	successor, acquired, err := locker.TryAcquire(ctx, "placement:workload:sast:0", "scanner-2", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("second TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	if successor.Owner() == held.Owner() {
		t.Fatalf("both acquisitions under claimant %q minted the token %q", "scanner-2", held.Owner())
	}
	if owned, err := held.Renew(ctx, time.Minute); err != nil || owned {
		t.Fatalf("the released lease renewed its successor's lock: owned = %v, error = %v", owned, err)
	}
}

// TestAnAnonymousClaimIsStillServed pins the empty claimant: a caller with no
// identity worth publishing -- cron's scheduler lock, where every replica would
// publish the same name -- gets an ordinary unique token and gives up only the
// ability to read the holder back out of the store.
func TestAnAnonymousClaimIsStillServed(t *testing.T) {
	_, client, locker := newLockerTest(t)
	ctx := context.Background()

	held, acquired, err := locker.TryAcquire(ctx, "locks:nightly", "", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	stored, err := client.Get(ctx, "locks:nightly").Result()
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored != held.Owner() {
		t.Fatalf("stored value = %q, want the lease's own token %q", stored, held.Owner())
	}
	if strings.Contains(stored, "/") {
		t.Fatalf("stored value = %q, want no claimant prefix when none was named", stored)
	}
}

func TestLockerRenewExtendsTTLAndExpiredLockIsTakenOver(t *testing.T) {
	server, _, locker := newLockerTest(t)
	ctx := context.Background()
	held, acquired, err := locker.TryAcquire(ctx, "placement:webscan:0", "scanner-1", 100*time.Millisecond)
	if err != nil || !acquired {
		t.Fatalf("TryAcquire() acquired = %v, error = %v", acquired, err)
	}
	server.FastForward(70 * time.Millisecond)
	if owned, err := held.Renew(ctx, 200*time.Millisecond); err != nil || !owned {
		t.Fatalf("Renew() owned = %v, error = %v", owned, err)
	}
	if ttl := server.TTL("placement:webscan:0"); ttl < 190*time.Millisecond {
		t.Fatalf("TTL after renewal = %s, want approximately 200ms", ttl)
	}

	server.FastForward(250 * time.Millisecond)
	if server.Exists("placement:webscan:0") {
		t.Fatal("lease did not expire after renewed TTL")
	}
	successor, acquired, err := locker.TryAcquire(ctx, "placement:webscan:0", "scanner-2", time.Second)
	if err != nil || !acquired || successor == nil {
		t.Fatalf("post-expiry takeover acquired = %v, lease = %v, error = %v", acquired, successor, err)
	}
}

// TestAssembledLeasePluginExportsALockerOverTheNamedInstance is the
// load-bearing test for this Definition's existence. The locker exists because
// *redis.Client, a third-party type, cannot implement lease.Locker: assembly
// rejects a contract its declaring Definition's primary type is not assignable
// to. This test proves the second-Definition shape actually reaches a consumer,
// and that it binds the instance the section named rather than an arbitrary one.
func TestAssembledLeasePluginExportsALockerOverTheNamedInstance(t *testing.T) {
	server := miniredis.RunT(t)
	client := goredis.NewClient(&goredis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"redis":       map[string]any{"cache": map[string]any{"addr": server.Addr()}},
			"redis-lease": map[string]any{"instance": "cache"},
		},
	}, "XBC_REDIS_LEASE_TEST_UNSET_")
	if err != nil {
		t.Fatalf("config.NewEnvironment: %v", err)
	}

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan: %v", err)
	}

	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{
		ContextFactory: func(identity plugin.Identity, _ log.Logger) *plugin.Context {
			return plugin.NewRuntimeContext(healthTestHost{}, identity)
		},
	})
	if err != nil {
		t.Fatalf("assembly.Construct: %v", err)
	}
	t.Cleanup(func() {
		if instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: "cache"}); ok {
			_ = instance.StopBounded(context.Background(), time.Second)
		}
	})

	instance, ok := constructed.Instance(plugin.Identity{Plugin: LeaseKey, Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("the configured lease Definition must be selected")
	}
	exported, ok := instance.Primary().(lease.Locker)
	if !ok {
		t.Fatalf("lease primary is %T, want a lease.Locker", instance.Primary())
	}

	held, acquired, err := exported.TryAcquire(context.Background(), "placement:sca:0", "scanner-1", time.Minute)
	if err != nil || !acquired || held == nil {
		t.Fatalf("TryAcquire() = (%v, %v), error = %v", held, acquired, err)
	}
	if held.Key() != "placement:sca:0" {
		t.Fatalf("lease key = %q, want the requested key", held.Key())
	}
	if !server.Exists("placement:sca:0") {
		t.Fatal("the exported locker did not write to the instance the section named")
	}
	if released, err := held.Release(context.Background()); err != nil || !released {
		t.Fatalf("Release() released = %v, error = %v", released, err)
	}
}

// TestUnconfiguredLeaseCapabilityIsInert pins the activation boundary: the
// second Definition must not make a lease.Locker appear in an application that
// never asked for one.
func TestUnconfiguredLeaseCapabilityIsInert(t *testing.T) {
	server := miniredis.RunT(t)
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"redis": map[string]any{"cache": map[string]any{"addr": server.Addr()}},
		},
	}, "XBC_REDIS_LEASE_TEST_UNSET_")
	if err != nil {
		t.Fatalf("config.NewEnvironment: %v", err)
	}

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("assembly.BuildPlan: %v", err)
	}
	for _, identity := range plan.Order() {
		if identity.Plugin == LeaseKey {
			t.Fatalf("plugins.redis-lease is unconfigured, but plan contains %s", identity)
		}
	}
}

// TestLeaseCapabilityWithoutItsInstanceFailsPlanning proves the section cannot
// silently borrow some other client: naming an instance that the composition
// did not configure is a planning error that names both sides.
func TestLeaseCapabilityWithoutItsInstanceFailsPlanning(t *testing.T) {
	server := miniredis.RunT(t)
	environment, err := config.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"redis":       map[string]any{"cache": map[string]any{"addr": server.Addr()}},
			"redis-lease": map[string]any{"instance": "missing"},
		},
	}, "XBC_REDIS_LEASE_TEST_UNSET_")
	if err != nil {
		t.Fatalf("config.NewEnvironment: %v", err)
	}

	_, err = assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err == nil {
		t.Fatal("naming an unconfigured Redis instance must fail planning")
	}
	if !strings.Contains(err.Error(), "redis[missing]") {
		t.Fatalf("error = %v, want it to name the exact missing producer", err)
	}
}
