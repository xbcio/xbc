package raft

import (
	"io"

	hashiraft "github.com/hashicorp/raft"
)

// Entry is one committed command delivered to an FSM. Data is a defensive
// copy, so an implementation may retain it after Apply returns.
type Entry struct {
	Index uint64
	Term  uint64
	Data  []byte
}

// FSM is the application-owned replicated state machine contract. Apply must
// be deterministic. Returning an error value from Apply makes Node.Apply return
// that error while preserving HashiCorp Raft's native response semantics.
type FSM interface {
	Apply(Entry) any
	Snapshot() (Snapshot, error)
	Restore(io.Reader) error
}

// Snapshot captures immutable FSM state. Persist must write bytes but must not
// close the writer; the Raft library owns sink close/cancel. Release frees any
// resources held by the captured view.
type Snapshot interface {
	Persist(io.Writer) error
	Release()
}

type fsmAdapter struct{ fsm FSM }

func (a fsmAdapter) Apply(log *hashiraft.Log) any {
	data := append([]byte(nil), log.Data...)
	return a.fsm.Apply(Entry{Index: log.Index, Term: log.Term, Data: data})
}

func (a fsmAdapter) Snapshot() (hashiraft.FSMSnapshot, error) {
	snapshot, err := a.fsm.Snapshot()
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, ErrNilSnapshot
	}
	return snapshotAdapter{snapshot: snapshot}, nil
}

func (a fsmAdapter) Restore(reader io.ReadCloser) error {
	return a.fsm.Restore(reader)
}

type snapshotAdapter struct{ snapshot Snapshot }

func (a snapshotAdapter) Persist(sink hashiraft.SnapshotSink) error {
	return a.snapshot.Persist(sink)
}

func (a snapshotAdapter) Release() { a.snapshot.Release() }
