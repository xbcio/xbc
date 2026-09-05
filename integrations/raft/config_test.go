package raft

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultConfigIsProductionSafe(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Bootstrap {
		t.Fatal("DefaultConfig().Bootstrap = true; enabling the plugin could create split brain")
	}
	if cfg.Storage != StorageFile {
		t.Fatalf("DefaultConfig().Storage = %q, want file", cfg.Storage)
	}
	if cfg.DataDir == "" || cfg.BindAddr == "" {
		t.Fatalf("DefaultConfig() lacks durable path or bind address: %#v", cfg)
	}
	cfg.NodeID = "node-a"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() error = %v", err)
	}
}

func TestConfigAllowsEphemeralMemoryTransport(t *testing.T) {
	cfg := fastConfig("node-a", true)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("memory ephemeral config error = %v", err)
	}
}

func TestConfigValidatesIdentityAddressStorageAndTimeouts(t *testing.T) {
	valid := fastConfig("node-a", true)
	tests := []struct {
		name   string
		mutate func(*Config)
		match  string
	}{
		{"missing node ID", func(c *Config) { c.NodeID = "" }, "node_id"},
		{"control node ID", func(c *Config) { c.NodeID = "node\n" }, "control"},
		{"bad bind", func(c *Config) { c.BindAddr = "localhost" }, "host:port"},
		{"wildcard without advertise", func(c *Config) { c.BindAddr = "0.0.0.0:7100" }, "advertise_addr"},
		{"wildcard advertise", func(c *Config) { c.BindAddr = "127.0.0.1:7100"; c.AdvertiseAddr = "0.0.0.0:7100" }, "routable"},
		{"advertise with ephemeral bind", func(c *Config) { c.AdvertiseAddr = "127.0.0.1:7100" }, "ephemeral"},
		{"unknown storage", func(c *Config) { c.Storage = "cloud" }, "storage"},
		{"file missing path", func(c *Config) { c.Storage = StorageFile; c.DataDir = ""; c.BindAddr = "127.0.0.1:7100" }, "data_dir"},
		{"file ephemeral port", func(c *Config) { c.Storage = StorageFile; c.DataDir = t.TempDir() }, "stable"},
		{"transport pool", func(c *Config) { c.TransportPool = 0 }, "transport_pool"},
		{"transport timeout", func(c *Config) { c.TransportTimeout = 0 }, "transport_timeout"},
		{"apply timeout", func(c *Config) { c.ApplyTimeout = 0 }, "apply_timeout"},
		{"operation timeout", func(c *Config) { c.OperationTimeout = 0 }, "operation_timeout"},
		{"shutdown timeout", func(c *Config) { c.ShutdownTimeout = 0 }, "shutdown_timeout"},
		{"snapshot retain", func(c *Config) { c.SnapshotRetain = 0 }, "snapshot_retain"},
		{"snapshot threshold", func(c *Config) { c.SnapshotThreshold = 0 }, "snapshot_threshold"},
		{"heartbeat too low", func(c *Config) { c.HeartbeatTimeout = 4 * time.Millisecond }, "HeartbeatTimeout"},
		{"election below heartbeat", func(c *Config) { c.ElectionTimeout = 50 * time.Millisecond }, "ElectionTimeout"},
		{"lease above heartbeat", func(c *Config) { c.LeaderLeaseTimeout = 101 * time.Millisecond }, "LeaderLeaseTimeout"},
		{"commit too low", func(c *Config) { c.CommitTimeout = 0 }, "CommitTimeout"},
		{"snapshot interval too low", func(c *Config) { c.SnapshotInterval = time.Millisecond }, "SnapshotInterval"},
		{"append entries", func(c *Config) { c.MaxAppendEntries = 0 }, "MaxAppendEntries"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.match)
			}
		})
	}
}

func TestConfigValidatesInitialPeersAndLocalIdentity(t *testing.T) {
	base := fastConfig("node-a", true)
	base.BindAddr = "127.0.0.1:7101"
	base.InitialPeers = []Peer{
		{ID: "node-a", Address: "127.0.0.1:7101"},
		{ID: "node-b", Address: "127.0.0.1:7102"},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid initial peers error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		match  string
	}{
		{"peers without bootstrap", func(c *Config) { c.Bootstrap = false }, "bootstrap=true"},
		{"missing local", func(c *Config) { c.InitialPeers[0].ID = "node-c" }, "does not contain"},
		{"local address mismatch", func(c *Config) { c.InitialPeers[0].Address = "127.0.0.1:7199" }, "want advertise"},
		{"duplicate ID", func(c *Config) { c.InitialPeers[1].ID = "node-a"; c.InitialPeers[1].Address = "127.0.0.1:7101" }, "duplicate initial peer ID"},
		{"duplicate address", func(c *Config) { c.InitialPeers[1].Address = "127.0.0.1:7101" }, "duplicate initial peer address"},
		{"wildcard peer", func(c *Config) { c.InitialPeers[1].Address = "0.0.0.0:7102" }, "cannot be a wildcard"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.InitialPeers = append([]Peer(nil), base.InitialPeers...)
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.match)
			}
		})
	}
}
