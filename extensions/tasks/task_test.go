package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// callRecord is the observation channel between the package-level task
// definitions below and the tests: a test installs one in the context and the
// handler records the argument and context error it saw.
type callRecord struct {
	calls []any
	errs  []error
}

// callRecordKey is the context key of the callRecord, a defined type so no
// other package's key can collide with it.
type callRecordKey struct{}

// recordFrom returns the callRecord a test installed, or a detached one, so a
// handler invoked without the test's context still records instead of
// panicking.
func recordFrom(ctx context.Context) *callRecord {
	if record, ok := ctx.Value(callRecordKey{}).(*callRecord); ok {
		return record
	}
	return &callRecord{}
}

func (r *callRecord) observe(arg any, err error) {
	r.calls = append(r.calls, arg)
	r.errs = append(r.errs, err)
}

// withRecord returns a context carrying record.
func withRecord(record *callRecord) context.Context {
	return context.WithValue(context.Background(), callRecordKey{}, record)
}

// The handles below are package-level on purpose: New and Method claim their
// name for the process and panic on a duplicate, so a handle defined inside a
// test function would explode the second time the package's tests run in one
// binary (-count>1).

var testInt = New("test.int", func(ctx context.Context, arg int) error {
	recordFrom(ctx).observe(arg, ctx.Err())
	return ctx.Err()
})

var testPanic = New("test.panic", func(ctx context.Context, arg string) error {
	panic("test panic: " + arg)
})

var testUnencodable = New("test.unencodable", func(ctx context.Context, value chan int) error {
	return nil
})

// testService is the plugin value a method task hangs on; the record field
// lets the tests observe through the receiver.
type testService struct {
	record *callRecord
}

func (s *testService) confirm(ctx context.Context, arg string) error {
	s.record.observe(arg, ctx.Err())
	return ctx.Err()
}

func (s *testService) accept(ctx context.Context, value chan int) error {
	return nil
}

var testConfirm = Method("test.confirm", (*testService).confirm)

var testUnencodableMethod = Method("test.unencodable_method", (*testService).accept)

// testNameSequence supplies names the definition tests must define at run
// time; a per-call suffix keeps them distinct when the tests run repeatedly in
// one binary.
var testNameSequence atomic.Int64

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s.%d", prefix, testNameSequence.Add(1))
}

func TestTaskRunCallsTheFunctionDirectly(t *testing.T) {
	record := &callRecord{}
	if err := testInt.Run(withRecord(record), 7); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if len(record.calls) != 1 || record.calls[0] != 7 {
		t.Fatalf("function saw calls %v, want one call with 7", record.calls)
	}
}

func TestTaskRunPropagatesTheFunctionError(t *testing.T) {
	ctx, cancel := context.WithCancel(withRecord(&callRecord{}))
	cancel()
	if err := testInt.Run(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
}

func TestTaskRunRecoversAPanic(t *testing.T) {
	err := testPanic.Run(context.Background(), "boom")
	if err == nil {
		t.Fatal("Run returned nil for a panicking task")
	}
	for _, want := range []string{"panicked", "test panic: boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Run error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestMethodRunWithoutAnExecutorIsNoHandler(t *testing.T) {
	err := testConfirm.Run(context.Background(), "hello")
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("Run returned %v, want ErrNoHandler", err)
	}
	if !strings.Contains(err.Error(), `"test.confirm"`) {
		t.Fatalf("Run error %q does not name the task", err.Error())
	}
}

func TestTaskSubmitWithoutAnExecutorIsNotInstalled(t *testing.T) {
	if err := testInt.Submit(context.Background(), 3); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Submit returned %v, want ErrNotInstalled", err)
	}
}

func TestSubmitReportsUnencodableArguments(t *testing.T) {
	if err := testUnencodable.Submit(context.Background(), make(chan int)); !errors.Is(err, ErrPayload) {
		t.Fatalf("Submit returned %v, want ErrPayload", err)
	}
	if err := testUnencodableMethod.Submit(context.Background(), make(chan int)); !errors.Is(err, ErrPayload) {
		t.Fatalf("method Submit returned %v, want ErrPayload", err)
	}
}

func TestBindAttachesTheReceiverAndFuncsReturnsFunctionTasks(t *testing.T) {
	record := &callRecord{}
	bindings := Bind(&testService{record: record}, testConfirm)
	if len(bindings) != 1 {
		t.Fatalf("Bind returned %d bindings, want 1", len(bindings))
	}
	binding := bindings[0]
	if binding.Name() != "test.confirm" {
		t.Fatalf("binding name %q, want test.confirm", binding.Name())
	}
	if err := binding.Handler()(withRecord(record), []byte(`"hello"`)); err != nil {
		t.Fatalf("handler returned %v, want nil", err)
	}
	if len(record.calls) != 1 || record.calls[0] != "hello" {
		t.Fatalf("method saw calls %v, want one call with hello", record.calls)
	}

	funcs := Funcs()
	names := make([]string, 0, len(funcs))
	for _, fn := range funcs {
		names = append(names, fn.Name())
	}
	if !sorted(names) {
		t.Fatalf("Funcs returned %v, want names in order", names)
	}
	seen := false
	for _, fn := range funcs {
		if fn.Name() == "test.int" {
			seen = true
			intRecord := &callRecord{}
			if err := fn.Handler()(withRecord(intRecord), []byte("11")); err != nil {
				t.Fatalf("function handler returned %v, want nil", err)
			}
			if len(intRecord.calls) != 1 || intRecord.calls[0] != 11 {
				t.Fatalf("function handler saw calls %v, want one call with 11", intRecord.calls)
			}
		}
		if fn.Name() == "test.confirm" {
			t.Fatal("Funcs returned a method task; method bindings come from Providers")
		}
	}
	if !seen {
		t.Fatal("Funcs did not return the test.int function task")
	}
}

func sorted(names []string) bool {
	for index := 1; index < len(names); index++ {
		if names[index] < names[index-1] {
			return false
		}
	}
	return true
}

func TestNewPanicsOnInvalidDefinitions(t *testing.T) {
	mustPanic(t, "must not be empty", func() { New("", func(context.Context, int) error { return nil }) })
	mustPanic(t, "leading or trailing whitespace", func() { New(" padded", func(context.Context, int) error { return nil }) })
	mustPanic(t, "leading or trailing whitespace", func() { New("padded ", func(context.Context, int) error { return nil }) })
	mustPanic(t, "nil function", func() { New[int](uniqueName("test.nil"), nil) })
	mustPanic(t, "nil method", func() { Method[testService, string](uniqueName("test.nil_method"), nil) })
}

func TestDefinitionPanicsOnDuplicateNames(t *testing.T) {
	name := uniqueName("test.duplicate")
	New(name, func(context.Context, int) error { return nil })
	mustPanic(t, "already defined", func() { New(name, func(context.Context, int) error { return nil }) })
	mustPanic(t, "already defined", func() { Method(name, (*testService).confirm) })

	methodName := uniqueName("test.duplicate_method")
	Method(methodName, (*testService).confirm)
	mustPanic(t, "already defined", func() { New(methodName, func(context.Context, int) error { return nil }) })
}

func TestARejectedDefinitionLeavesTheNameFree(t *testing.T) {
	name := uniqueName("test.rejected")
	mustPanic(t, "nil function", func() { New[int](name, nil) })
	New(name, func(context.Context, int) error { return nil })
}

// mustPanic runs fn and fails the test unless it panics with a string
// containing want.
func mustPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatalf("expected a panic containing %q, got none", want)
		}
		message, ok := recovered.(string)
		if !ok {
			t.Fatalf("expected a string panic containing %q, got %T: %v", want, recovered, recovered)
		}
		if !strings.Contains(message, want) {
			t.Fatalf("panic %q does not contain %q", message, want)
		}
	}()
	fn()
}
