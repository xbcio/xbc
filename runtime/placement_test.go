package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	assert.True(t, placement.Hosts("sast"))
	assert.False(t, placement.Hosts("sca"))
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

// workloadKeys reduces a request's declarations to the part the assertion is
// about, so it does not also pin the replica counts.
func workloadKeys(workloads []plugin.Workload) []plugin.WorkloadKey {
	keys := make([]plugin.WorkloadKey, len(workloads))
	for index, workload := range workloads {
		keys[index] = workload.Key
	}
	return keys
}
