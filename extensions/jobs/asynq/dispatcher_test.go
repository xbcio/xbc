package asynq

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	hibiken "github.com/hibiken/asynq"

	"github.com/xbcio/xbc/plugin"
)

func TestCollectHandlersUsesDeterministicTypedEntriesAndFreezesRegistrations(t *testing.T) {
	calls := []string{}
	first := &testContributor{name: "first", calls: &calls, registrations: []HandlerRegistration{{Type: "one", Handler: HandlerFunc(func(context.Context, Task) error { return nil })}}}
	second := &testContributor{name: "second", calls: &calls, registrations: []HandlerRegistration{{Type: "two", Handler: HandlerFunc(func(context.Context, Task) error { return nil })}}}
	contributors := []plugin.Entry[HandlerContributor]{
		{Identity: plugin.Identity{Plugin: "first"}, Value: first},
		{Identity: plugin.Identity{Plugin: "second", Instance: "named"}, Value: second},
	}

	dispatch, err := collectHandlers(contributors)
	if err != nil {
		t.Fatalf("collectHandlers() error = %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"first", "second"}) {
		t.Fatalf("contributor call order = %v", calls)
	}
	if len(dispatch.handlers) != 2 || dispatch.handlers["one"] == nil || dispatch.handlers["two"] == nil {
		t.Fatalf("handlers = %v", dispatch.handlers)
	}
	first.registrations[0].Type = "changed"
	if dispatch.handlers["one"] == nil || dispatch.handlers["changed"] != nil {
		t.Fatal("dispatcher was not frozen independently of contributor registrations")
	}
}

func TestCollectHandlersRejectsInvalidContributorContracts(t *testing.T) {
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
		{name: "no contributors", want: "no task handlers"},
		{name: "empty contributor", contributors: []plugin.Entry[HandlerContributor]{entry("empty", &testContributor{})}, want: "no task handlers"},
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
			_, err := collectHandlers(test.contributors)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("collectHandlers() error = %v, want %q", err, test.want)
			}
		})
	}
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
