package model

import (
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateIdentifierAcceptsCanonicalNamesAndNamesTheKind(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"web", "gorm-readonly", "db_2", "a1"} {
		assert.NoError(t, Key(value).Validate(), value)
		assert.NoError(t, ValidateInstanceName(value), value)
	}

	err := Key("").Validate()
	require.Error(t, err)
	assert.EqualError(t, err, "xbc: plugin key cannot be empty")

	err = ValidateInstanceName("")
	require.Error(t, err)
	assert.EqualError(t, err, "xbc: instance name cannot be empty")

	for _, value := range []string{"Web", "web.server", "web server", "web/one", "wéb"} {
		err := Key(value).Validate()
		require.Error(t, err, value)
		assert.Contains(t, err.Error(), "plugin key")
		assert.Contains(t, err.Error(), "contains invalid character")
		assert.Contains(t, err.Error(), "only lowercase letters, digits, underscores, and hyphens are allowed")
	}

	err = ValidateInstanceName("Readonly")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instance name")
	assert.Contains(t, err.Error(), "contains invalid character")
}

func TestIdentityNormalizationRenderingAndOrder(t *testing.T) {
	t.Parallel()
	assert.Equal(t, DefaultInstance, NormalizeInstance(""))
	assert.Equal(t, "readonly", NormalizeInstance("readonly"))
	assert.Equal(t, Identity{Plugin: "gorm", Instance: DefaultInstance}, Identity{Plugin: "gorm"}.Normalized())

	assert.Equal(t, "gorm", Identity{Plugin: "gorm"}.String())
	assert.Equal(t, "gorm", Identity{Plugin: "gorm", Instance: DefaultInstance}.String())
	assert.Equal(t, "gorm[readonly]", Identity{Plugin: "gorm", Instance: "readonly"}.String())

	assert.Zero(t, CompareIdentity(Identity{Plugin: "gorm"}, Identity{Plugin: "gorm", Instance: DefaultInstance}))
	assert.Negative(t, CompareIdentity(Identity{Plugin: "a"}, Identity{Plugin: "b"}))
	assert.Positive(t, CompareIdentity(Identity{Plugin: "b"}, Identity{Plugin: "a"}))
	assert.Negative(t, CompareIdentity(Identity{Plugin: "a"}, Identity{Plugin: "a", Instance: "readonly"}))
	assert.Positive(t, CompareIdentity(Identity{Plugin: "a", Instance: "readonly"}, Identity{Plugin: "a"}))

	identities := []Identity{
		{Plugin: "b"},
		{Plugin: "a", Instance: "readonly"},
		{Plugin: "a"},
	}
	SortIdentities(identities)
	assert.Equal(t, []Identity{
		{Plugin: "a"},
		{Plugin: "a", Instance: "readonly"},
		{Plugin: "b"},
	}, identities)
}

func TestCardinalityRendersDiagnosticNames(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "single", SingleInstance.String())
	assert.Equal(t, "multiple", MultipleInstances.String())
	assert.Equal(t, "cardinality(7)", Cardinality(7).String())
}

func TestNewInputTokenAssignsDistinctIdentifiersAndNormalizesInstance(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf((*any)(nil)).Elem()
	first := NewInputToken(QueryRef, typ, "producer", "", "origin.go:1")
	second := NewInputToken(QueryMany, typ, "", "readonly", "origin.go:2")

	require.NotZero(t, first.ID)
	assert.NotEqual(t, first.ID, second.ID)
	assert.Equal(t, QueryRef, first.Kind)
	assert.Equal(t, DefaultInstance, first.Instance)
	assert.Equal(t, "producer", first.Key.String())
	assert.Equal(t, "origin.go:1", first.Origin)
	assert.Equal(t, "readonly", second.Instance)
	assert.Equal(t, typ, second.Type)

	var mu sync.Mutex
	seen := map[uint64]bool{}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := NewInputToken(QueryOne, typ, "", "", "")
			mu.Lock()
			defer mu.Unlock()
			assert.False(t, seen[token.ID], "token identifiers are unique across goroutines")
			seen[token.ID] = true
		}()
	}
	wg.Wait()
}

func TestDefinitionHandleIsOpaqueAndCopiesMutableMetadata(t *testing.T) {
	t.Parallel()
	contracts := []Contract{{Type: reflect.TypeOf((*any)(nil)).Elem(), Origin: "declared.go:1"}}
	config := &ConfigDescriptor{Type: reflect.TypeOf(struct{}{})}
	definition := NewDefinition(DefinitionDescriptor{Key: "web", Contracts: contracts, Config: config})

	contracts[0].Origin = "mutated"
	config.Type = nil

	descriptor, ok := DefinitionDescriptorOf(definition)
	require.True(t, ok)
	assert.Equal(t, Key("web"), descriptor.Key)
	require.Len(t, descriptor.Contracts, 1)
	assert.Equal(t, "declared.go:1", descriptor.Contracts[0].Origin, "NewDefinition copies the contract slice")
	require.NotNil(t, descriptor.Config)
	assert.NotNil(t, descriptor.Config.Type, "NewDefinition copies the config descriptor")

	descriptor.Contracts[0].Origin = "caller mutation"
	descriptor.Config.Type = nil
	again, ok := DescribeDefinition(definition)
	require.True(t, ok)
	assert.Equal(t, "declared.go:1", again.Contracts[0].Origin, "DescribeDefinition returns a defensive copy")
	assert.NotNil(t, again.Config.Type)
}

func TestSameDefinitionComparesHandleIdentityNotMetadata(t *testing.T) {
	t.Parallel()
	descriptor := DefinitionDescriptor{Key: "web"}
	first := NewDefinition(descriptor)
	second := NewDefinition(descriptor)

	assert.True(t, SameDefinition(first, first))
	assert.False(t, SameDefinition(first, second), "equal metadata does not make two handles identical")

	var zero Definition
	assert.False(t, SameDefinition(zero, zero), "a zero handle never matches")
	_, ok := DescribeDefinition(zero)
	assert.False(t, ok)
}

func TestBundlesCombineWithoutAliasingAndRetainOrigin(t *testing.T) {
	t.Parallel()
	first := NewDefinition(DefinitionDescriptor{Key: "a"})
	second := NewDefinition(DefinitionDescriptor{Key: "b"})

	left := NewBundle("left.go:1", first)
	right := NewBundle("right.go:1", second)
	combined := CombineBundles(left, right)

	entries := BundleEntries(combined)
	require.Len(t, entries, 2)
	assert.True(t, SameDefinition(first, entries[0].Definition))
	assert.Equal(t, "left.go:1", entries[0].Origin)
	assert.True(t, SameDefinition(second, entries[1].Definition))
	assert.Equal(t, "right.go:1", entries[1].Origin)

	entries[0].Origin = "mutated"
	assert.Equal(t, "left.go:1", BundleEntries(combined)[0].Origin, "BundleEntries returns a defensive copy")

	assert.Empty(t, BundleEntries(Bundle{}))
	assert.Len(t, BundleEntries(CombineBundles(left, left)), 2, "combining does not deduplicate; freezing does")
}

func TestWorkloadKeyValidatesLikeAPluginKey(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"sast", "coderanger", "web-scan_2"} {
		assert.NoError(t, WorkloadKey(value).Validate(), value)
		assert.Equal(t, value, WorkloadKey(value).String())
	}
	assert.EqualError(t, WorkloadKey("").Validate(), "xbc: workload key cannot be empty")

	err := WorkloadKey("Sast").Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workload key")
	assert.Contains(t, err.Error(), "contains invalid character")
}

func TestAssignWorkloadTagsOccurrencesAndDeclaresTheWorkload(t *testing.T) {
	t.Parallel()
	member := NewDefinition(DefinitionDescriptor{Key: "sast-worker"})
	unowned := NewDefinition(DefinitionDescriptor{Key: "web"})
	base := CombineBundles(NewBundle("base.go:1", member), NewBundle("base.go:2", unowned))

	assigned := AssignWorkload(base, Workload{Key: "sast", Exclusive: true, Replicas: 3})

	assert.Equal(t, []Workload{{Key: "sast", Exclusive: true, Replicas: 3}}, BundleWorkloads(assigned))
	for _, entry := range BundleEntries(assigned) {
		assert.Equal(t, WorkloadKey("sast"), entry.Workload)
	}
	assert.Empty(t, BundleWorkloads(base), "assigning returns a new Bundle rather than mutating the one given")
	for _, entry := range BundleEntries(base) {
		assert.Equal(t, WorkloadKey(""), entry.Workload)
	}

	entries := BundleEntries(assigned)
	entries[0].Workload = "mutated"
	assert.Equal(t, WorkloadKey("sast"), BundleEntries(assigned)[0].Workload, "BundleEntries returns a defensive copy")
}

func TestCombineBundlesCarriesWorkloadDeclarationsAlongsideOccurrences(t *testing.T) {
	t.Parallel()
	member := NewDefinition(DefinitionDescriptor{Key: "worker"})
	workload := AssignWorkload(NewBundle("workload.go:1", member), Workload{Key: "sast", Replicas: 2})

	combined := CombineBundles(NewBundle("plain.go:1", NewDefinition(DefinitionDescriptor{Key: "web"})), workload)

	assert.Equal(t, []Workload{{Key: "sast", Replicas: 2}}, BundleWorkloads(combined))
	require.Len(t, BundleEntries(combined), 2)
	assert.True(t, SameDefinition(member, BundleEntries(combined)[1].Definition))
	assert.Equal(t, WorkloadKey("sast"), BundleEntries(combined)[1].Workload)
	assert.Equal(t, WorkloadKey(""), BundleEntries(combined)[0].Workload)

	workloads := BundleWorkloads(combined)
	workloads[0].Replicas = 99
	assert.Equal(t, 2, BundleWorkloads(combined)[0].Replicas, "BundleWorkloads returns a defensive copy")
}

// TestBundleWorkloadsReportsEachKeyOnceFromItsFirstOccurrence pins the
// behaviour that lets a diagnostic read declarations without having to decide
// what a conflict means first. Conflict detection belongs to ValidateWorkloads,
// which can report it, and this accessor must stay total.
func TestBundleWorkloadsReportsEachKeyOnceFromItsFirstOccurrence(t *testing.T) {
	t.Parallel()
	member := NewDefinition(DefinitionDescriptor{Key: "worker"})
	leading := AssignWorkload(NewBundle("leading.go:1", member), Workload{Key: "sast", Exclusive: true, Replicas: 4})
	trailing := AssignWorkload(NewBundle("trailing.go:1", member), Workload{Key: "sast", Replicas: 1})
	alpha := AssignWorkload(NewBundle("alpha.go:1", member), Workload{Key: "alpha", Replicas: 1})

	workloads := BundleWorkloads(CombineBundles(leading, alpha, trailing))
	require.Len(t, workloads, 2)
	assert.Equal(t, []WorkloadKey{"alpha", "sast"}, []WorkloadKey{workloads[0].Key, workloads[1].Key},
		"declarations are sorted by key and a repeated key is reported once")
	assert.Equal(t, Workload{Key: "sast", Exclusive: true, Replicas: 4}, workloads[1],
		"the occurrence that comes first in Bundle order wins, so the result never depends on how a caller ordered its Bundles")

	assert.Nil(t, BundleWorkloads(Bundle{}))
}

// TestValidateWorkloadsAcceptsAConsistentComposition is the positive control
// for every rejection below: without it a validator that rejected everything
// would look like a passing guard.
func TestValidateWorkloadsAcceptsAConsistentComposition(t *testing.T) {
	t.Parallel()
	member := NewDefinition(DefinitionDescriptor{Key: "sast-worker"})
	remote := NewDefinition(DefinitionDescriptor{Key: "remote-worker"})
	plain := NewDefinition(DefinitionDescriptor{Key: "web"})

	workload := AssignWorkload(NewBundle("workload.go:1", member), Workload{Key: "sast", Replicas: 3})
	escapeHatch := NewBundle("remote.go:1", remote)

	require.NoError(t, ValidateWorkloads(
		[]Bundle{workload, escapeHatch, NewBundle("plain.go:1", plain)},
		map[Definition]WorkloadKey{remote: "sast"},
	))

	require.NoError(t, ValidateWorkloads([]Bundle{workload}, nil),
		"a Definition that names no workload needs no entry in the map")

	// The same key declared identically twice, in two Bundles, is one
	// declaration rather than a conflict.
	require.NoError(t, ValidateWorkloads(
		[]Bundle{workload, AssignWorkload(NewBundle("again.go:1", member), Workload{Key: "sast", Replicas: 3})},
		nil,
	))
}

func TestValidateWorkloadsRejectsDeclarationConflicts(t *testing.T) {
	t.Parallel()
	member := NewDefinition(DefinitionDescriptor{Key: "sast-worker"})
	remote := NewDefinition(DefinitionDescriptor{Key: "remote-worker"})
	other := NewDefinition(DefinitionDescriptor{Key: "other-worker"})
	sast := Workload{Key: "sast", Replicas: 3}

	for name, testCase := range map[string]struct {
		bundles    []Bundle
		definition map[Definition]WorkloadKey
		message    string
	}{
		"one Definition in two workloads": {
			bundles: []Bundle{
				AssignWorkload(NewBundle("a.go:1", member), sast),
				AssignWorkload(NewBundle("b.go:1", member), Workload{Key: "coderanger", Replicas: 1}),
			},
			message: `xbc: plugin "sast-worker" belongs to workload "sast" and workload "coderanger"; a Definition belongs to at most one workload`,
		},
		"escape hatch disagrees with the occurrence": {
			bundles:    []Bundle{AssignWorkload(NewBundle("a.go:1", remote), sast)},
			definition: map[Definition]WorkloadKey{remote: "coderanger"},
			message:    `xbc: plugin "remote-worker" declares workload "coderanger" on itself but its Bundle occurrence belongs to workload "sast"; declare the ownership once`,
		},
		"conflicting redeclaration of one key": {
			bundles: []Bundle{
				AssignWorkload(NewBundle("a.go:1", member), sast),
				AssignWorkload(NewBundle("b.go:1", other), Workload{Key: "sast", Exclusive: true, Replicas: 3}),
			},
			message: `xbc: workload "sast" is declared twice with different placement; first: exclusive=false replicas=3, second: exclusive=true replicas=3`,
		},
		"escape hatch names an undeclared workload": {
			bundles:    []Bundle{NewBundle("a.go:1", remote)},
			definition: map[Definition]WorkloadKey{remote: "sast"},
			message:    `xbc: plugin "remote-worker" belongs to workload "sast", which nothing in the composition declares; declare it with plugin.WorkloadOf`,
		},
		"an unowned Definition does not rescue a declared one": {
			bundles:    []Bundle{NewBundle("a.go:1", member), NewBundle("b.go:1", other)},
			definition: map[Definition]WorkloadKey{member: "sast"},
			message:    `xbc: plugin "sast-worker" belongs to workload "sast", which nothing in the composition declares; declare it with plugin.WorkloadOf`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ValidateWorkloads(testCase.bundles, testCase.definition)
			require.Error(t, err)
			assert.EqualError(t, err, testCase.message)
		})
	}
}

// TestValidateWorkloadsRejectsATaggedOccurrenceWithNoDeclaration covers the
// shape AssignWorkload cannot produce -- a tagged entry whose declaration is
// absent from the composition -- by constructing the Bundle directly. Keeping
// the check despite that is deliberate: it is the invariant, not the
// constructor, that assembly relies on, so a future tagging path must not be
// able to introduce an unexplained membership silently.
func TestValidateWorkloadsRejectsATaggedOccurrenceWithNoDeclaration(t *testing.T) {
	t.Parallel()
	member := NewDefinition(DefinitionDescriptor{Key: "sast-worker"})
	tagged := Bundle{entries: []BundleEntry{
		{Definition: member, Origin: "handwritten.go:1", Workload: "sast"},
	}}

	err := ValidateWorkloads([]Bundle{tagged}, nil)
	require.Error(t, err)
	assert.EqualError(t, err,
		`xbc: plugin "sast-worker" belongs to workload "sast", which nothing in the composition declares; declare it with plugin.WorkloadOf`)
}

// TestValidateWorkloadsNamesAZeroDefinition keeps the one input that has no
// key to print from producing a message with a blank where the subject is.
func TestValidateWorkloadsNamesAZeroDefinition(t *testing.T) {
	t.Parallel()
	var unbuilt Definition
	tagged := Bundle{entries: []BundleEntry{
		{Definition: unbuilt, Origin: "handwritten.go:1", Workload: "sast"},
	}}

	err := ValidateWorkloads([]Bundle{tagged}, nil)
	require.Error(t, err)
	assert.EqualError(t, err,
		`xbc: a zero Definition belongs to workload "sast", which nothing in the composition declares; declare it with plugin.WorkloadOf`)

	declaring := AssignWorkload(NewBundle("c.go:1", unbuilt), Workload{Key: "sast", Replicas: 1})
	require.NoError(t, ValidateWorkloads([]Bundle{declaring}, nil))
}

func TestBuildContextExposesIdentityAndCopiesSlots(t *testing.T) {
	t.Parallel()
	token := NewInputToken(QueryMany, reflect.TypeOf(0), "", "", "")
	slots := map[uint64][]ResolvedEntry{
		token.ID: {{Identity: Identity{Plugin: "producer"}, Value: 1}},
	}
	context := NewFactoryContext(Identity{Plugin: "consumer"}, nil, slots)

	assert.Equal(t, Identity{Plugin: "consumer", Instance: DefaultInstance}, context.Identity())
	assert.NotNil(t, context.Log(), "a nil logger falls back to the process logger")

	slots[token.ID][0].Value = 2
	entries := ReadBuildSlot(context, token)
	require.Len(t, entries, 1)
	assert.Equal(t, 1, entries[0].Value, "NewBuildContext copies the slot map")

	entries[0].Value = 3
	assert.Equal(t, 1, ReadBuildSlot(context, token)[0].Value, "ReadBuildSlot returns a defensive copy")
}

func TestBuildContextRejectsUndeclaredTokens(t *testing.T) {
	t.Parallel()
	declared := NewInputToken(QueryMany, reflect.TypeOf(0), "", "", "")
	undeclared := NewInputToken(QueryMany, reflect.TypeOf(0), "", "", "")
	context := NewBuildContext(Identity{Plugin: "consumer"}, nil, map[uint64][]ResolvedEntry{declared.ID: nil})

	assert.PanicsWithValue(t,
		"xbc: plugin consumer used undeclared input token "+strconv.FormatUint(undeclared.ID, 10)+" (int)",
		func() { ReadBuildSlot(context, undeclared) })
}

func TestZeroBuildContextPanicsWithTheAttemptedOperation(t *testing.T) {
	t.Parallel()
	var zero BuildContext
	assert.PanicsWithValue(t, "xbc: zero BuildContext cannot read identity", func() { _ = zero.Identity() })
	assert.PanicsWithValue(t, "xbc: zero BuildContext cannot read logger", func() { _ = zero.Log() })
	assert.PanicsWithValue(t, "xbc: zero BuildContext cannot read input", func() {
		_ = ReadBuildSlot(zero, InputToken{})
	})
	assert.NotPanics(t, func() { InvalidateBuildContext(zero) })
}

func TestInvalidatedBuildContextRejectsEveryReadThroughEveryCopy(t *testing.T) {
	t.Parallel()
	token := NewInputToken(QueryMany, reflect.TypeOf(0), "", "", "")
	context := NewBuildContext(Identity{Plugin: "late", Instance: "readonly"}, nil, map[uint64][]ResolvedEntry{token.ID: nil})
	escaped := context

	InvalidateBuildContext(context)
	InvalidateBuildContext(context)

	const message = "xbc: plugin late[readonly] used BuildContext after its factory returned"
	assert.PanicsWithValue(t, message, func() { _ = escaped.Identity() })
	assert.PanicsWithValue(t, message, func() { _ = escaped.Log() })
	assert.PanicsWithValue(t, message, func() { _ = ReadBuildSlot(escaped, token) })
}
