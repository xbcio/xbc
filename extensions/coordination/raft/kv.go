package raft

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

var (
	// ErrInvalidCommand marks malformed or unsupported default-KV commands.
	ErrInvalidCommand = errors.New("raft: invalid KV command")
	// ErrInvalidSnapshot marks corrupt or unsupported default-KV snapshots.
	ErrInvalidSnapshot = errors.New("raft: invalid KV snapshot")
	// ErrNilSnapshot reports a custom FSM returning a nil Snapshot without an error.
	ErrNilSnapshot = errors.New("raft: FSM returned a nil snapshot")
)

const (
	kvVersion       byte = 1
	kvSet           byte = 1
	kvDelete        byte = 2
	maxKVKeyBytes        = 1 << 20
	maxKVValueBytes      = 64 << 20
	maxKVEntries         = 1_000_000
)

var (
	commandMagic  = [4]byte{'X', 'K', 'V', 'C'}
	snapshotMagic = [4]byte{'X', 'K', 'V', 'S'}
)

// MutationResult describes the value replaced or removed by a KV command.
type MutationResult struct {
	Previous []byte
	Existed  bool
}

// KVFSM is the safe default FSM used when no custom FSM is exported. Keys are
// strings, values are arbitrary bytes, reads and mutation responses are
// defensive copies, and snapshots are deterministic.
type KVFSM struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// NewKVFSM returns an empty default state machine.
func NewKVFSM() *KVFSM { return &KVFSM{data: make(map[string][]byte)} }

// EncodeSet encodes one deterministic mutation accepted by KVFSM.
func EncodeSet(key string, value []byte) ([]byte, error) {
	if err := validateKVKey(key); err != nil {
		return nil, err
	}
	if len(value) > maxKVValueBytes {
		return nil, fmt.Errorf("%w: value exceeds %d bytes", ErrInvalidCommand, maxKVValueBytes)
	}
	out := make([]byte, 0, 9+len(key)+len(value))
	out = append(out, commandMagic[:]...)
	out = append(out, kvVersion, kvSet)
	out = binary.BigEndian.AppendUint32(out, uint32(len(key)))
	out = append(out, key...)
	out = binary.BigEndian.AppendUint64(out, uint64(len(value)))
	out = append(out, value...)
	return out, nil
}

// EncodeDelete encodes one deterministic deletion accepted by KVFSM.
func EncodeDelete(key string) ([]byte, error) {
	if err := validateKVKey(key); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 10+len(key))
	out = append(out, commandMagic[:]...)
	out = append(out, kvVersion, kvDelete)
	out = binary.BigEndian.AppendUint32(out, uint32(len(key)))
	out = append(out, key...)
	return out, nil
}

// Get returns a defensive copy of a value.
func (f *KVFSM) Get(key string) ([]byte, bool) {
	f.mu.RLock()
	value, ok := f.data[key]
	copyValue := append([]byte(nil), value...)
	f.mu.RUnlock()
	return copyValue, ok
}

// Len reports the current number of keys.
func (f *KVFSM) Len() int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return len(f.data)
}

// Apply implements FSM.
func (f *KVFSM) Apply(entry Entry) any {
	command, err := decodeKVCommand(entry.Data)
	if err != nil {
		return err
	}
	f.mu.Lock()
	previous, existed := f.data[command.key]
	result := MutationResult{Previous: append([]byte(nil), previous...), Existed: existed}
	switch command.operation {
	case kvSet:
		f.data[command.key] = append([]byte(nil), command.value...)
	case kvDelete:
		delete(f.data, command.key)
	}
	f.mu.Unlock()
	return result
}

// Snapshot implements FSM by taking a deep copy under a read lock.
func (f *KVFSM) Snapshot() (Snapshot, error) {
	f.mu.RLock()
	data := make(map[string][]byte, len(f.data))
	for key, value := range f.data {
		data[key] = append([]byte(nil), value...)
	}
	f.mu.RUnlock()
	return &kvSnapshot{data: data}, nil
}

// Restore implements FSM transactionally: malformed input leaves current state
// unchanged.
func (f *KVFSM) Restore(reader io.Reader) error {
	if reader == nil {
		return fmt.Errorf("%w: nil reader", ErrInvalidSnapshot)
	}
	data, err := decodeKVSnapshot(reader)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.data = data
	f.mu.Unlock()
	return nil
}

type kvCommand struct {
	operation byte
	key       string
	value     []byte
}

func decodeKVCommand(data []byte) (kvCommand, error) {
	if len(data) < len(commandMagic)+2+4 || string(data[:4]) != string(commandMagic[:]) {
		return kvCommand{}, fmt.Errorf("%w: bad header", ErrInvalidCommand)
	}
	if data[4] != kvVersion {
		return kvCommand{}, fmt.Errorf("%w: unsupported version %d", ErrInvalidCommand, data[4])
	}
	operation := data[5]
	keyLength := int(binary.BigEndian.Uint32(data[6:10]))
	if keyLength < 1 || keyLength > maxKVKeyBytes || len(data) < 10+keyLength {
		return kvCommand{}, fmt.Errorf("%w: invalid key length", ErrInvalidCommand)
	}
	key := string(data[10 : 10+keyLength])
	rest := data[10+keyLength:]
	switch operation {
	case kvSet:
		if len(rest) < 8 {
			return kvCommand{}, fmt.Errorf("%w: missing value length", ErrInvalidCommand)
		}
		valueLength := binary.BigEndian.Uint64(rest[:8])
		if valueLength > maxKVValueBytes || valueLength != uint64(len(rest)-8) {
			return kvCommand{}, fmt.Errorf("%w: invalid value length", ErrInvalidCommand)
		}
		return kvCommand{operation: operation, key: key, value: append([]byte(nil), rest[8:]...)}, nil
	case kvDelete:
		if len(rest) != 0 {
			return kvCommand{}, fmt.Errorf("%w: trailing delete data", ErrInvalidCommand)
		}
		return kvCommand{operation: operation, key: key}, nil
	default:
		return kvCommand{}, fmt.Errorf("%w: unsupported operation %d", ErrInvalidCommand, operation)
	}
}

func validateKVKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: key is empty", ErrInvalidCommand)
	case len(key) > maxKVKeyBytes:
		return fmt.Errorf("%w: key exceeds %d bytes", ErrInvalidCommand, maxKVKeyBytes)
	default:
		return nil
	}
}

type kvSnapshot struct{ data map[string][]byte }

func (s *kvSnapshot) Persist(writer io.Writer) error {
	if writer == nil {
		return fmt.Errorf("%w: nil writer", ErrInvalidSnapshot)
	}
	buffered := bufio.NewWriter(writer)
	if _, err := buffered.Write(snapshotMagic[:]); err != nil {
		return err
	}
	if err := buffered.WriteByte(kvVersion); err != nil {
		return err
	}
	if err := binary.Write(buffered, binary.BigEndian, uint64(len(s.data))); err != nil {
		return err
	}
	keys := make([]string, 0, len(s.data))
	for key := range s.data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := s.data[key]
		if err := binary.Write(buffered, binary.BigEndian, uint32(len(key))); err != nil {
			return err
		}
		if _, err := buffered.WriteString(key); err != nil {
			return err
		}
		if err := binary.Write(buffered, binary.BigEndian, uint64(len(value))); err != nil {
			return err
		}
		if _, err := buffered.Write(value); err != nil {
			return err
		}
	}
	return buffered.Flush()
}

func (s *kvSnapshot) Release() { s.data = nil }

func decodeKVSnapshot(reader io.Reader) (map[string][]byte, error) {
	buffered := bufio.NewReader(reader)
	var magic [4]byte
	if _, err := io.ReadFull(buffered, magic[:]); err != nil || magic != snapshotMagic {
		return nil, fmt.Errorf("%w: bad header", ErrInvalidSnapshot)
	}
	version, err := buffered.ReadByte()
	if err != nil || version != kvVersion {
		return nil, fmt.Errorf("%w: unsupported version", ErrInvalidSnapshot)
	}
	var count uint64
	if err := binary.Read(buffered, binary.BigEndian, &count); err != nil || count > maxKVEntries {
		return nil, fmt.Errorf("%w: invalid entry count", ErrInvalidSnapshot)
	}
	data := make(map[string][]byte, int(count))
	for range count {
		var keyLength uint32
		if err := binary.Read(buffered, binary.BigEndian, &keyLength); err != nil || keyLength == 0 || keyLength > maxKVKeyBytes {
			return nil, fmt.Errorf("%w: invalid key length", ErrInvalidSnapshot)
		}
		keyBytes := make([]byte, int(keyLength))
		if _, err := io.ReadFull(buffered, keyBytes); err != nil {
			return nil, fmt.Errorf("%w: read key: %v", ErrInvalidSnapshot, err)
		}
		key := string(keyBytes)
		if _, duplicate := data[key]; duplicate {
			return nil, fmt.Errorf("%w: duplicate key %q", ErrInvalidSnapshot, key)
		}
		var valueLength uint64
		if err := binary.Read(buffered, binary.BigEndian, &valueLength); err != nil || valueLength > maxKVValueBytes {
			return nil, fmt.Errorf("%w: invalid value length", ErrInvalidSnapshot)
		}
		value := make([]byte, int(valueLength))
		if _, err := io.ReadFull(buffered, value); err != nil {
			return nil, fmt.Errorf("%w: read value: %v", ErrInvalidSnapshot, err)
		}
		data[key] = value
	}
	if _, err := buffered.ReadByte(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data", ErrInvalidSnapshot)
	}
	return data, nil
}
