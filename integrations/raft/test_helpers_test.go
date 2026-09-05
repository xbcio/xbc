package raft

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	hashiraft "github.com/hashicorp/raft"

	corelog "github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// testRuntimeHost is a minimal plugin.RuntimeHost used only by tests that
// exercise Definition through internal/assembly. Raft's Lifecycle declares no
// Init or Start stage, so no test here needs task admission or a traffic gate.
type testRuntimeHost struct {
	execution context.Context
}

func (h testRuntimeHost) ExecutionContext() context.Context { return h.execution }
func (testRuntimeHost) Logger() corelog.Logger              { return corelog.Nop() }
func (testRuntimeHost) TrafficGate() <-chan struct{}        { return nil }
func (testRuntimeHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool {
	return false
}
func (testRuntimeHost) RequestShutdown(plugin.Identity, string) bool { return false }

func fastConfig(id string, bootstrap bool) Config {
	cfg := DefaultConfig()
	cfg.NodeID = id
	cfg.BindAddr = "127.0.0.1:0"
	cfg.Storage = StorageMemory
	cfg.DataDir = ""
	cfg.Bootstrap = bootstrap
	cfg.TransportPool = 2
	cfg.TransportTimeout = 500 * time.Millisecond
	cfg.HeartbeatTimeout = 100 * time.Millisecond
	cfg.ElectionTimeout = 100 * time.Millisecond
	cfg.CommitTimeout = 10 * time.Millisecond
	cfg.LeaderLeaseTimeout = 50 * time.Millisecond
	cfg.SnapshotInterval = time.Hour
	cfg.SnapshotThreshold = 64
	cfg.SnapshotRetain = 2
	cfg.ApplyTimeout = 3 * time.Second
	cfg.OperationTimeout = 5 * time.Second
	cfg.ShutdownTimeout = 5 * time.Second
	return cfg
}

// buildTestNode normalizes cfg and constructs a *node with the same factory
// logic Definition uses, without going through plugin.BuildContext or a fake
// FSM Input. A nil fsm installs the default KVFSM.
func buildTestNode(cfg Config, fsm FSM) (*node, error) {
	if fsm == nil {
		fsm = NewKVFSM()
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return buildRuntime(corelog.Nop(), normalized, fsm)
}

// newTestNode builds a *node and registers its Stop for automatic cleanup.
func newTestNode(t *testing.T, cfg Config, fsm FSM) *node {
	t.Helper()
	n, err := buildTestNode(cfg, fsm)
	if err != nil {
		t.Fatalf("buildRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = n.stop(context.Background()) })
	return n
}

func waitForLeader(t *testing.T, node Node) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	for {
		if node.State() == hashiraft.Leader {
			address, id := node.Leader()
			if address != "" && id != "" {
				return
			}
		}
		select {
		case leader := <-node.LeaderCh():
			if leader && node.State() == hashiraft.Leader {
				return
			}
		case <-ctx.Done():
			t.Fatalf("node %q did not become leader: state=%s leader=%q", node.ID(), node.State(), func() string { address, _ := node.Leader(); return string(address) }())
		}
	}
}

func reserveTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve TCP address: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release reserved TCP address: %v", err)
	}
	return address
}

type recordingFSM struct {
	mu      sync.RWMutex
	value   []byte
	applied chan Entry
}

func newRecordingFSM() *recordingFSM {
	return &recordingFSM{applied: make(chan Entry, 16)}
}

func (f *recordingFSM) Apply(entry Entry) any {
	entry.Data = append([]byte(nil), entry.Data...)
	f.mu.Lock()
	f.value = append([]byte(nil), entry.Data...)
	f.mu.Unlock()
	f.applied <- entry
	return append([]byte(nil), entry.Data...)
}

func (f *recordingFSM) Snapshot() (Snapshot, error) {
	f.mu.RLock()
	value := append([]byte(nil), f.value...)
	f.mu.RUnlock()
	return byteSnapshot(value), nil
}

func (f *recordingFSM) Restore(reader io.Reader) error {
	value, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.value = value
	f.mu.Unlock()
	return nil
}

type byteSnapshot []byte

func (s byteSnapshot) Persist(writer io.Writer) error {
	_, err := writer.Write(s)
	return err
}
func (byteSnapshot) Release() {}

type restoreErrorFSM struct{}

func (*restoreErrorFSM) Apply(Entry) any             { return nil }
func (*restoreErrorFSM) Snapshot() (Snapshot, error) { return byteSnapshot(nil), nil }
func (*restoreErrorFSM) Restore(io.Reader) error     { return errors.New("restore rejected for test") }

func receiveEntry(t *testing.T, channel <-chan Entry, want []byte) Entry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	select {
	case entry := <-channel:
		if string(entry.Data) != string(want) {
			t.Fatalf("applied data = %q, want %q", entry.Data, want)
		}
		return entry
	case <-ctx.Done():
		t.Fatalf("timed out waiting for FSM entry %q", want)
		return Entry{}
	}
}

func requireAddressReusable(t *testing.T, address string) {
	t.Helper()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("transport address %q was not released: %v", address, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close reuse listener: %v", err)
	}
}

func operationDeadline(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func assertNoErrorChannel(t *testing.T, errs <-chan error) {
	t.Helper()
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
}

func formatNode(node Node) string {
	return string(node.ID()) + "@" + string(node.Address())
}
