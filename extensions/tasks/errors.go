package tasks

import "errors"

// Sentinel errors a caller matches against with errors.Is. An executor wraps
// its own failures so both this contract's sentinel and the executor's remain
// reachable.
var (
	// ErrNotInstalled reports that no executor is installed: neither the
	// local nor the remote plugin is part of this application, so there is
	// nothing to submit work to.
	ErrNotInstalled = errors.New("tasks: no executor is installed")

	// ErrClosed reports that the executor a call needed has begun shutting
	// down or has been uninstalled. It wraps the executor's own state, such
	// as async.ErrShuttingDown.
	ErrClosed = errors.New("tasks: executor is shutting down")

	// ErrSaturated reports that the local pool had no capacity within its
	// submit timeout.
	ErrSaturated = errors.New("tasks: executor is saturated")

	// ErrNoHandler reports that this process holds no handler for the task: a
	// method task whose provider is not part of this application, or a
	// function task this process was not configured to consume.
	ErrNoHandler = errors.New("tasks: no handler for the task in this process")

	// ErrPayload reports that a task argument could not be encoded for
	// delivery or decoded on the way back in.
	ErrPayload = errors.New("tasks: payload could not be encoded or decoded")

	// ErrPermanent marks a failure that must not be retried. It is reported to
	// the caller through Permanent, never returned bare.
	ErrPermanent = errors.New("tasks: permanent failure")
)

// Permanent marks err as a failure the remote executor must not retry, such
// as a validation error no amount of redelivery would fix. A nil err stays
// nil; an err already marked permanent is returned unchanged.
func Permanent(err error) error {
	if err == nil || errors.Is(err, ErrPermanent) {
		return err
	}
	return &permanentError{cause: err}
}

// permanentError carries the original failure so Errors.Is reaches both it and
// ErrPermanent.
type permanentError struct {
	cause error
}

func (e *permanentError) Error() string { return e.cause.Error() }

func (e *permanentError) Unwrap() error { return e.cause }

func (e *permanentError) Is(target error) bool { return target == ErrPermanent }
