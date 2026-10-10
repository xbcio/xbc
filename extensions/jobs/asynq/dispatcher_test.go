package asynq

import (
	"context"
	"errors"
	"reflect"
	"runtime/pprof"
	"strings"
	"testing"

	hibiken "github.com/hibiken/asynq"

	"github.com/xbcio/xbc/plugin"
)

func TestGroupHandlersKeepsContributorOrderAndFreezesRegistrations(t *testing.T) {
	calls := []string{}
	first := &testContributor{name: "first", calls: &calls, registrations: []HandlerRegistration{{Type: "one", Handler: HandlerFunc(func(context.Context, Task) error { return nil })}}}
	second := &testContributor{name: "second", calls: &calls, registrations: []HandlerRegistration{{Type: "two", Handler: HandlerFunc(func(context.Context, Task) error { return nil })}}}
	contributors := []plugin.Entry[HandlerContributor]{
		{Identity: plugin.Identity{Plugin: "first"}, Value: first},
		{Identity: plugin.Identity{Plugin: "second", Instance: "named"}, Value: second},
	}

	groups, err := groupHandlers(contributors, nil)
	if err != nil {
		t.Fatalf("groupHandlers() error = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"first", "second"}) {
		t.Fatalf("contributor call order = %v", calls)
	}
	if len(groups) != 1 || groups[0].workload != "" {
		t.Fatalf("groups = %v, want one unowned group", workloadsOf(groups))
	}
	dispatch := groups[0].dispatcher
	if len(dispatch.handlers) != 2 || dispatch.handlers["one"] == nil || dispatch.handlers["two"] == nil {
		t.Fatalf("handlers = %v", dispatch.handlers)
	}
	first.registrations[0].Type = "changed"
	if dispatch.handlers["one"] == nil || dispatch.handlers["changed"] != nil {
		t.Fatal("dispatcher was not frozen independently of contributor registrations")
	}
}

func TestGroupHandlersSplitsContributorsByWorkload(t *testing.T) {
	valid := HandlerFunc(func(context.Context, Task) error { return nil })
	entry := func(key plugin.Key, workload plugin.WorkloadKey, types ...string) plugin.Entry[HandlerContributor] {
		registrations := make([]HandlerRegistration, 0, len(types))
		for _, taskType := range types {
			registrations = append(registrations, HandlerRegistration{Type: taskType, Handler: valid})
		}
		return plugin.Entry[HandlerContributor]{
			Identity: plugin.Identity{Plugin: key},
			Workload: workload,
			Value:    &testContributor{registrations: registrations},
		}
	}

	groups, err := groupHandlers([]plugin.Entry[HandlerContributor]{
		entry("housekeeping", "", "housekeeping"),
		entry("sast", "sast", "sast.scan"),
		entry("saas", "saas", "saas.report"),
		entry("sast-tools", "sast", "sast.cleanup"),
	}, nil)
	if err != nil {
		t.Fatalf("groupHandlers() error = %v", err)
	}
	want := []plugin.WorkloadKey{"", "sast", "saas"}
	if !reflect.DeepEqual(workloadsOf(groups), want) {
		t.Fatalf("groups = %v, want one group per workload in first-appearance order %v", workloadsOf(groups), want)
	}
	if len(groups[1].dispatcher.handlers) != 2 {
		t.Fatalf("sast handlers = %v, want both contributors' registrations", groups[1].dispatcher.handlers)
	}
	if _, ok := groups[0].dispatcher.handlers["sast.scan"]; ok {
		t.Fatal("a workload handler was registered in the unowned group")
	}
}

func TestGroupHandlersLeavesNothingBehindForAProcessThatContributesNothing(t *testing.T) {
	groups, err := groupHandlers(nil, nil)
	if err != nil {
		t.Fatalf("groupHandlers(nil) error = %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %v, want none", workloadsOf(groups))
	}

	groups, err = groupHandlers([]plugin.Entry[HandlerContributor]{
		{Identity: plugin.Identity{Plugin: "empty"}, Value: &testContributor{}},
		{Identity: plugin.Identity{Plugin: "empty-workload"}, Workload: "sast", Value: &testContributor{}},
	}, nil)
	if err != nil {
		t.Fatalf("groupHandlers(empty contributors) error = %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups = %v, want none: a worker with no handler would only fail what it fetched", workloadsOf(groups))
	}
}

func TestGroupHandlersRejectsInvalidContributorContracts(t *testing.T) {
	valid := HandlerFunc(func(context.Context, Task) error { return nil })
	var typedNil *testPointerHandler
	entry := func(key plugin.Key, contributor HandlerContributor) plugin.Entry[HandlerContributor] {
		return plugin.Entry[HandlerContributor]{Identity: plugin.Identity{Plugin: key}, Value: contributor}
	}
	tests := []struct {
		name         string
		contributors []plugin.Entry[HandlerContributor]
		want         string
	}{
		{name: "empty type", contributors: []plugin.Entry[HandlerContributor]{entry("bad", &testContributor{registrations: []HandlerRegistration{{Handler: valid}}})}, want: "invalid task type"},
		{name: "type whitespace", contributors: []plugin.Entry[HandlerContributor]{entry("bad", &testContributor{registrations: []HandlerRegistration{{Type: " task", Handler: valid}}})}, want: "invalid task type"},
		{name: "nil handler", contributors: []plugin.Entry[HandlerContributor]{entry("bad", &testContributor{registrations: []HandlerRegistration{{Type: "task"}}})}, want: "nil handler"},
		{name: "typed nil handler", contributors: []plugin.Entry[HandlerContributor]{entry("bad", &testContributor{registrations: []HandlerRegistration{{Type: "task", Handler: typedNil}}})}, want: "nil handler"},
		{name: "duplicate", contributors: []plugin.Entry[HandlerContributor]{
			entry("first", &testContributor{registrations: []HandlerRegistration{{Type: "task", Handler: valid}}}),
			entry("second", &testContributor{registrations: []HandlerRegistration{{Type: "task", Handler: valid}}}),
		}, want: "duplicate handler"},
		{name: "panic", contributors: []plugin.Entry[HandlerContributor]{entry("panic", panicContributor{})}, want: "callback panicked"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := groupHandlers(test.contributors, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("groupHandlers() error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestGroupHandlersNamesTheWorkloadInItsErrors keeps the diagnosis useful in a
// process that runs several workers: "invalid task type" alone would not say
// which workload's contributor returned it.
func TestGroupHandlersNamesTheWorkloadInItsErrors(t *testing.T) {
	valid := HandlerFunc(func(context.Context, Task) error { return nil })
	groups, err := groupHandlers([]plugin.Entry[HandlerContributor]{
		{Identity: plugin.Identity{Plugin: "sast"}, Workload: "sast", Value: &testContributor{registrations: []HandlerRegistration{{Type: "sast.scan", Handler: valid}}}},
		{Identity: plugin.Identity{Plugin: "sast-tools"}, Workload: "sast", Value: &testContributor{registrations: []HandlerRegistration{{Type: "sast.scan", Handler: valid}}}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), `in workload "sast"`) {
		t.Fatalf("groupHandlers() error = %v, groups = %v", err, workloadsOf(groups))
	}

	_, err = groupHandlers([]plugin.Entry[HandlerContributor]{
		{Identity: plugin.Identity{Plugin: "first"}, Value: &testContributor{registrations: []HandlerRegistration{{Type: "task", Handler: valid}}}},
		{Identity: plugin.Identity{Plugin: "second"}, Value: &testContributor{registrations: []HandlerRegistration{{Type: "task", Handler: valid}}}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "in the unowned group") {
		t.Fatalf("groupHandlers() error = %v, want the unowned group named", err)
	}
}

// TestDispatcherFilesEachTaskUnderItsWorkloadLabel pins the attribution a CPU
// profile is read by once one process runs several workloads' workers.
//
// The label is the same word the runtime files its managed tasks under, and
// that is the point: a reader joining a profile by workload cannot join two
// spellings of it. The literal below is therefore part of the test -- the
// runtime's own label test writes the same string -- and the unowned group is
// asserted to stay unlabelled for the reason the runtime leaves unowned
// plugins unlabelled.
func TestDispatcherFilesEachTaskUnderItsWorkloadLabel(t *testing.T) {
	type observation struct {
		value   string
		present bool
	}
	observations := make(chan observation, 2)
	sastErr := errors.New("retry the scan")
	handlerFor := func(err error) Handler {
		return HandlerFunc(func(ctx context.Context, _ Task) error {
			value, present := pprof.Label(ctx, "workload")
			observations <- observation{value: value, present: present}
			return err
		})
	}
	entry := func(key plugin.Key, workload plugin.WorkloadKey, taskType string, handler Handler) plugin.Entry[HandlerContributor] {
		return plugin.Entry[HandlerContributor]{
			Identity: plugin.Identity{Plugin: key},
			Workload: workload,
			Value:    &testContributor{registrations: []HandlerRegistration{{Type: taskType, Handler: handler}}},
		}
	}

	groups, err := groupHandlers([]plugin.Entry[HandlerContributor]{
		entry("scanner", "sast", "sast.scan", handlerFor(sastErr)),
		entry("housekeeping", "", "housekeeping", handlerFor(nil)),
	}, nil)
	if err != nil {
		t.Fatalf("groupHandlers() error = %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %v, want one group per workload", workloadsOf(groups))
	}

	if err := groups[0].dispatcher.ProcessTask(context.Background(), hibiken.NewTask("sast.scan", nil)); !errors.Is(err, sastErr) {
		t.Fatalf("ProcessTask() error = %v, want the handler's own error through the labelling wrapper", err)
	}
	if got := <-observations; got.value != "sast" || !got.present {
		t.Fatalf("a task of workload sast observed label %q/%v, want it filed under its workload", got.value, got.present)
	}
	if err := groups[1].dispatcher.ProcessTask(context.Background(), hibiken.NewTask("housekeeping", nil)); err != nil {
		t.Fatalf("ProcessTask() error = %v", err)
	}
	if got := <-observations; got.present || got.value != "" {
		t.Fatalf("a task of the unowned group observed label %q/%v, want it left unlabelled like every shared contributor", got.value, got.present)
	}
}

func workloadsOf(groups []handlerGroup) []plugin.WorkloadKey {
	workloads := make([]plugin.WorkloadKey, 0, len(groups))
	for _, group := range groups {
		workloads = append(workloads, group.workload)
	}
	return workloads
}

func TestDispatcherCopiesPayloadAndHeadersAndPropagatesErrors(t *testing.T) {
	handlerErr := errors.New("retry me")
	var received Task
	dispatch := &dispatcher{handlers: map[string]Handler{
		"task": HandlerFunc(func(_ context.Context, task Task) error {
			received = task
			task.Payload[0] = 'X'
			task.Headers["trace"] = "changed"
			return handlerErr
		}),
	}}
	vendorTask := hibiken.NewTaskWithHeaders("task", []byte("payload"), map[string]string{"trace": "original"})
	err := dispatch.ProcessTask(context.Background(), vendorTask)
	if !errors.Is(err, handlerErr) {
		t.Fatalf("ProcessTask() error = %v", err)
	}
	if received.Type != "task" || string(vendorTask.Payload()) != "payload" || vendorTask.Headers()["trace"] != "original" {
		t.Fatalf("delivery mutated vendor task: received=%+v vendor=%q/%v", received, vendorTask.Payload(), vendorTask.Headers())
	}
	if err := dispatch.ProcessTask(context.Background(), hibiken.NewTask("unknown", nil)); !errors.Is(err, ErrHandlerNotFound) {
		t.Fatalf("unknown ProcessTask() error = %v", err)
	}
}

type testContributor struct {
	name          string
	calls         *[]string
	registrations []HandlerRegistration
}

func (p *testContributor) TaskHandlers() []HandlerRegistration {
	if p.calls != nil {
		*p.calls = append(*p.calls, p.name)
	}
	return p.registrations
}

type panicContributor struct{}

func (panicContributor) TaskHandlers() []HandlerRegistration { panic("boom") }

type testPointerHandler struct{}

func (*testPointerHandler) HandleTask(context.Context, Task) error { return nil }
