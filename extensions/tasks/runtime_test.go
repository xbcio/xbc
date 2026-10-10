package tasks

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// The executor tests share the two process-wide install slots, so none of them
// runs in parallel and every install is undone in the test that made it.

// fakeLocal is a Local that records what was routed to it. Installed
// executors are only ever driven through the package's own entry points, so it
// mimics behavior -- reject, record -- rather than execution.
type fakeLocal struct {
	mu        sync.Mutex
	names     []string
	payloads  [][]byte
	bindings  map[string]Binding
	goErr     error
	submitErr error
}

func newFakeLocal() *fakeLocal {
	return &fakeLocal{bindings: make(map[string]Binding)}
}

func (f *fakeLocal) Go(ctx context.Context, name string, fn func(context.Context)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.goErr != nil {
		return f.goErr
	}
	f.names = append(f.names, name)
	return nil
}

func (f *fakeLocal) Submit(ctx context.Context, name string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return f.submitErr
	}
	f.names = append(f.names, name)
	f.payloads = append(f.payloads, payload)
	return nil
}

func (f *fakeLocal) Lookup(name string) (Binding, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	binding, ok := f.bindings[name]
	return binding, ok
}

func (f *fakeLocal) submissions() ([]string, [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...), append([][]byte(nil), f.payloads...)
}

// fakeRemote is a Remote recording submissions the same way.
type fakeRemote struct {
	mu        sync.Mutex
	names     []string
	payloads  [][]byte
	bindings  map[string]Binding
	submitErr error
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{bindings: make(map[string]Binding)}
}

func (f *fakeRemote) Submit(ctx context.Context, name string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return f.submitErr
	}
	f.names = append(f.names, name)
	f.payloads = append(f.payloads, payload)
	return nil
}

func (f *fakeRemote) Lookup(name string) (Binding, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	binding, ok := f.bindings[name]
	return binding, ok
}

func (f *fakeRemote) submissions() ([]string, [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.names...), append([][]byte(nil), f.payloads...)
}

func TestInstallLocalIsFirstComeFirstServed(t *testing.T) {
	first := newFakeLocal()
	uninstallFirst, installed := InstallLocal(first)
	if !installed {
		t.Fatal("installing into an empty slot reported installed=false")
	}
	defer uninstallFirst()

	second := newFakeLocal()
	uninstallSecond, installed := InstallLocal(second)
	if installed {
		t.Fatal("installing over a bound executor reported installed=true")
	}
	uninstallSecond()

	if err := testInt.Submit(context.Background(), 1); err != nil {
		t.Fatalf("Submit returned %v, want nil", err)
	}
	firstNames, _ := first.submissions()
	secondNames, _ := second.submissions()
	if len(firstNames) != 1 || firstNames[0] != "test.int" {
		t.Fatalf("bound executor received %v, want one test.int submission", firstNames)
	}
	if len(secondNames) != 0 {
		t.Fatalf("rejected executor received %v, want nothing", secondNames)
	}

	uninstallFirst()
	uninstallFirst() // uninstalling twice is not an error and must not panic
	uninstallReinstalled, installed := InstallLocal(second)
	if !installed {
		t.Fatal("the slot did not accept an executor again after uninstall")
	}
	defer uninstallReinstalled()
	if err := Go(context.Background(), func(context.Context) {}); err != nil {
		t.Fatalf("Go returned %v after reinstalling, want nil", err)
	}
}

func TestInstallRemoteIsIndependentOfLocal(t *testing.T) {
	local := newFakeLocal()
	uninstallLocal, installed := InstallLocal(local)
	if !installed {
		t.Fatal("installing the local executor reported installed=false")
	}
	defer uninstallLocal()
	remote := newFakeRemote()
	uninstallRemote, installed := InstallRemote(remote)
	if !installed {
		t.Fatal("installing the remote executor reported installed=false")
	}
	defer uninstallRemote()

	uninstallLocal()
	if err := Go(context.Background(), func(context.Context) {}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Go returned %v after the local executor was uninstalled, want ErrNotInstalled", err)
	}
	if err := testInt.Submit(context.Background(), 2); err != nil {
		t.Fatalf("Submit returned %v, want nil: the remote executor is still installed", err)
	}
	names, _ := remote.submissions()
	if len(names) != 1 {
		t.Fatalf("remote executor received %v, want one submission", names)
	}
}

func TestAStaleUninstallDoesNotRemoveASuccessor(t *testing.T) {
	first := newFakeLocal()
	uninstallFirst, _ := InstallLocal(first)
	uninstallFirst()

	second := newFakeLocal()
	uninstallSecond, installed := InstallLocal(second)
	if !installed {
		t.Fatal("installing after an uninstall reported installed=false")
	}
	defer uninstallSecond()

	uninstallFirst() // the old executor's uninstall must not touch the successor
	if err := Go(context.Background(), func(context.Context) {}); err != nil {
		t.Fatalf("Go returned %v after a stale uninstall, want nil: the successor must stay installed", err)
	}
}

func TestSubmitPrefersTheRemoteExecutor(t *testing.T) {
	local := newFakeLocal()
	uninstallLocal, _ := InstallLocal(local)
	defer uninstallLocal()
	remote := newFakeRemote()
	uninstallRemote, _ := InstallRemote(remote)
	defer uninstallRemote()

	if err := testInt.Submit(context.Background(), 5); err != nil {
		t.Fatalf("Submit returned %v, want nil", err)
	}
	remoteNames, remotePayloads := remote.submissions()
	localNames, _ := local.submissions()
	if len(remoteNames) != 1 {
		t.Fatalf("remote executor received %v, want one submission", remoteNames)
	}
	if len(localNames) != 0 {
		t.Fatalf("local executor received %v although the remote executor is installed", localNames)
	}
	if string(remotePayloads[0]) != "5" {
		t.Fatalf("remote payload %q, want 5", string(remotePayloads[0]))
	}

	remote.submitErr = errors.New("remote refused")
	if err := testInt.Submit(context.Background(), 6); err == nil || !strings.Contains(err.Error(), "remote refused") {
		t.Fatalf("Submit returned %v, want the remote executor's error", err)
	}
	localNames, _ = local.submissions()
	if len(localNames) != 0 {
		t.Fatalf("local executor received %v after a remote failure; submission must not fall back", localNames)
	}
}

func TestGoNamesTheFunction(t *testing.T) {
	local := newFakeLocal()
	uninstall, _ := InstallLocal(local)
	defer uninstall()

	if err := Go(context.Background(), func(context.Context) {}); err != nil {
		t.Fatalf("Go returned %v, want nil", err)
	}
	if err := Go(context.Background(), func(context.Context) {}, Named("custom.name")); err != nil {
		t.Fatalf("Go returned %v, want nil", err)
	}
	names, _ := local.submissions()
	if len(names) != 2 {
		t.Fatalf("executor saw %v, want two Go names", names)
	}
	if !strings.Contains(names[0], "TestGoNamesTheFunction") {
		t.Fatalf("Go derived the name %q, want it to name the test function", names[0])
	}
	if names[1] != "custom.name" {
		t.Fatalf("Go used the name %q, want the Named override", names[1])
	}
}

func TestGoWithoutAnExecutorIsNotInstalled(t *testing.T) {
	if err := Go(context.Background(), func(context.Context) {}); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Go returned %v, want ErrNotInstalled", err)
	}
	mustPanic(t, "requires a function", func() { Go(context.Background(), nil) })
	mustPanic(t, "must not be empty", func() { Named("") })
	mustPanic(t, "leading or trailing whitespace", func() { Named(" named") })
}

func TestMethodRunLooksUpLocalThenRemote(t *testing.T) {
	localRecord := &callRecord{}
	remoteRecord := &callRecord{}
	local := newFakeLocal()
	local.bindings["test.confirm"] = Bind(&testService{record: localRecord}, testConfirm)[0]
	remote := newFakeRemote()
	remote.bindings["test.confirm"] = Bind(&testService{record: remoteRecord}, testConfirm)[0]

	uninstallLocal, _ := InstallLocal(local)
	uninstallRemote, _ := InstallRemote(remote)
	defer uninstallLocal()
	defer uninstallRemote()

	if err := testConfirm.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if len(localRecord.calls) != 1 || localRecord.calls[0] != "hello" {
		t.Fatalf("local binding saw %v, want one call with hello", localRecord.calls)
	}
	if len(remoteRecord.calls) != 0 {
		t.Fatalf("remote binding saw %v although the local executor holds the task", remoteRecord.calls)
	}

	// With the local executor gone, Run keeps working through the remote one:
	// the method task's plugin is still installed, and it is the remote
	// executor's table that now answers.
	uninstallLocal()
	if err := testConfirm.Run(context.Background(), "again"); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if len(remoteRecord.calls) != 1 || remoteRecord.calls[0] != "again" {
		t.Fatalf("remote binding saw %v, want one call with again", remoteRecord.calls)
	}

	// Run passes the caller's context through the binding, so a canceled
	// context reaches the method.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := testConfirm.Run(ctx, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}
