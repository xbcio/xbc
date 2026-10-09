package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/plugin/assembly"
)

// placementTestBundles builds the composition the runtime-level placement tests
// are read against: two workloads and one unowned Definition, so the hosted set
// is a set rather than a switch.
func placementTestBundles() []plugin.Bundle {
	return []plugin.Bundle{
		plugin.WorkloadOf("sast", plugin.BundleOf(
			plugin.Define("sast-worker", func(plugin.BuildContext) (*runtimeTestValue, error) {
				return &runtimeTestValue{}, nil
			}),
		), plugin.WithReplicas(3), plugin.WithExclusiveProcess()),
		plugin.WorkloadOf("sca", plugin.BundleOf(
			plugin.Define("sca-worker", func(plugin.BuildContext) (*runtimeTestValue, error) {
				return &runtimeTestValue{}, nil
			}),
		), plugin.WithReplicas(2)),
		plugin.BundleOf(plugin.Define("web", func(plugin.BuildContext) (*runtimeTestValue, error) {
			return &runtimeTestValue{}, nil
		})),
	}
}

// errPlacementUnavailable stands in for a lease store that cannot be reached at
// startup.
var errPlacementUnavailable = errors.New("placement test: lease store unreachable")

// recordingPlacement is a PlacementSource that answers with whatever the test
// told it to, and records what it was asked.
type recordingPlacement struct {
	placement plugin.Placement
	err       error
	requests  []plugin.PlacementRequest
}

func (source *recordingPlacement) Resolve(request plugin.PlacementRequest) (plugin.Placement, error) {
	source.requests = append(source.requests, request)
	if source.err != nil {
		return plugin.Placement{}, source.err
	}
	return source.placement, nil
}

func placementTestConfig(t *testing.T, extra string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "application.yml")
	contents := "log:\n  console:\n    enabled: false\n  file:\n    enabled: false\nxbc:\n  shutdown_timeout: 5s\n" + extra
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return []string{"--config", path}
}

// TestStaticPlacementHostsEveryEnabledWorkload is the default's delivery
// boundary: a deployment that assigns roles in configuration carries exactly
// the workloads it enabled, and carries the unowned Definitions whatever it
// enabled.
func TestStaticPlacementHostsEveryEnabledWorkload(t *testing.T) {
	app := newApp(placementTestBundles())
	cmd, err := parseArgs(placementTestConfig(t, "workloads:\n  sca:\n    enabled: false\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	placement, err := app.resolvePlacement()
	require.NoError(t, err)
	assert.Equal(t, "static", placement.Source)
	assert.Equal(t, []plugin.WorkloadKey{"sast"}, placement.Hosted,
		"an enabled: false workload is never hosted by the default source")
}

// TestWithPlacementReplacesTheDefaultDecision pins the seam: the source the
// application supplied is the one consulted, it sees the declared workloads and
// the configuration's veto, and its answer is the one the plan is built from.
func TestWithPlacementReplacesTheDefaultDecision(t *testing.T) {
	source := &recordingPlacement{placement: plugin.Placement{
		Source: "lease",
		Holder: "host-7-1726-9f3c1a2b",
		Hosted: []plugin.WorkloadKey{"sca"},
	}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)
	app.ready = make(chan struct{})

	cmd, err := parseArgs(placementTestConfig(t, "workloads:\n  sast:\n    enabled: false\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	placement, err := app.resolvePlacement()
	require.NoError(t, err)
	assert.Equal(t, "lease", placement.Source)
	assert.Equal(t, "host-7-1726-9f3c1a2b", placement.Holder)
	assert.Equal(t, []plugin.WorkloadKey{"sca"}, placement.Hosted)

	require.Len(t, source.requests, 1, "the source is consulted exactly once per run")
	assert.Equal(t, []plugin.WorkloadKey{"sast", "sca"}, workloadKeys(source.requests[0].Workloads),
		"the source sees the declared workloads, sorted by key")
	assert.False(t, source.requests[0].Admits("sast"), "the configuration veto is visible to the source")
	assert.True(t, source.requests[0].Admits("sca"))
}

// TestAPlacementSourceIsToldWhichProcessIsAsking covers the identity half of the
// request. A source that claims capacity on this process's behalf is the only
// party that can record which process claimed it, and it cannot derive that: it
// is built at the composition root, before any configuration is bound.
//
// The derived default is asserted rather than a configured one, because that is
// the case with no deployment input behind it -- an empty Instance here would
// leave every claim attributable only to whatever token the store invented.
func TestAPlacementSourceIsToldWhichProcessIsAsking(t *testing.T) {
	source := &recordingPlacement{placement: plugin.Placement{Source: "lease"}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)
	app.ready = make(chan struct{})

	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	_, err = app.resolvePlacement()
	require.NoError(t, err)

	require.Len(t, source.requests, 1)
	assert.NotEmpty(t, source.requests[0].Instance,
		"a process always has an identity; the runtime derives one when the deployment names none")
	assert.Equal(t, app.settings.Instance(), source.requests[0].Instance,
		"the source is told the same identity every log line, metric and doctor report uses")
}

// TestAConfiguredInstanceIdentityReachesThePlacementSource is the deployment
// case: a supervisor that already knows which slot this process is takes the
// naming over, and the name it chose is what a claim is recorded under.
func TestAConfiguredInstanceIdentityReachesThePlacementSource(t *testing.T) {
	source := &recordingPlacement{placement: plugin.Placement{Source: "lease"}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)
	app.ready = make(chan struct{})

	cmd, err := parseArgs(placementTestConfig(t, "  instance_id: scanner-2\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	_, err = app.resolvePlacement()
	require.NoError(t, err)

	require.Len(t, source.requests, 1)
	assert.Equal(t, "scanner-2", source.requests[0].Instance)
}

// TestOnePlacementIdentityIsAlsoWhatPluginsPublish closes the loop the claimant
// parameter opened. A placement source records which process holds a slot; a
// plugin holding anything else the replicas share -- a job lock, a claim row --
// has to record the same thing, or an operator reading two stores gets two
// answers about one process and cannot line them up.
//
// So the assertion is equality, not non-emptiness: the Context a plugin receives
// must hand back the identity the source was told, not a second one derived
// beside it.
func TestOnePlacementIdentityIsAlsoWhatPluginsPublish(t *testing.T) {
	source := &recordingPlacement{placement: plugin.Placement{Source: "lease"}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)
	app.ready = make(chan struct{})

	cmd, err := parseArgs(placementTestConfig(t, "  instance_id: scanner-2\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	_, err = app.resolvePlacement()
	require.NoError(t, err)
	require.Len(t, source.requests, 1)

	ctx := plugin.NewRuntimeContext(hostAdapter{app: app}, plugin.Identity{Plugin: "cron", Instance: "reports"})
	assert.Equal(t, source.requests[0].Instance, ctx.ProcessInstance(),
		"a plugin publishes the same process identity the placement source was told")
	assert.Equal(t, "reports", ctx.Instance(),
		"the plugin instance name stays a separate answer and is not overwritten by it")
}

// TestWithPlacementOmittedIsStaticPlacement keeps the two spellings of the
// default from drifting.
//
// The configuration has to leave one workload out, because this composition
// declares an exclusive one: hosting sast beside sca is refused outright, so a
// run that carried both could not settle a placement at all. Everything the
// test is about -- which decision the two spellings produce -- is unaffected.
func TestWithPlacementOmittedIsStaticPlacement(t *testing.T) {
	omitted := newApp(placementTestBundles())
	explicit, err := New(WithBundles(placementTestBundles()...), WithPlacement(StaticPlacement()))
	require.NoError(t, err)

	cmd, err := parseArgs(placementTestConfig(t, "workloads:\n  sca:\n    enabled: false\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, omitted.bootstrap(cmd))
	require.NoError(t, explicit.bootstrap(cmd))

	want, err := omitted.resolvePlacement()
	require.NoError(t, err)
	got, err := explicit.resolvePlacement()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestPlacementFailsStartupOnAnUnsatisfiableAnswer covers the two ways a
// source's answer is rejected. Neither may be tolerated: a hosted key nothing
// declares would leave the process carrying nothing but unowned plugins while
// still reporting ready, and a duplicate makes a count disagree with the set
// the plan actually used.
func TestPlacementFailsStartupOnAnUnsatisfiableAnswer(t *testing.T) {
	for name, hosted := range map[string][]plugin.WorkloadKey{
		"undeclared workload": {"nope"},
		"duplicate workload":  {"sca", "sca"},
	} {
		t.Run(name, func(t *testing.T) {
			app, err := New(WithBundles(placementTestBundles()...), WithPlacement(&recordingPlacement{
				placement: plugin.Placement{Source: "lease", Hosted: hosted},
			}))
			require.NoError(t, err)
			cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
			require.NoError(t, err)
			require.NoError(t, app.bootstrap(cmd))

			_, err = app.resolvePlacement()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "placement source")
			assert.Contains(t, err.Error(), string(hosted[0]))
		})
	}
}

// TestPlacementRejectsAnUnattributedDecision closes the one rejection that
// fails open. Assembly reads a Placement with no source and no hosted keys as
// "nobody was consulted", which hosts every enabled workload -- the path a
// caller with no placement source needs. A source that answered with the zero
// value, because it forgot to name itself or because it meant to carry nothing,
// would have that reading applied to a real decision and end up carrying
// everything, which is exactly the non-deterministic, capacity-fail-open shape
// the cold-start rule forbids.
func TestPlacementRejectsAnUnattributedDecision(t *testing.T) {
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(&recordingPlacement{
		placement: plugin.Placement{},
	}))
	require.NoError(t, err)
	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	_, err = app.resolvePlacement()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty Source")
	assert.Contains(t, err.Error(), "would host every enabled workload",
		"the message says why an unnamed decision cannot be accepted")
	assert.Contains(t, err.Error(), "host no key to carry none",
		"the message names the spelling of the intent it is not refusing")
}

// TestPlacementHonoursAnAttributedEmptyHostedSet is the other side of that
// rejection: "this process carries no workload" is a legitimate decision -- it
// is the standby a lease source produces when it wins no slot -- and a named
// source that hosts no key must really carry none.
//
// It is asserted against the plan rather than against the decision alone,
// because the failure being guarded against lives in assembly's reading of the
// decision, not in the decision itself.
func TestPlacementHonoursAnAttributedEmptyHostedSet(t *testing.T) {
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(&recordingPlacement{
		placement: plugin.Placement{Source: "lease", Hosted: nil},
	}))
	require.NoError(t, err)
	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	placement, err := app.resolvePlacement()
	require.NoError(t, err)
	assert.Equal(t, "lease", placement.Source)
	assert.Empty(t, placement.Hosted)

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles:   app.bundles,
		Env:       app.env,
		Logger:    app.logger,
		Placement: placement,
	})
	require.NoError(t, err)
	assert.Equal(t, []plugin.Identity{{Plugin: "web", Instance: plugin.DefaultInstance}}, plan.Order(),
		"an empty hosted set contributes no workload Definition, and leaves the unowned ones alone")
	for _, hosted := range plan.Workloads() {
		assert.False(t, hosted.Hosted, "workload %q is not carried by this process", hosted.Workload.Key)
		assert.Empty(t, hosted.Identities, "workload %q contributed no instance", hosted.Workload.Key)
	}
}

// TestPlacementFailsStartupWhenTheSourceCannotAnswer is the cold-start rule: a
// process whose role cannot be decided must not guess. Falling back to "host
// everything" would make the process shape non-deterministic and, in a lease
// deployment, fail open on capacity.
func TestPlacementFailsStartupWhenTheSourceCannotAnswer(t *testing.T) {
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(&recordingPlacement{
		err: errPlacementUnavailable,
	}))
	require.NoError(t, err)
	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	_, err = app.resolvePlacement()
	require.ErrorIs(t, err, errPlacementUnavailable)
}

// TestWorkloadSectionAcceptsThePlainEnvironmentSpelling pins the environment
// path of a workload's own section. The workloads root is not the framework's
// own root, so it keeps its complete spelling: workloads.sast.max_goroutines is
// XBC_WORKLOADS_SAST_MAX_GOROUTINES.
func TestWorkloadSectionAcceptsThePlainEnvironmentSpelling(t *testing.T) {
	t.Setenv("XBC_WORKLOADS_SAST_ENABLED", "false")
	t.Setenv("XBC_WORKLOADS_SAST_MAX_GOROUTINES", "64")

	app := newApp(placementTestBundles())
	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	placement, err := app.resolvePlacement()
	require.NoError(t, err)
	assert.Equal(t, []plugin.WorkloadKey{"sca"}, placement.Hosted,
		"an environment veto removes a workload from the hosted set")
}

// TestUndeclaredWorkloadKeyIsRejectedByName keeps a misspelled workload from
// being silently ignored. The workloads namespace claims the prefix, so only
// the declared keys beneath it answer for a variable or a block.
func TestUndeclaredWorkloadKeyIsRejectedByName(t *testing.T) {
	app := newApp(placementTestBundles())
	cmd, err := parseArgs(placementTestConfig(t, "workloads:\n  sasst:\n    enabled: true\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	err = app.bootstrap(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workloads.sasst")
	assert.Contains(t, err.Error(), "no plugin or framework section owns")
}

// TestInstanceIDIsStablePerProcessAndOverridable pins the identity a placement
// source records. The derived default must be computed once -- a token that
// changed between the acquisition that recorded it and the renewal that
// presents it would break the lease it names -- while an explicit value is used
// exactly as written, because an operator who names a process means that name
// to survive a restart.
func TestInstanceIDIsStablePerProcessAndOverridable(t *testing.T) {
	app := newApp(placementTestBundles())
	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	derived := app.settings.Instance()
	assert.NotEmpty(t, derived)
	assert.Equal(t, derived, app.settings.Instance(), "the derived identity is resolved once")
	assert.NotContains(t, derived, " ", "it is one token wherever it is printed")
	// host-<unix seconds>-<8 hex digits>
	parts := strings.Split(derived, "-")
	require.GreaterOrEqual(t, len(parts), 3, "the identity names the host, the boot time and a suffix: %s", derived)
	assert.Len(t, parts[len(parts)-1], 8)

	t.Setenv("XBC_INSTANCE_ID", "sast-3")
	override := newApp(placementTestBundles())
	cmd, err = parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, override.bootstrap(cmd))
	assert.Equal(t, "sast-3", override.settings.Instance())
}

// TestExclusiveWorkloadIsRefusedBesideAnotherWorkload is the co-residence rule
// at the level an operator meets it: the process is refused before anything is
// built, and the refusal names both declarations so there is no guessing which
// one to move.
//
// The unowned Definition is deliberately part of this composition. An exclusive
// workload sharing its process with the transport and the health probes is the
// ordinary deployment shape -- "no other workload" is about workloads, not
// about the infrastructure every process carries -- and a rule that refused
// that would refuse every real composition.
func TestExclusiveWorkloadIsRefusedBesideAnotherWorkload(t *testing.T) {
	app := newApp(placementTestBundles())
	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	_, err = app.resolvePlacement()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `workload "sast" requires a process of its own`)
	assert.Contains(t, err.Error(), `also hosts "sca"`, "the message names the other side of the conflict")
	assert.Contains(t, err.Error(), "workloads.sast.enabled",
		"the suggestion names the key an operator can act on")
	assert.NotContains(t, err.Error(), `"web"`,
		"an unowned Definition is not a workload and cannot conflict with an exclusive one")
}

// TestExclusiveWorkloadAloneInItsProcessIsHosted is the other half of the rule:
// exclusivity restricts co-residence, it does not forbid being hosted. A
// deployment gives an exclusive workload a process of its own by turning the
// others off there, and that process must start.
func TestExclusiveWorkloadAloneInItsProcessIsHosted(t *testing.T) {
	app := newApp(placementTestBundles())
	cmd, err := parseArgs(placementTestConfig(t, "workloads:\n  sca:\n    enabled: false\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	placement, err := app.resolvePlacement()
	require.NoError(t, err)
	assert.Equal(t, []plugin.WorkloadKey{"sast"}, placement.Hosted)
}

// TestAnyHostedSetIsAllowedWithoutAnExclusiveWorkload is the rule's third side.
// Three ordinary workloads sharing one process is the shape placement exists to
// make possible, so nothing here may be restricted.
func TestAnyHostedSetIsAllowedWithoutAnExclusiveWorkload(t *testing.T) {
	workload := func(key plugin.WorkloadKey, definition plugin.Key) plugin.Bundle {
		return plugin.WorkloadOf(key, plugin.BundleOf(
			plugin.Define(definition, func(plugin.BuildContext) (*runtimeTestValue, error) {
				return &runtimeTestValue{}, nil
			}),
		), plugin.WithReplicas(2))
	}
	app := newApp([]plugin.Bundle{
		workload("alpha", "alpha-worker"),
		workload("beta", "beta-worker"),
		workload("gamma", "gamma-worker"),
	})
	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	placement, err := app.resolvePlacement()
	require.NoError(t, err)
	assert.Equal(t, []plugin.WorkloadKey{"alpha", "beta", "gamma"}, placement.Hosted)
}

// TestValidateExclusiveHostingCoversEveryShape is the rule as a table, one row
// per way a hosted set can relate to an exclusive declaration.
//
// It is written against the validator rather than only through a boot because
// the rule is a property of the set, and the two shapes that must stay legal --
// an exclusive workload alone, and any set of ordinary ones -- are exactly the
// two a wrong comparison collapses. Making the size test always true lets the
// conflict rows through; making it always false refuses the exclusive-alone
// row. Both mutations fail here.
func TestValidateExclusiveHostingCoversEveryShape(t *testing.T) {
	t.Parallel()
	declared := []plugin.Workload{
		{Key: "sast", Exclusive: true, Replicas: 3},
		{Key: "sca", Replicas: 2},
		{Key: "webscan", Replicas: 2},
		{Key: "solo", Exclusive: true, Replicas: 1},
	}
	for name, testCase := range map[string]struct {
		hosted     []plugin.WorkloadKey
		wantErr    bool
		wantOthers string
	}{
		"nothing hosted":        {hosted: nil},
		"one ordinary workload": {hosted: []plugin.WorkloadKey{"sca"}},
		"ordinary pair":         {hosted: []plugin.WorkloadKey{"sca", "webscan"}},
		"exclusive alone":       {hosted: []plugin.WorkloadKey{"sast"}},
		"exclusive with an ordinary": {
			hosted:     []plugin.WorkloadKey{"sast", "sca"},
			wantErr:    true,
			wantOthers: `also hosts "sca"`,
		},
		"exclusive with two ordinary": {
			hosted:     []plugin.WorkloadKey{"webscan", "sast", "sca"},
			wantErr:    true,
			wantOthers: `also hosts "sca", "webscan"`,
		},
		"two exclusive workloads": {
			hosted:     []plugin.WorkloadKey{"solo", "sast"},
			wantErr:    true,
			wantOthers: `also hosts "solo"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateExclusiveHosting(testCase.hosted, declared)
			if !testCase.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "requires a process of its own")
			assert.Contains(t, err.Error(), testCase.wantOthers,
				"the conflicting workloads are named in key order, whatever order the source produced")
		})
	}
}

// TestInstanceIDRejectsWhitespace keeps the identity usable as one log field
// and one lease owner token.
func TestInstanceIDRejectsWhitespace(t *testing.T) {
	app := newApp(placementTestBundles())
	cmd, err := parseArgs(placementTestConfig(t, "  instance_id: \"host 7\"\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	err = app.bootstrap(cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "xbc.instance_id")
}

// TestPlacementCannotOverrideADeploymentVeto is the diagnostic half of the
// enabled:false rule. The veto itself already holds -- no source may host a
// workload the deployment disabled -- but a source that tried was told its key
// was undeclared, which names a spelling problem instead of the override it
// actually attempted. The two are different operator mistakes with different
// fixes, so the message has to say which one happened.
func TestPlacementCannotOverrideADeploymentVeto(t *testing.T) {
	source := &recordingPlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"sca"},
	}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)
	app.ready = make(chan struct{})

	cmd, err := parseArgs(placementTestConfig(t, "workloads:\n  sca:\n    enabled: false\n"), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	_, err = app.resolvePlacement()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workloads.sca.enabled",
		"the message names the key the deployment actually set")
	assert.Contains(t, err.Error(), "veto",
		"it says a source cannot overrule the deployment")
	assert.NotContains(t, err.Error(), "does not declare",
		"a declared-but-disabled key is not an undeclared one")
}

// TestResolvedPlacementSortsTheHostedSet pins the "sorted by key" contract on
// Placement.Hosted. Two hosted-set readers -- the startup line and doctor --
// print it directly, and a source is free to answer in whatever order it walked
// its store, so the order is settled where the decision is accepted.
func TestResolvedPlacementSortsTheHostedSet(t *testing.T) {
	workload := func(key plugin.WorkloadKey, definition plugin.Key) plugin.Bundle {
		return plugin.WorkloadOf(key, plugin.BundleOf(
			plugin.Define(definition, func(plugin.BuildContext) (*runtimeTestValue, error) {
				return &runtimeTestValue{}, nil
			}),
		), plugin.WithReplicas(2))
	}
	source := &recordingPlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"gamma", "alpha", "beta"},
	}}
	app, err := New(WithBundles(
		workload("alpha", "alpha-worker"),
		workload("beta", "beta-worker"),
		workload("gamma", "gamma-worker"),
	), WithPlacement(source))
	require.NoError(t, err)

	cmd, err := parseArgs(placementTestConfig(t, ""), config.DefaultEnvPrefix)
	require.NoError(t, err)
	require.NoError(t, app.bootstrap(cmd))

	placement, err := app.resolvePlacement()
	require.NoError(t, err)
	assert.Equal(t, []plugin.WorkloadKey{"alpha", "beta", "gamma"}, placement.Hosted,
		"the accepted hosted set is reported in key order whatever order the source produced")
}

// workloadKeys reduces a request's declarations to the part the assertion is
// about, so it does not also pin the replica counts.
func workloadKeys(workloads []plugin.Workload) []plugin.WorkloadKey {
	keys := make([]plugin.WorkloadKey, len(workloads))
	for index, workload := range workloads {
		keys[index] = workload.Key
	}
	return keys
}

// ── releasing what a decision acquired ──────────────────────────────────────

// claimingPlacement is a PlacementSource that claims something while it
// resolves and only gives it back through Release -- the shape the lease source
// has, where Resolve wins the slots and the constructed plugin's PreStop and
// Stop hooks are what hand them back. claimed stands in for a held slot: it is
// the observable the runtime's release on the pre-Construction paths exists to
// clear.
//
// The state is atomic because the hand-off test drives a real Execute on
// another goroutine while the test reads the claim.
type claimingPlacement struct {
	placement plugin.Placement

	claimed  atomic.Bool
	releases atomic.Int32
}

func (source *claimingPlacement) Resolve(plugin.PlacementRequest) (plugin.Placement, error) {
	source.claimed.Store(true)
	return source.placement, nil
}

func (source *claimingPlacement) Release(context.Context) error {
	source.releases.Add(1)
	source.claimed.Store(false)
	return nil
}

// refusingReleasePlacement is a claimingPlacement whose handback always fails:
// the lease store that refuses the release of what the decision won, which is
// the failure doctor has no logger to warn about.
type refusingReleasePlacement struct {
	placement plugin.Placement
	releases  atomic.Int32
}

func (source *refusingReleasePlacement) Resolve(plugin.PlacementRequest) (plugin.Placement, error) {
	return source.placement, nil
}

func (source *refusingReleasePlacement) Release(context.Context) error {
	source.releases.Add(1)
	return errPlacementUnavailable
}

// stoppingPlacement is a PlacementSource that asks this run to stop while it
// resolves, which is the timing a stop request arriving during planning has.
// It drives the runtime's planning checkpoint -- ensureStarting("planning") --
// from the one moment at which the source has already paid for its answer, and
// it records whether the release it is handed still carried the cancelled run,
// which is the state the release must not inherit.
type stoppingPlacement struct {
	placement plugin.Placement
	stop      func()

	releases          atomic.Int32
	cancelledReleases atomic.Int32
}

func (source *stoppingPlacement) Resolve(plugin.PlacementRequest) (plugin.Placement, error) {
	source.stop()
	return source.placement, nil
}

func (source *stoppingPlacement) Release(ctx context.Context) error {
	source.releases.Add(1)
	if ctx.Err() != nil {
		source.cancelledReleases.Add(1)
	}
	return nil
}

// placementUnwiredContract is exported by nothing in the compositions below,
// which is how a plan failure is spelled: a required input with no producer
// fails BuildPlan, after the placement decision has already been made.
type placementUnwiredContract interface{ Unwired() }

// TestDoctorGivesBackWhatPlacementAcquiredToAnswer pins the read-only command's
// boundary with the source it consults. Doctor has to resolve the hosted set to
// report it, and for a lease source resolving is winning: the slots are taken
// while the answer is produced, and nothing is constructed to own them, so the
// runtime must give them back before the command returns. Otherwise a
// diagnostic would hold cluster capacity until each lease expired.
func TestDoctorGivesBackWhatPlacementAcquiredToAnswer(t *testing.T) {
	source := &claimingPlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"sca"},
	}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)

	out := runDoctor(t, app, runtimeTestConfig(t, time.Second)...)

	assert.Contains(t, out, "hosted   sca",
		"the report is still produced from the decision the source made")
	assert.False(t, source.claimed.Load(),
		"doctor constructs nothing, so nothing owns the claim resolving made")
	assert.Equal(t, int32(1), source.releases.Load(),
		"the release is attempted exactly once")
}

// TestDoctorReportsAPlacementReleaseThatDidNotComplete pins doctor's one
// surface for a failure the run path would log. Doctor runs against a no-op
// logger, so a lease store that refuses the handback would otherwise leave the
// claim to expire with nothing said about it. The note lands in the report and
// names that outcome; the command's own result is untouched, which runDoctor
// asserts by requiring exit code 0 and no error.
func TestDoctorReportsAPlacementReleaseThatDidNotComplete(t *testing.T) {
	source := &refusingReleasePlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"sca"},
	}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)

	out := runDoctor(t, app, runtimeTestConfig(t, time.Second)...)

	assert.Contains(t, out, "hosted   sca",
		"the report is still produced from the decision the source made")
	assert.Contains(t, out, "placement release",
		"the failure is reported where a logger-less command can carry it")
	assert.Contains(t, out, errPlacementUnavailable.Error(),
		"the store's own failure is what the note hands to the operator")
	assert.Contains(t, out, "expires with its own lease ttl",
		"the note says what becomes of the claim that was not given back")
	assert.Equal(t, int32(1), source.releases.Load(),
		"the release is still attempted exactly once")
}

// TestAPlanFailureGivesBackWhatPlacementAcquired covers the other pre-construct
// exit: the source answered and won its claim, and the plan built from that
// answer then failed. Construct never runs, so the release cannot be left to
// the plugin graph.
func TestAPlanFailureGivesBackWhatPlacementAcquired(t *testing.T) {
	needsAnUnwiredContract := plugin.Define("needs-an-unwired-contract",
		func(plugin.BuildContext) (*runtimeTestValue, error) { return &runtimeTestValue{}, nil },
		plugin.Options[*runtimeTestValue]{Inputs: plugin.Inputs(plugin.RequireOne[placementUnwiredContract]())},
	)
	source := &claimingPlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"sca"},
	}}
	app, err := New(WithBundles(
		append(placementTestBundles(), plugin.BundleOf(needsAnUnwiredContract))...,
	), WithPlacement(source))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))

	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "found none",
		"the failure is the unwired required input, not placement")
	assert.False(t, source.claimed.Load(),
		"a plan that never reaches Construct owes the claim back")
	assert.Equal(t, int32(1), source.releases.Load())
	assert.Nil(t, app.owned)
}

// TestAConstructionFailureGivesBackWhatPlacementAcquired covers the boundary
// from the failed side: Construct was attempted and did not succeed, so no
// plugin owns the claim yet and the release still owes it back.
func TestAConstructionFailureGivesBackWhatPlacementAcquired(t *testing.T) {
	refusesToBuild := plugin.Define("refuses-to-build",
		func(plugin.BuildContext) (*runtimeTestValue, error) {
			return nil, errors.New("factory refuses")
		})
	source := &claimingPlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"sca"},
	}}
	app, err := New(WithBundles(
		append(placementTestBundles(), plugin.BundleOf(refusesToBuild))...,
	), WithPlacement(source))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))

	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory refuses")
	assert.False(t, source.claimed.Load(),
		"a construction that did not succeed leaves the claim to the runtime")
	assert.Equal(t, int32(1), source.releases.Load())
	assert.Nil(t, app.owned)
}

// TestARefusedDecisionGivesBackWhatPlacementAcquired covers the source that
// answered and was refused. The runtime rejects the decision after the source
// has already paid for it, so the release cannot be left to the answer's
// consumer: there will not be one.
func TestARefusedDecisionGivesBackWhatPlacementAcquired(t *testing.T) {
	source := &claimingPlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"undeclared"},
	}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))

	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not declare")
	assert.False(t, source.claimed.Load(),
		"an answer the runtime refuses is still the source's claim until it is released")
	assert.Equal(t, int32(1), source.releases.Load())
}

// TestANothingEnabledPlanGivesBackWhatPlacementAcquired covers the pre-construct
// exit that never reaches Construct because there is no graph to construct:
// placement carried no workload and the composition declared nothing beside
// them, so the plan is empty and the run is refused by errNothingEnabled. The
// release remains the only thing that gives the claim back.
func TestANothingEnabledPlanGivesBackWhatPlacementAcquired(t *testing.T) {
	source := &claimingPlacement{placement: plugin.Placement{
		Source: "lease",
	}}
	app, err := New(WithBundles(
		plugin.WorkloadOf("sca", plugin.BundleOf(
			plugin.Define("sca-worker", func(plugin.BuildContext) (*runtimeTestValue, error) {
				return &runtimeTestValue{}, nil
			}),
		), plugin.WithReplicas(2)),
	), WithPlacement(source))
	require.NoError(t, err)

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))

	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not carried by this process",
		"the failure is the empty plan, not placement")
	assert.False(t, source.claimed.Load(),
		"a plan that enables nothing owes the claim back")
	assert.Equal(t, int32(1), source.releases.Load())
	assert.Nil(t, app.owned, "nothing is constructed when the plan enables nothing")
}

// TestAStopDuringPlanningGivesBackWhatPlacementAcquired covers the exit whose
// shape depends on when the stop lands: the source answered and was paid for,
// the plan built from that answer, and the run was refused at the planning
// checkpoint -- before Construct, so the plugin graph never had the claim to
// own. The release has to work on a run whose context the stop already
// cancelled, which is exactly the case releasePlacement drops that cancellation
// for.
func TestAStopDuringPlanningGivesBackWhatPlacementAcquired(t *testing.T) {
	source := &stoppingPlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"sca"},
	}}
	app, err := New(WithBundles(placementTestBundles()...), WithPlacement(source))
	require.NoError(t, err)
	source.stop = func() { app.requestStop(stopReasonSignal) }

	code, err := app.Execute(context.Background(), runtimeTestConfig(t, time.Second))

	assert.Equal(t, 1, code)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aborting planning phase",
		"the refusal is the planning checkpoint that follows the decision")
	assert.Equal(t, int32(1), source.releases.Load(),
		"the claim is given back exactly once even though the stop cancelled the run")
	assert.Zero(t, source.cancelledReleases.Load(),
		"the release runs on a context detached from the stop that cancelled the run, or a store would fail it on its first call")
	assert.Nil(t, app.owned, "the stop is honoured before anything is constructed")
}

// TestAConstructedGraphOwnsTheClaimUntilTheRunEnds pins both halves of the
// release boundary. A live run's claim belongs to the plugin graph -- the
// placement plugin's PreStop and Stop hooks are what give it back -- so the
// runtime's backstop must not touch it while the process still serves the
// workload it was won for. Once the run has ended, the backstop is the one
// that returns whatever is left, and this composition is exactly the case
// that needs it: the source that claimed is not the graph that would release,
// because the Bundle carrying the releasing plugin was never selected.
//
// The boundary is positional as well as counted: the carried plugin's own Stop
// records the release count it observes, which must still be zero when the last
// of the graph unwinds. A backstop that returned the claim beside the stop
// rather than after the whole reverse order would fail that reading, which no
// count taken after the run could tell apart.
func TestAConstructedGraphOwnsTheClaimUntilTheRunEnds(t *testing.T) {
	source := &claimingPlacement{placement: plugin.Placement{
		Source: "lease",
		Hosted: []plugin.WorkloadKey{"sca"},
	}}
	// -1 marks the hook as never run, so a composition that stops without
	// calling it cannot pass the reading below by leaving it at its zero value.
	releasesAtStop := atomic.Int32{}
	releasesAtStop.Store(-1)
	definition := plugin.Define("carried", func(plugin.BuildContext) (*runtimeTestValue, error) {
		return &runtimeTestValue{}, nil
	}, plugin.Options[*runtimeTestValue]{Lifecycle: plugin.Lifecycle[*runtimeTestValue]{
		OpenTraffic: func(*runtimeTestValue, *plugin.Context) error { return nil },
		Stop: func(*runtimeTestValue, context.Context) error {
			releasesAtStop.Store(source.releases.Load())
			return nil
		},
	}})
	app, err := New(WithBundles(
		plugin.WorkloadOf("sca", plugin.BundleOf(definition), plugin.WithReplicas(2)),
	), WithPlacement(source))
	require.NoError(t, err)
	app.ready = make(chan struct{})

	result := executeRuntimeTest(app, runtimeTestConfig(t, time.Second)...)
	awaitRuntimeTestReady(t, app)

	assert.True(t, source.claimed.Load(),
		"the constructed plugin owns the claim once Construct has succeeded")
	assert.Zero(t, source.releases.Load(),
		"a live run's claim is the plugin graph's to hold, never the runtime's to return")

	app.requestStop(stopReasonSignal)
	completed := awaitRuntimeTestResult(t, result)
	require.NoError(t, completed.err)
	assert.Equal(t, 0, completed.code)
	assert.Equal(t, int32(0), releasesAtStop.Load(),
		"the graph's own Stop still saw the claim held, so the backstop runs after the last unwind and not beside the stop")
	assert.Equal(t, int32(1), source.releases.Load(),
		"the end of the run returns the claim, which nothing in this composition else could")
}
