package asynq

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"

	hibiken "github.com/hibiken/asynq"

	"github.com/xbcio/xbc/plugin"
)

type dispatcher struct {
	handlers map[string]Handler
}

func (d *dispatcher) ProcessTask(ctx context.Context, task *hibiken.Task) error {
	handler, ok := d.handlers[task.Type()]
	if !ok {
		return fmt.Errorf("%w for type %q", ErrHandlerNotFound, task.Type())
	}
	return handler.HandleTask(ctx, Task{
		Type:    task.Type(),
		Payload: append([]byte(nil), task.Payload()...),
		Headers: maps.Clone(task.Headers()),
	})
}

func collectHandlers(contributors []plugin.Entry[HandlerContributor]) (*dispatcher, error) {
	handlers := make(map[string]Handler)
	owners := make(map[string]plugin.Identity)
	for _, contributor := range contributors {
		registrations, err := contributorHandlers(contributor)
		if err != nil {
			return nil, err
		}
		for index, registration := range registrations {
			if strings.TrimSpace(registration.Type) != registration.Type || registration.Type == "" {
				return nil, fmt.Errorf("asynq: HandlerContributor %s returned invalid task type %q at index %d", contributor.Identity, registration.Type, index)
			}
			if isNilInterface(registration.Handler) {
				return nil, fmt.Errorf("asynq: HandlerContributor %s returned nil handler for type %q at index %d", contributor.Identity, registration.Type, index)
			}
			if previous, duplicate := owners[registration.Type]; duplicate {
				return nil, fmt.Errorf("asynq: duplicate handler for task type %q from %s and %s", registration.Type, previous, contributor.Identity)
			}
			handlers[registration.Type] = registration.Handler
			owners[registration.Type] = contributor.Identity
		}
	}
	if len(handlers) == 0 {
		return nil, fmt.Errorf("asynq: no task handlers discovered; compose at least one enabled HandlerContributor")
	}
	return &dispatcher{handlers: handlers}, nil
}

func contributorHandlers(contributor plugin.Entry[HandlerContributor]) (registrations []HandlerRegistration, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			registrations = nil
			err = fmt.Errorf("asynq: HandlerContributor %s TaskHandlers callback panicked: %v", contributor.Identity, recovered)
		}
	}()
	return contributor.Value.TaskHandlers(), nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
