package raft

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestKVFSMSetDeleteDefensiveCopiesAndErrors(t *testing.T) {
	fsm := NewKVFSM()
	value := []byte("one")
	set, err := EncodeSet("alpha", value)
	if err != nil {
		t.Fatal(err)
	}
	value[0] = 'X'
	response := fsm.Apply(Entry{Index: 1, Term: 1, Data: set})
	result, ok := response.(MutationResult)
	if !ok || result.Existed || result.Previous != nil {
		t.Fatalf("first set response = %#v", response)
	}
	got, ok := fsm.Get("alpha")
	if !ok || string(got) != "one" {
		t.Fatalf("Get(alpha) = %q, %v", got, ok)
	}
	got[0] = 'Y'
	got, _ = fsm.Get("alpha")
	if string(got) != "one" {
		t.Fatalf("Get returned alias; stored value = %q", got)
	}

	set, _ = EncodeSet("alpha", []byte("two"))
	result = fsm.Apply(Entry{Index: 2, Term: 1, Data: set}).(MutationResult)
	if !result.Existed || string(result.Previous) != "one" {
		t.Fatalf("replacement response = %#v", result)
	}
	result.Previous[0] = 'Z'
	got, _ = fsm.Get("alpha")
	if string(got) != "two" {
		t.Fatalf("mutation response aliased state: %q", got)
	}

	deleteCommand, _ := EncodeDelete("alpha")
	result = fsm.Apply(Entry{Index: 3, Term: 1, Data: deleteCommand}).(MutationResult)
	if !result.Existed || string(result.Previous) != "two" || fsm.Len() != 0 {
		t.Fatalf("delete response/state = %#v, len=%d", result, fsm.Len())
	}
	if response := fsm.Apply(Entry{Data: []byte("not a command")}); !errors.Is(response.(error), ErrInvalidCommand) {
		t.Fatalf("malformed command response = %v", response)
	}
	if _, err := EncodeSet("", nil); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("EncodeSet(empty) error = %v", err)
	}
}

func TestKVFSMSnapshotIsDeterministicDeepAndTransactional(t *testing.T) {
	fsm := NewKVFSM()
	for _, pair := range []struct{ key, value string }{{"z", "last"}, {"a", "first"}} {
		command, _ := EncodeSet(pair.key, []byte(pair.value))
		fsm.Apply(Entry{Data: command})
	}

	first, err := fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	second, err := fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var firstBytes, secondBytes bytes.Buffer
	if err := first.Persist(&firstBytes); err != nil {
		t.Fatal(err)
	}
	if err := second.Persist(&secondBytes); err != nil {
		t.Fatal(err)
	}
	first.Release()
	second.Release()
	if !bytes.Equal(firstBytes.Bytes(), secondBytes.Bytes()) {
		t.Fatal("identical KV states produced non-deterministic snapshots")
	}

	command, _ := EncodeSet("a", []byte("changed"))
	fsm.Apply(Entry{Data: command})
	if err := fsm.Restore(bytes.NewReader(firstBytes.Bytes())); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if value, _ := fsm.Get("a"); string(value) != "first" {
		t.Fatalf("restored a = %q, want first", value)
	}

	before, _ := fsm.Get("a")
	if err := fsm.Restore(bytes.NewReader([]byte("corrupt"))); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("corrupt Restore() error = %v", err)
	}
	after, _ := fsm.Get("a")
	if !bytes.Equal(before, after) {
		t.Fatalf("failed Restore changed live state from %q to %q", before, after)
	}
}

func TestKVFSMConcurrentReadApplyAndSnapshot(t *testing.T) {
	fsm := NewKVFSM()
	const workers = 8
	const iterations = 200
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := range workers {
		go func() {
			defer wg.Done()
			for iteration := range iterations {
				key := fmt.Sprintf("key-%d", worker)
				command, err := EncodeSet(key, []byte(fmt.Sprintf("%d", iteration)))
				if err != nil {
					errs <- err
					return
				}
				if response := fsm.Apply(Entry{Data: command}); response == nil {
					errs <- errors.New("nil Apply response")
					return
				}
				fsm.Get(key)
				if iteration%25 == 0 {
					snapshot, err := fsm.Snapshot()
					if err != nil {
						errs <- err
						return
					}
					var out bytes.Buffer
					if err := snapshot.Persist(&out); err != nil {
						errs <- err
						snapshot.Release()
						return
					}
					snapshot.Release()
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if fsm.Len() != workers {
		t.Fatalf("KV length = %d, want %d", fsm.Len(), workers)
	}
}
