package session

import (
	"context"
	"reflect"
	"testing"

	"github.com/xbcio/xbc/extensions/authentication"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// legacyManager implements exactly the published Manager method set. It is a
// compile-time regression guard: if Manager ever gains a method, this type
// stops satisfying it, which is the same breakage an external implementation
// would see.
type legacyManager struct{}

func (legacyManager) Create(context.Context, string, map[string]any) (Session, error) {
	return Session{}, nil
}

func (legacyManager) Rotate(context.Context, string) (Session, error) { return Session{}, nil }

func (legacyManager) Revoke(context.Context, string) error { return nil }

func (legacyManager) ListBySubject(context.Context, string) ([]SessionInfo, error) {
	return nil, nil
}

func (legacyManager) RevokeBySubject(context.Context, string) (int, error) { return 0, nil }

func (legacyManager) RevokeByDigest(context.Context, string, string) (bool, error) {
	return false, nil
}

var (
	_ Manager          = legacyManager{}
	_ AttributeManager = (*manager)(nil)
	_ AttributeManager = (*Plugin)(nil)
)

// TestManagerKeepsItsPublishedMethodSet pins the method set a store or test
// double implemented before AttributeManager existed. Widening Manager is a
// breaking change for every external implementation, so it must be deliberate.
func TestManagerKeepsItsPublishedMethodSet(t *testing.T) {
	want := []string{"Create", "ListBySubject", "Revoke", "RevokeByDigest", "RevokeBySubject", "Rotate"}
	typ := reflect.TypeOf((*Manager)(nil)).Elem()
	got := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Manager methods = %v, want the published set %v", got, want)
	}
}

// TestAttributeManagerAddsOnlyAttributeMaintenanceAndVerify keeps the extended
// contract limited to the three capabilities it was introduced for.
func TestAttributeManagerAddsOnlyAttributeMaintenanceAndVerify(t *testing.T) {
	base := reflect.TypeOf((*Manager)(nil)).Elem()
	extended := reflect.TypeOf((*AttributeManager)(nil)).Elem()
	baseMethods := make(map[string]bool, base.NumMethod())
	for i := range base.NumMethod() {
		baseMethods[base.Method(i).Name] = true
	}
	var added []string
	for i := range extended.NumMethod() {
		name := extended.Method(i).Name
		if !baseMethods[name] {
			added = append(added, name)
		}
	}
	want := []string{"PatchAttributes", "UpdateAttributes", "Verify"}
	if !reflect.DeepEqual(added, want) {
		t.Fatalf("AttributeManager methods beyond Manager = %v, want %v", added, want)
	}
}

// TestDefinitionExportsBothManagerContracts proves the graph exposes the
// original Manager behavior and the extended capabilities side by side.
func TestDefinitionExportsBothManagerContracts(t *testing.T) {
	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	want := []reflect.Type{
		reflect.TypeOf((*authentication.Authenticator)(nil)).Elem(),
		reflect.TypeOf((*Manager)(nil)).Elem(),
		reflect.TypeOf((*AttributeManager)(nil)).Elem(),
	}
	var got []reflect.Type
	for _, contract := range descriptor.Contracts {
		got = append(got, contract.Type)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Definition contracts = %v, want %v", got, want)
	}
}
