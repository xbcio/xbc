package raft

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	hashiraft "github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable Definition and configuration identity of the Raft plugin.
const Key plugin.Key = "raft"

// ConfigPath is the configuration section that activates Definition.
const ConfigPath = "plugins.raft"

var fsmInput = plugin.OptionalOne[FSM]()

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	buildNode,
	plugin.Options[*node]{
		Instances:  plugin.SingleInstance,
		Activation: plugin.WhenConfigured(ConfigPath),
		Inputs:     plugin.Inputs(fsmInput),
		Exports: plugin.Contracts(
			plugin.ExportAs(func(n *node) Node { return n }),
		),
		Lifecycle: plugin.Lifecycle[*node]{
			Stop: (*node).stop,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns Raft's canonical immutable Definition handle.
func Definition() plugin.Definition { return definition }

// Bundle returns the side-effect-free Raft composition bundle.
func Bundle() plugin.Bundle { return bundle }

func prepareConfig(cfg Config) (Config, error) {
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// buildNode is the primary factory. It acquires the TCP transport and
// persistence stores, bootstraps only an empty store when explicitly
// requested, and starts HashiCorp Raft. An exported FSM contributed by
// another Plugin overrides the default KVFSM. Every failure path closes any
// resource already acquired, matching the ownership-transfer contract: this
// function either returns a fully usable *node or leaves nothing behind.
func buildNode(ctx plugin.BuildContext, cfg Config) (*node, error) {
	fsm := FSM(NewKVFSM())
	if entry, ok := fsmInput.Get(ctx); ok && !isNilFSM(entry.Value) {
		fsm = entry.Value
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return buildRuntime(ctx.Log(), normalized, fsm)
}

func isNilFSM(fsm FSM) bool {
	if fsm == nil {
		return true
	}
	value := reflect.ValueOf(fsm)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

type storageBundle struct {
	logs      hashiraft.LogStore
	stable    hashiraft.StableStore
	snapshots hashiraft.SnapshotStore
	closer    io.Closer
}

func buildRuntime(logger log.Logger, cfg normalizedConfig, fsm FSM) (result *node, err error) {
	if isNilFSM(fsm) {
		return nil, errors.New("raft: fsm is required")
	}
	writer := raftLogWriter{logger: logger}
	storage, err := openStorage(cfg, writer)
	if err != nil {
		return nil, err
	}
	var transport *hashiraft.NetworkTransport
	var raftNode *hashiraft.Raft
	defer func() {
		if err == nil {
			return
		}
		err = errors.Join(err, closeAcquired(raftNode, transport, storage.closer))
	}()

	var advertise net.Addr
	if cfg.advertise != nil {
		advertise = cfg.advertise
	}
	transport, err = hashiraft.NewTCPTransport(
		cfg.BindAddr,
		advertise,
		cfg.TransportPool,
		cfg.TransportTimeout,
		writer,
	)
	if err != nil {
		return nil, fmt.Errorf("raft: create TCP transport at %q: %w", cfg.BindAddr, err)
	}
	actualAddress := transport.LocalAddr()
	if cfg.advertise != nil && actualAddress != cfg.effectiveAddr {
		return nil, fmt.Errorf("raft: transport advertised %q, want %q", actualAddress, cfg.effectiveAddr)
	}

	raftConfig := hashiraft.DefaultConfig()
	raftConfig.LocalID = hashiraft.ServerID(cfg.NodeID)
	raftConfig.HeartbeatTimeout = cfg.HeartbeatTimeout
	raftConfig.ElectionTimeout = cfg.ElectionTimeout
	raftConfig.CommitTimeout = cfg.CommitTimeout
	raftConfig.LeaderLeaseTimeout = cfg.LeaderLeaseTimeout
	raftConfig.MaxAppendEntries = cfg.MaxAppendEntries
	raftConfig.TrailingLogs = cfg.TrailingLogs
	raftConfig.SnapshotInterval = cfg.SnapshotInterval
	raftConfig.SnapshotThreshold = cfg.SnapshotThreshold
	raftConfig.LogOutput = writer

	existing, err := hashiraft.HasExistingState(storage.logs, storage.stable, storage.snapshots)
	if err != nil {
		return nil, fmt.Errorf("raft: inspect existing state: %w", err)
	}
	if cfg.Bootstrap && !existing {
		configuration, configErr := bootstrapConfiguration(cfg, actualAddress)
		if configErr != nil {
			return nil, configErr
		}
		if err := hashiraft.BootstrapCluster(raftConfig, storage.logs, storage.stable, storage.snapshots, transport, configuration); err != nil {
			return nil, fmt.Errorf("raft: bootstrap cluster: %w", err)
		}
	}

	raftNode, err = hashiraft.NewRaft(raftConfig, fsmAdapter{fsm: fsm}, storage.logs, storage.stable, storage.snapshots, transport)
	if err != nil {
		return nil, fmt.Errorf("raft: start node: %w", err)
	}

	return &node{
		raft:             raftNode,
		transport:        transport,
		storeCloser:      storage.closer,
		id:               hashiraft.ServerID(cfg.NodeID),
		address:          actualAddress,
		fsm:              fsm,
		applyTimeout:     cfg.ApplyTimeout,
		operationTimeout: cfg.OperationTimeout,
		shutdownTimeout:  cfg.ShutdownTimeout,
	}, nil
}

func openStorage(cfg normalizedConfig, writer io.Writer) (storageBundle, error) {
	switch cfg.Storage {
	case StorageMemory:
		store := hashiraft.NewInmemStore()
		return storageBundle{
			logs:      store,
			stable:    store,
			snapshots: hashiraft.NewInmemSnapshotStore(),
		}, nil
	case StorageFile:
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return storageBundle{}, fmt.Errorf("raft: create data_dir %q: %w", cfg.DataDir, err)
		}
		store, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft.db"))
		if err != nil {
			return storageBundle{}, fmt.Errorf("raft: open Bolt store: %w", err)
		}
		snapshots, err := hashiraft.NewFileSnapshotStore(cfg.DataDir, cfg.SnapshotRetain, writer)
		if err != nil {
			return storageBundle{}, errors.Join(fmt.Errorf("raft: open snapshot store: %w", err), store.Close())
		}
		return storageBundle{logs: store, stable: store, snapshots: snapshots, closer: store}, nil
	default:
		return storageBundle{}, fmt.Errorf("raft: unsupported storage %q", cfg.Storage)
	}
}

func bootstrapConfiguration(cfg normalizedConfig, actual hashiraft.ServerAddress) (hashiraft.Configuration, error) {
	if len(cfg.initialServers) == 0 {
		return hashiraft.Configuration{Servers: []hashiraft.Server{{
			Suffrage: hashiraft.Voter,
			ID:       hashiraft.ServerID(cfg.NodeID),
			Address:  actual,
		}}}, nil
	}
	for _, server := range cfg.initialServers {
		if server.ID == hashiraft.ServerID(cfg.NodeID) && server.Address != actual {
			return hashiraft.Configuration{}, fmt.Errorf("raft: listener advertises %q but local initial peer uses %q", actual, server.Address)
		}
	}
	return hashiraft.Configuration{Servers: append([]hashiraft.Server(nil), cfg.initialServers...)}, nil
}

func closeAcquired(raftNode *hashiraft.Raft, transport *hashiraft.NetworkTransport, store io.Closer) error {
	var errs []error
	if raftNode != nil {
		if err := raftNode.Shutdown().Error(); err != nil {
			errs = append(errs, fmt.Errorf("raft: stop node: %w", err))
		}
	}
	if transport != nil {
		if err := transport.Close(); err != nil {
			errs = append(errs, fmt.Errorf("raft: close transport: %w", err))
		}
	}
	if store != nil {
		if err := store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("raft: close store: %w", err))
		}
	}
	return errors.Join(errs...)
}

type raftLogWriter struct {
	logger interface{ Info(string, ...any) }
}

func (w raftLogWriter) Write(data []byte) (int, error) {
	if message := strings.TrimSpace(string(data)); message != "" {
		w.logger.Info("hashicorp raft", "message", message)
	}
	return len(data), nil
}
