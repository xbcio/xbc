package raft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	hashiraft "github.com/hashicorp/raft"
)

// ErrStopped reports API use after shutdown has started.
var ErrStopped = errors.New("raft: plugin is stopped")

// Node is the narrow programmatic Raft API published by the plugin. Raft
// returns the native *hashicorp/raft.Raft for advanced use that this
// interface deliberately does not otherwise wrap.
type Node interface {
	ID() hashiraft.ServerID
	Address() hashiraft.ServerAddress
	FSM() FSM
	Raft() *hashiraft.Raft

	Join(context.Context, hashiraft.ServerID, hashiraft.ServerAddress) error
	Remove(context.Context, hashiraft.ServerID) error
	Apply(context.Context, []byte) (any, error)
	Barrier(context.Context) error
	Snapshot(context.Context) error
	Configuration(context.Context) (hashiraft.Configuration, error)

	State() hashiraft.RaftState
	Leader() (hashiraft.ServerAddress, hashiraft.ServerID)
	LeaderCh() <-chan bool
}

// node is the primary value constructed by Definition's factory. It owns the
// TCP transport and persistence store acquired for the Raft node it wraps and
// implements Node's narrow operational API plus XBC's Stop lifecycle stage.
type node struct {
	raft            *hashiraft.Raft
	transport       *hashiraft.NetworkTransport
	storeCloser     io.Closer
	shutdownTimeout time.Duration

	id               hashiraft.ServerID
	address          hashiraft.ServerAddress
	fsm              FSM
	applyTimeout     time.Duration
	operationTimeout time.Duration
	stopped          atomic.Bool

	stopMu   sync.Mutex
	stopDone chan struct{}
	stopErr  error
}

var _ Node = (*node)(nil)

func (n *node) ID() hashiraft.ServerID           { return n.id }
func (n *node) Address() hashiraft.ServerAddress { return n.address }
func (n *node) FSM() FSM                         { return n.fsm }
func (n *node) Raft() *hashiraft.Raft            { return n.raft }

func (n *node) State() hashiraft.RaftState { return n.raft.State() }

func (n *node) Leader() (hashiraft.ServerAddress, hashiraft.ServerID) {
	return n.raft.LeaderWithID()
}

func (n *node) LeaderCh() <-chan bool { return n.raft.LeaderCh() }

func (n *node) Join(ctx context.Context, id hashiraft.ServerID, address hashiraft.ServerAddress) error {
	if err := n.ready(ctx); err != nil {
		return err
	}
	if err := validateNodeID(string(id)); err != nil {
		return fmt.Errorf("raft: join: %w", err)
	}
	resolved, _, wildcard, err := parseTCPAddress("join address", string(address), false)
	if err != nil {
		return err
	}
	if wildcard {
		return errors.New("raft: join address cannot be a wildcard")
	}
	timeout, opCtx, cancel := operationContext(ctx, n.operationTimeout)
	defer cancel()
	future := n.raft.AddVoter(id, hashiraft.ServerAddress(resolved.String()), 0, timeout)
	if err := waitFuture(opCtx, future); err != nil {
		return fmt.Errorf("raft: join node %q at %q: %w", id, resolved.String(), err)
	}
	return nil
}

func (n *node) Remove(ctx context.Context, id hashiraft.ServerID) error {
	if err := n.ready(ctx); err != nil {
		return err
	}
	if err := validateNodeID(string(id)); err != nil {
		return fmt.Errorf("raft: remove: %w", err)
	}
	timeout, opCtx, cancel := operationContext(ctx, n.operationTimeout)
	defer cancel()
	future := n.raft.RemoveServer(id, 0, timeout)
	if err := waitFuture(opCtx, future); err != nil {
		return fmt.Errorf("raft: remove node %q: %w", id, err)
	}
	return nil
}

func (n *node) Apply(ctx context.Context, command []byte) (any, error) {
	if err := n.ready(ctx); err != nil {
		return nil, err
	}
	if command == nil {
		return nil, errors.New("raft: apply: nil command")
	}
	timeout, opCtx, cancel := operationContext(ctx, n.applyTimeout)
	defer cancel()
	future := n.raft.Apply(append([]byte(nil), command...), timeout)
	if err := waitFuture(opCtx, future); err != nil {
		return nil, fmt.Errorf("raft: apply: %w", err)
	}
	response := future.Response()
	if responseErr, ok := response.(error); ok {
		return nil, responseErr
	}
	return response, nil
}

func (n *node) Barrier(ctx context.Context) error {
	if err := n.ready(ctx); err != nil {
		return err
	}
	timeout, opCtx, cancel := operationContext(ctx, n.operationTimeout)
	defer cancel()
	if err := waitFuture(opCtx, n.raft.Barrier(timeout)); err != nil {
		return fmt.Errorf("raft: barrier: %w", err)
	}
	return nil
}

func (n *node) Snapshot(ctx context.Context) error {
	if err := n.ready(ctx); err != nil {
		return err
	}
	_, opCtx, cancel := operationContext(ctx, n.operationTimeout)
	defer cancel()
	if err := waitFuture(opCtx, n.raft.Snapshot()); err != nil {
		return fmt.Errorf("raft: snapshot: %w", err)
	}
	return nil
}

func (n *node) Configuration(ctx context.Context) (hashiraft.Configuration, error) {
	if err := n.ready(ctx); err != nil {
		return hashiraft.Configuration{}, err
	}
	_, opCtx, cancel := operationContext(ctx, n.operationTimeout)
	defer cancel()
	future := n.raft.GetConfiguration()
	if err := waitFuture(opCtx, future); err != nil {
		return hashiraft.Configuration{}, fmt.Errorf("raft: get configuration: %w", err)
	}
	return future.Configuration(), nil
}

func (n *node) ready(ctx context.Context) error {
	if ctx == nil {
		return errors.New("raft: operation requires a non-nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.stopped.Load() {
		return ErrStopped
	}
	return nil
}

// stop is the Lifecycle.Stop adapter. It is concurrent-safe and idempotent:
// every caller observes the same shutdown result, subject to its own context
// deadline. It shuts down Raft before closing the TCP transport and store it
// owns, bounded by the configured shutdown timeout.
func (n *node) stop(parent context.Context) error {
	if n == nil {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}

	n.stopMu.Lock()
	if n.stopDone != nil {
		done := n.stopDone
		n.stopMu.Unlock()
		select {
		case <-done:
			n.stopMu.Lock()
			err := n.stopErr
			n.stopMu.Unlock()
			return err
		case <-parent.Done():
			return parent.Err()
		}
	}
	n.stopDone = make(chan struct{})
	done := n.stopDone
	timeout := n.shutdownTimeout
	if timeout <= 0 {
		timeout = DefaultConfig().ShutdownTimeout
	}
	n.stopMu.Unlock()

	n.stopped.Store(true)
	stopCtx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- closeAcquired(n.raft, n.transport, n.storeCloser) }()

	var stopErr error
	select {
	case err := <-result:
		stopErr = err
	case <-stopCtx.Done():
		stopErr = fmt.Errorf("raft: shutdown: %w", stopCtx.Err())
	}

	n.stopMu.Lock()
	n.stopErr = stopErr
	close(done)
	n.stopMu.Unlock()
	return stopErr
}

func operationContext(parent context.Context, fallback time.Duration) (time.Duration, context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, fallback)
	timeout := fallback
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
		if timeout <= 0 {
			timeout = time.Nanosecond
		}
	}
	return timeout, ctx, cancel
}

func waitFuture(ctx context.Context, future hashiraft.Future) error {
	result := make(chan error, 1)
	go func() { result <- future.Error() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
