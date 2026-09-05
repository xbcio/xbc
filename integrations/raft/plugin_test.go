package raft

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	hashiraft "github.com/hashicorp/raft"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

func testEnvironment(t *testing.T, values map[string]any) *config.Environment {
	t.Helper()
	environment, err := config.NewEnvironment(values, "XBC_RAFT_TEST_UNSET_")
	if err != nil {
		t.Fatalf("config.NewEnvironment() error = %v", err)
	}
	return environment
}

func validRaftEnvironment(t *testing.T, id string) *config.Environment {
	t.Helper()
	return testEnvironment(t, map[string]any{
		"plugins": map[string]any{
			"raft": map[string]any{
				"node_id":   id,
				"bind_addr": "127.0.0.1:0",
				"storage":   "memory",
				"bootstrap": true,
			},
		},
	})
}

func TestDefinitionIsCanonicalAndBundleIsStable(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero {
		t.Fatal("Definition() returned a zero handle")
	}
	if Definition() != Definition() {
		t.Fatal("Definition() returned different handles")
	}
	first, second := Bundle(), Bundle()
	if reflect.DeepEqual(first, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty bundle")
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("Bundle() returned different composition content")
	}
}

func TestDefinitionIsDisabledUntilConfiguredAndExportsNodeAndPrimary(t *testing.T) {
	disabled, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     testEnvironment(t, nil),
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	if disabled.DefinitionCount() != 1 {
		t.Fatalf("DefinitionCount() = %d, want 1", disabled.DefinitionCount())
	}
	if got := disabled.Disabled(); !reflect.DeepEqual(got, []plugin.Key{Key}) {
		t.Fatalf("Disabled() = %#v, want [%s]", got, Key)
	}
	if len(disabled.Order()) != 0 {
		t.Fatalf("Order() = %#v, want empty until plugins.raft is configured", disabled.Order())
	}

	enabled, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     validRaftEnvironment(t, "solo"),
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	want := []plugin.Identity{{Plugin: Key, Instance: plugin.DefaultInstance}}
	if got := enabled.Order(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Order() = %#v, want %#v", got, want)
	}
	if got := enabled.Contracts(reflect.TypeOf((*node)(nil))); !reflect.DeepEqual(got, want) {
		t.Fatalf("Contracts(*node) = %#v, want %#v", got, want)
	}
	if got := enabled.Contracts(reflect.TypeOf((*Node)(nil)).Elem()); !reflect.DeepEqual(got, want) {
		t.Fatalf("Contracts(Node) = %#v, want %#v", got, want)
	}
}

func TestPrepareConfigDelegatesToValidate(t *testing.T) {
	valid := fastConfig("prepared", true)
	prepared, err := prepareConfig(valid)
	if err != nil {
		t.Fatalf("prepareConfig(valid) error = %v", err)
	}
	if prepared.NodeID != valid.NodeID {
		t.Fatalf("prepareConfig(valid) = %#v", prepared)
	}
	invalid := valid
	invalid.NodeID = ""
	if _, err := prepareConfig(invalid); err == nil {
		t.Fatal("prepareConfig(invalid) error = nil")
	}
}

func TestDefinitionConstructsNodeAndCustomFSMOverridesDefault(t *testing.T) {
	environment := validRaftEnvironment(t, "default-fsm")
	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, ok := constructed.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("constructed instance not found")
	}
	n, ok := instance.Primary().(*node)
	if !ok {
		t.Fatalf("Primary() = %T, want *node", instance.Primary())
	}
	if _, ok := n.FSM().(*KVFSM); !ok {
		t.Fatalf("default FSM = %T, want *KVFSM", n.FSM())
	}
	if !instance.HasStop() {
		t.Fatal("Definition must declare a Stop lifecycle stage")
	}
	if err := instance.StopBounded(context.Background(), time.Second); err != nil {
		t.Fatalf("StopBounded() error = %v", err)
	}
	if err := instance.StopBounded(context.Background(), time.Second); err != nil {
		t.Fatalf("repeated StopBounded() error = %v", err)
	}

	custom := newRecordingFSM()
	fsmDefinition := plugin.Define(
		"raft-test-fsm",
		func(plugin.BuildContext) (*recordingFSM, error) { return custom, nil },
		plugin.Options[*recordingFSM]{
			Exports: plugin.Contracts(
				plugin.ExportAs(func(value *recordingFSM) FSM { return value }),
			),
		},
	)
	withCustom, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{plugin.BundleOf(fsmDefinition), Bundle()},
		Env:     validRaftEnvironment(t, "custom-fsm"),
	})
	if err != nil {
		t.Fatalf("BuildPlan() with custom FSM error = %v", err)
	}
	constructedCustom, err := assembly.Construct(withCustom, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() with custom FSM error = %v", err)
	}
	t.Cleanup(func() { _, _ = constructedCustom.Unwind(context.Background(), time.Second, nil) })
	customInstance, ok := constructedCustom.Instance(plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
	if !ok {
		t.Fatal("constructed custom-FSM instance not found")
	}
	customNode := customInstance.Primary().(*node)
	if customNode.FSM() != FSM(custom) {
		t.Fatalf("FSM() = %#v, want the exported custom FSM instance", customNode.FSM())
	}
}

func TestSingleNodeElectionApplyBarrierSnapshotAndStop(t *testing.T) {
	cfg := fastConfig("single", true)
	n, err := buildTestNode(cfg, nil)
	if err != nil {
		t.Fatalf("buildTestNode() error = %v", err)
	}
	waitForLeader(t, n)

	ctx, cancel := operationDeadline(t)
	defer cancel()
	command, err := EncodeSet("answer", []byte("42"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := n.Apply(ctx, command)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if result, ok := response.(MutationResult); !ok || result.Existed {
		t.Fatalf("Apply() response = %#v", response)
	}
	if err := n.Barrier(ctx); err != nil {
		t.Fatalf("Barrier() error = %v", err)
	}
	if err := n.Snapshot(ctx); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	configuration, err := n.Configuration(ctx)
	if err != nil || len(configuration.Servers) != 1 || configuration.Servers[0].ID != n.ID() {
		t.Fatalf("Configuration() = %#v, %v", configuration, err)
	}
	fsm := n.FSM().(*KVFSM)
	if value, ok := fsm.Get("answer"); !ok || string(value) != "42" {
		t.Fatalf("default FSM answer = %q, %v", value, ok)
	}
	native := n.Raft()
	if native == nil || native.State() != hashiraft.Leader {
		t.Fatalf("Raft() = %p, state = %v", native, func() any {
			if native == nil {
				return nil
			}
			return native.State()
		}())
	}
	if n.ID() != "single" {
		t.Fatalf("ID() = %q", n.ID())
	}
	_, port, err := net.SplitHostPort(string(n.Address()))
	if err != nil || port == "0" {
		t.Fatalf("Address() = %q, want actual TCP port", n.Address())
	}

	address := string(n.Address())
	const closers = 16
	errs := make(chan error, closers)
	var wg sync.WaitGroup
	wg.Add(closers)
	for range closers {
		go func() {
			defer wg.Done()
			errs <- n.stop(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	assertNoErrorChannel(t, errs)
	if native.State() != hashiraft.Shutdown || n.State() != hashiraft.Shutdown {
		t.Fatalf("state after Stop = native:%s node:%s", native.State(), n.State())
	}
	if _, err := n.Apply(context.Background(), command); !errors.Is(err, ErrStopped) {
		t.Fatalf("Apply after Stop error = %v, want ErrStopped", err)
	}
	requireAddressReusable(t, address)
}

func TestAdvertiseAddressControlsTransportIdentity(t *testing.T) {
	address := reserveTCPAddress(t)
	_, port, _ := net.SplitHostPort(address)
	cfg := fastConfig("advertised", true)
	cfg.BindAddr = net.JoinHostPort("0.0.0.0", port)
	cfg.AdvertiseAddr = address
	n := newTestNode(t, cfg, nil)
	waitForLeader(t, n)
	if string(n.Address()) != address {
		t.Fatalf("Address() = %q, want advertised %q", n.Address(), address)
	}
	if err := n.stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireAddressReusable(t, address)
}

func TestOperationHonorsCanceledContext(t *testing.T) {
	n := newTestNode(t, fastConfig("context", true), nil)
	waitForLeader(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.Apply(ctx, []byte("command")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply(canceled) error = %v", err)
	}
	if err := n.Barrier(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Barrier(canceled) error = %v", err)
	}
	if _, err := n.Configuration(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Configuration(canceled) error = %v", err)
	}
	if err := n.Join(ctx, "other", "127.0.0.1:9999"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Join(canceled) error = %v", err)
	}
	if err := n.Remove(ctx, "other"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove(canceled) error = %v", err)
	}
}

func TestBuildRuntimeRejectsNilFSM(t *testing.T) {
	cfg := fastConfig("nil-fsm", true)
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildRuntime(log.Nop(), normalized, nil); err == nil {
		t.Fatal("buildRuntime with nil FSM error = nil")
	}
}

func TestStopIsSafeOnNilNode(t *testing.T) {
	var n *node
	if err := n.stop(context.Background()); err != nil {
		t.Fatalf("stop() on nil node error = %v", err)
	}
}
