package websocket

import (
	"context"

	coderwebsocket "github.com/coder/websocket"
)

// shutdownReason is the close reason sent in the going-away frame Drain
// starts. It is a constant, not a formatted value, because the protocol caps a
// close reason at 125 bytes and an operator-facing message has no business
// varying between connections.
const shutdownReason = "server shutting down"

// connection is one accepted socket in the registry. done is closed by the
// handler after it removes the entry, so Drain and Stop can wait for serve to
// return without holding the lock.
type connection struct {
	conn *Conn
	done chan struct{}
}

// isDraining reports whether upgrades are refused.
func (p *Plugin) isDraining() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.draining
}

// register admits conn to the registry unless shutdown has begun. The flag
// check and the registration happen under one lock; a false return means the
// caller must close the connection without serving it.
func (p *Plugin) register(conn *Conn) (*connection, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.draining {
		return nil, false
	}
	if p.conns == nil {
		p.conns = make(map[*Conn]*connection)
	}
	entry := &connection{conn: conn, done: make(chan struct{})}
	p.conns[conn] = entry
	return entry, true
}

// unregister removes entry and publishes its completion. Removal and the close
// of done are ordered so that a waiter that observes done closed also observes
// the registry without this entry.
func (p *Plugin) unregister(entry *connection) {
	p.mu.Lock()
	if p.conns[entry.conn] == entry {
		delete(p.conns, entry.conn)
	}
	p.mu.Unlock()
	close(entry.done)
}

// beginShutdown sets the draining flag and snapshots the registry under one
// lock. Registration is refused from then on, so an upgrade that has not
// registered yet can never slip past the snapshot.
func (p *Plugin) beginShutdown() []*connection {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.draining = true
	entries := make([]*connection, 0, len(p.conns))
	for _, entry := range p.conns {
		entries = append(entries, entry)
	}
	return entries
}

// Drain stops admitting upgrades, starts a going-away close handshake on every
// registered connection, and waits for the handlers to return or for ctx to
// expire. An expired context ends the wait only: the sockets stay open for
// Stop, which owns the force-close and the resources behind it. It never calls
// CloseNow or releases anything a running handler still uses.
//
// Drain is idempotent and safe before Start, after a failed Start, and after an
// earlier Drain or Stop.
func (p *Plugin) Drain(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	entries := p.beginShutdown()
	for _, entry := range entries {
		// Close performs the close handshake and can block until the peer
		// answers or the library's own handshake timeout expires, so each
		// connection gets its own goroutine. A peer that never answers leaves
		// the connection to Stop.
		go func(entry *connection) {
			_ = entry.conn.Close(coderwebsocket.StatusGoingAway, shutdownReason)
		}(entry)
	}
	return waitForConnections(ctx, entries)
}

// Stop force-closes every remaining connection and waits for the handlers to
// return, bounded by ctx. Each CloseNow runs in its own goroutine: the library
// waits for an in-progress close handshake before it closes the transport, so a
// synchronous loop would let one slow socket delay every connection behind it
// and outlast ctx. The context bounds the wait for the handlers, not the
// library's OS-level close; an expired context returns ctx.Err(), and each
// entry is removed when its handler eventually returns.
//
// Stop is correct whether Drain ran, timed out, failed, or never ran, and it is
// safe before Start and called more than once.
func (p *Plugin) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	entries := p.beginShutdown()
	for _, entry := range entries {
		// CloseNow can block behind an in-progress close handshake, so each
		// connection gets its own goroutine for the same reason Drain's Close
		// calls do; the wait below stays bounded by ctx.
		go func(entry *connection) {
			_ = entry.conn.CloseNow()
		}(entry)
	}
	return waitForConnections(ctx, entries)
}

// waitForConnections waits for every entry's handler to return or for ctx to
// expire.
func waitForConnections(ctx context.Context, entries []*connection) error {
	for _, entry := range entries {
		select {
		case <-entry.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
