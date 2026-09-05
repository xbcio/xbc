package raft

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	hashiraft "github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

func TestJoinReplicateAndRemoveWithCustomFSM(t *testing.T) {
	leaderFSM := newRecordingFSM()
	followerFSM := newRecordingFSM()
	leader := newTestNode(t, fastConfig("leader", true), leaderFSM)
	follower := newTestNode(t, fastConfig("follower", false), followerFSM)
	waitForLeader(t, leader)

	ctx, cancel := operationDeadline(t)
	defer cancel()
	if err := leader.Join(ctx, follower.ID(), follower.Address()); err != nil {
		t.Fatalf("Join(%s) error = %v", formatNode(follower), err)
	}
	configuration, err := leader.Configuration(ctx)
	if err != nil || len(configuration.Servers) != 2 {
		t.Fatalf("configuration after Join = %#v, %v", configuration, err)
	}

	command := []byte("replicated-custom-command")
	response, err := leader.Apply(ctx, command)
	if err != nil || string(response.([]byte)) != string(command) {
		t.Fatalf("Apply() response/error = %q, %v", response, err)
	}
	leaderEntry := receiveEntry(t, leaderFSM.applied, command)
	followerEntry := receiveEntry(t, followerFSM.applied, command)
	if leaderEntry.Index == 0 || followerEntry.Index != leaderEntry.Index || followerEntry.Term != leaderEntry.Term {
		t.Fatalf("replicated entries differ: leader=%#v follower=%#v", leaderEntry, followerEntry)
	}

	if err := leader.Remove(ctx, follower.ID()); err != nil {
		t.Fatalf("Remove(%q) error = %v", follower.ID(), err)
	}
	configuration, err = leader.Configuration(ctx)
	if err != nil || len(configuration.Servers) != 1 || configuration.Servers[0].ID != leader.ID() {
		t.Fatalf("configuration after Remove = %#v, %v", configuration, err)
	}
	// ShutdownOnRemove defaults to true, so the removed follower has usually
	// already shut itself down by the time the test calls stop explicitly.
	if err := follower.stop(context.Background()); err != nil && !errors.Is(err, hashiraft.ErrRaftShutdown) {
		t.Fatalf("follower stop() error = %v", err)
	}
	if err := leader.stop(context.Background()); err != nil {
		t.Fatalf("leader stop() error = %v", err)
	}
}

func TestFileSnapshotRestartSkipsBootstrapAndFailedInitCleansResources(t *testing.T) {
	address := reserveTCPAddress(t)
	dataDir := t.TempDir()
	cfg := fastConfig("durable", true)
	cfg.BindAddr = address
	cfg.Storage = StorageFile
	cfg.DataDir = dataDir
	cfg.SnapshotThreshold = 1

	firstNode := newTestNode(t, cfg, nil)
	waitForLeader(t, firstNode)
	ctx, cancel := operationDeadline(t)
	command, err := EncodeSet("durable-key", []byte("durable-value"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := firstNode.Apply(ctx, command); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	if err := firstNode.Barrier(ctx); err != nil {
		t.Fatalf("first Barrier() error = %v", err)
	}
	if err := firstNode.Snapshot(ctx); err != nil {
		t.Fatalf("first Snapshot() error = %v", err)
	}
	cancel()
	if err := firstNode.stop(context.Background()); err != nil {
		t.Fatalf("first stop() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "raft.db")); err != nil {
		t.Fatalf("persistent Bolt store missing: %v", err)
	}

	// NewRaft must open the existing snapshot before its goroutines start. A
	// deliberate Restore failure therefore exercises buildRuntime's cleanup
	// after both the TCP listener and Bolt store have already been acquired.
	if _, err := buildTestNode(cfg, &restoreErrorFSM{}); err == nil {
		t.Fatal("buildTestNode with a rejecting Restore unexpectedly succeeded")
	}
	requireAddressReusable(t, address)
	store, err := raftboltdb.NewBoltStore(filepath.Join(dataDir, "raft.db"))
	if err != nil {
		t.Fatalf("failed construction leaked the Bolt file lock: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Keeping bootstrap=true intentionally verifies HasExistingState prevents a
	// second BootstrapCluster call on restart.
	secondNode := newTestNode(t, cfg, nil)
	waitForLeader(t, secondNode)
	ctx, cancel = operationDeadline(t)
	defer cancel()
	if err := secondNode.Barrier(ctx); err != nil {
		t.Fatalf("restart Barrier() error = %v", err)
	}
	fsm := secondNode.FSM().(*KVFSM)
	if value, ok := fsm.Get("durable-key"); !ok || string(value) != "durable-value" {
		t.Fatalf("restored durable-key = %q, %v", value, ok)
	}
	configuration, err := secondNode.Configuration(ctx)
	if err != nil || len(configuration.Servers) != 1 {
		t.Fatalf("restart Configuration() = %#v, %v", configuration, err)
	}
	if err := secondNode.stop(context.Background()); err != nil {
		t.Fatalf("second stop() error = %v", err)
	}
	requireAddressReusable(t, address)
}

func TestTransportBindFailureCleansUpAndDoesNotDisturbOwner(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	cfg := fastConfig("occupied", true)
	cfg.BindAddr = listener.Addr().String()
	if _, err := buildTestNode(cfg, nil); err == nil {
		t.Fatal("buildTestNode on an occupied address error = nil")
	}
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("owner listener was disturbed by the failed bind attempt: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
}
