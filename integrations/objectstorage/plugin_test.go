package objectstorage

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xbcconfig "github.com/xbcio/xbc/config"
	"github.com/xbcio/xbc/internal/assembly"
	"github.com/xbcio/xbc/internal/pluginmodel"
	"github.com/xbcio/xbc/plugin"
)

func TestDefinitionIsCanonicalAndBundleContainsIt(t *testing.T) {
	first := Definition()
	second := Definition()
	if first != second {
		t.Fatal("Definition() returned different declaration handles")
	}

	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(Bundle()))
	if len(entries) != 1 {
		t.Fatalf("Bundle() entries = %d, want 1", len(entries))
	}
	if !pluginmodel.SameDefinition(entries[0].Definition, pluginmodel.Definition(first)) {
		t.Fatal("Bundle() does not contain the canonical Definition handle")
	}

	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(first))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	if descriptor.Key != pluginmodel.Key(Key) || descriptor.Cardinality != pluginmodel.MultipleInstances {
		t.Fatalf("Definition descriptor = %+v", descriptor)
	}
	if descriptor.Activation.Kind != pluginmodel.ActivationConfigured || descriptor.Activation.Path != "plugins.objectstorage" {
		t.Fatalf("Definition activation = %+v", descriptor.Activation)
	}
	if descriptor.Primary != reflect.TypeOf((*managedStore)(nil)) {
		t.Fatalf("Definition primary = %v, want *managedStore", descriptor.Primary)
	}
	storeType := reflect.TypeOf((*Store)(nil)).Elem()
	if len(descriptor.Contracts) != 1 || descriptor.Contracts[0].Type != storeType {
		t.Fatalf("Definition contracts = %+v, want %v", descriptor.Contracts, storeType)
	}
	if descriptor.Config == nil || descriptor.Config.Type != reflect.TypeOf(Config{}) {
		t.Fatalf("Definition config = %+v", descriptor.Config)
	}
	if descriptor.Lifecycle.Stop == nil || descriptor.Lifecycle.Init != nil || descriptor.Lifecycle.Start != nil {
		t.Fatalf("Definition lifecycle = %+v", descriptor.Lifecycle)
	}
}

func TestDefinitionPlansWithoutConstructingAndDeduplicatesBundle(t *testing.T) {
	directory := t.TempDir()
	environment := objectStorageEnvironment(t, map[string]any{
		"assets": map[string]any{
			"local": map[string]any{"directory": directory},
		},
	})

	plan, err := assembly.BuildPlan(assembly.PlanOptions{
		Bundles: []plugin.Bundle{Bundle(), Bundle()},
		Env:     environment,
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	if plan.DefinitionCount() != 1 {
		t.Fatalf("DefinitionCount() = %d, want 1", plan.DefinitionCount())
	}
	identity := plugin.Identity{Plugin: Key, Instance: "assets"}
	if got := plan.Order(); len(got) != 1 || got[0] != identity {
		t.Fatalf("plan order = %+v, want [%v]", got, identity)
	}
	storeType := reflect.TypeOf((*Store)(nil)).Elem()
	if got := plan.Contracts(storeType); len(got) != 1 || got[0] != identity {
		t.Fatalf("Store contracts = %+v, want [%v]", got, identity)
	}

	constructed, err := assembly.Construct(plan, assembly.ConstructOptions{})
	if err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	instance, found := constructed.Instance(identity)
	if !found {
		t.Fatalf("constructed instance %v not found", identity)
	}
	store, ok := instance.Primary().(Store)
	if !ok {
		t.Fatalf("primary value has type %T, want Store", instance.Primary())
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.Put(context.Background(), "docs/readme.txt", strings.NewReader("hello"), PutOptions{Size: 5}); err != nil {
		t.Fatalf("Store.Put() error = %v", err)
	}

	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := constructed.Unwind(shutdown, time.Second, nil); err != nil {
		t.Fatalf("Unwind() error = %v", err)
	}
	if _, err := store.Stat(context.Background(), "docs/readme.txt"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Store after Unwind error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("repeated Store.Close() error = %v", err)
	}
}

func TestDefinitionRejectsInvalidPreparedConfiguration(t *testing.T) {
	environment := objectStorageEnvironment(t, map[string]any{
		"default": map[string]any{"backend": BackendS3},
	})
	_, err := assembly.BuildPlan(assembly.PlanOptions{Bundles: []plugin.Bundle{Bundle()}, Env: environment})
	if err == nil || !strings.Contains(err.Error(), "s3.bucket") {
		t.Fatalf("BuildPlan() error = %v, want missing S3 bucket", err)
	}
}

func TestStopStoreTimeoutSharesCleanupAndFinalCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	backend := &blockingStore{
		closeStarted: make(chan struct{}),
		closeRelease: make(chan struct{}),
		closeErr:     closeErr,
	}
	store := &managedStore{backend: backend}

	short, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { firstDone <- stopStore(store, short) }()
	select {
	case <-backend.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("shared close did not start")
	}
	if err := <-firstDone; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed stopStore() error = %v", err)
	}

	later := make(chan error, 1)
	go func() { later <- store.Close() }()
	select {
	case err := <-later:
		t.Fatalf("later Close returned before shared cleanup: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(backend.closeRelease)
	if err := <-later; !errors.Is(err, closeErr) {
		t.Fatalf("later Close() error = %v", err)
	}
	if calls := backend.closeCalls.Load(); calls != 1 {
		t.Fatalf("backend Close calls = %d, want 1", calls)
	}
	if err := stopStore(store, context.Background()); !errors.Is(err, closeErr) {
		t.Fatalf("repeated stopStore() error = %v", err)
	}
}

func TestStopStoreBeforeBackendIsNoopAndIdempotent(t *testing.T) {
	store := new(managedStore)
	if err := stopStore(store, nil); err != nil {
		t.Fatalf("stopStore before backend error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close before backend error = %v", err)
	}
	if err := stopStore(nil, context.Background()); err != nil {
		t.Fatalf("stopStore(nil) error = %v", err)
	}
}

func objectStorageEnvironment(t *testing.T, instances map[string]any) *xbcconfig.Environment {
	t.Helper()
	environment, err := xbcconfig.NewEnvironment(map[string]any{
		"plugins": map[string]any{
			"objectstorage": instances,
		},
	}, "XBC_OBJECTSTORAGE_PLUGIN_TEST_")
	if err != nil {
		t.Fatalf("NewEnvironment() error = %v", err)
	}
	return environment
}

type blockingStore struct {
	closeStarted chan struct{}
	closeRelease chan struct{}
	closeErr     error
	closeCalls   atomic.Int32
}

func (*blockingStore) Put(context.Context, string, io.Reader, PutOptions) (ObjectInfo, error) {
	return ObjectInfo{}, nil
}
func (*blockingStore) Get(context.Context, string) (*Object, error) { return nil, nil }
func (*blockingStore) Delete(context.Context, string) error         { return nil }
func (*blockingStore) Stat(context.Context, string) (ObjectInfo, error) {
	return ObjectInfo{}, nil
}
func (*blockingStore) List(context.Context, ListOptions) (ListResult, error) {
	return ListResult{}, nil
}
func (s *blockingStore) Close() error {
	if s.closeCalls.Add(1) == 1 {
		close(s.closeStarted)
	}
	<-s.closeRelease
	return s.closeErr
}
