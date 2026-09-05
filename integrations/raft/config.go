package raft

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	hashiraft "github.com/hashicorp/raft"
)

// Storage identifies the persistence strategy used by a node.
type Storage string

const (
	// StorageFile persists logs, stable state, and snapshots below DataDir.
	StorageFile Storage = "file"
	// StorageMemory keeps all Raft state in memory and is intended for tests or
	// deliberately ephemeral processes.
	StorageMemory Storage = "memory"
)

// Peer is one voting member of an initial bootstrap configuration.
type Peer struct {
	ID      string `yaml:"id"`
	Address string `yaml:"address"`
}

// Config configures one Raft node. Bootstrap defaults to false. InitialPeers
// is only consumed while bootstrapping a store with no existing Raft state.
type Config struct {
	NodeID        string `yaml:"node_id"`
	BindAddr      string `yaml:"bind_addr"      default:"127.0.0.1:7000"`
	AdvertiseAddr string `yaml:"advertise_addr"`

	Storage Storage `yaml:"storage"  default:"file"`
	DataDir string  `yaml:"data_dir" default:"data/raft"`

	Bootstrap    bool   `yaml:"bootstrap" default:"false"`
	InitialPeers []Peer `yaml:"initial_peers"`

	TransportPool    int           `yaml:"transport_pool"    default:"3"`
	TransportTimeout time.Duration `yaml:"transport_timeout" default:"10s"`

	HeartbeatTimeout   time.Duration `yaml:"heartbeat_timeout"    default:"1s"`
	ElectionTimeout    time.Duration `yaml:"election_timeout"     default:"1s"`
	CommitTimeout      time.Duration `yaml:"commit_timeout"       default:"50ms"`
	LeaderLeaseTimeout time.Duration `yaml:"leader_lease_timeout" default:"500ms"`
	MaxAppendEntries   int           `yaml:"max_append_entries"   default:"64"`
	TrailingLogs       uint64        `yaml:"trailing_logs"        default:"10240"`

	SnapshotInterval  time.Duration `yaml:"snapshot_interval"  default:"2m"`
	SnapshotThreshold uint64        `yaml:"snapshot_threshold" default:"8192"`
	SnapshotRetain    int           `yaml:"snapshot_retain"    default:"2"`

	ApplyTimeout     time.Duration `yaml:"apply_timeout"     default:"5s"`
	OperationTimeout time.Duration `yaml:"operation_timeout" default:"10s"`
	ShutdownTimeout  time.Duration `yaml:"shutdown_timeout"  default:"10s"`
}

// DefaultConfig returns production-oriented defaults. NodeID remains empty and
// must be set by each deployment. Most importantly, Bootstrap is false.
func DefaultConfig() Config {
	return Config{
		BindAddr:           "127.0.0.1:7000",
		Storage:            StorageFile,
		DataDir:            "data/raft",
		TransportPool:      3,
		TransportTimeout:   10 * time.Second,
		HeartbeatTimeout:   time.Second,
		ElectionTimeout:    time.Second,
		CommitTimeout:      50 * time.Millisecond,
		LeaderLeaseTimeout: 500 * time.Millisecond,
		MaxAppendEntries:   64,
		TrailingLogs:       10_240,
		SnapshotInterval:   2 * time.Minute,
		SnapshotThreshold:  8_192,
		SnapshotRetain:     2,
		ApplyTimeout:       5 * time.Second,
		OperationTimeout:   10 * time.Second,
		ShutdownTimeout:    10 * time.Second,
	}
}

// Validate checks all configuration invariants that do not require binding the
// TCP listener. Init validates again and checks the listener's actual address.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

type normalizedConfig struct {
	Config
	advertise      *net.TCPAddr
	effectiveAddr  hashiraft.ServerAddress
	bindPort       int
	initialServers []hashiraft.Server
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	// Validate before trimming so control characters at an edge are rejected
	// rather than silently changing a node's durable identity.
	if err := validateNodeID(c.NodeID); err != nil {
		return normalizedConfig{}, err
	}
	c.NodeID = strings.TrimSpace(c.NodeID)
	c.BindAddr = strings.TrimSpace(c.BindAddr)
	c.AdvertiseAddr = strings.TrimSpace(c.AdvertiseAddr)
	c.DataDir = strings.TrimSpace(c.DataDir)

	if err := validateNodeID(c.NodeID); err != nil {
		return normalizedConfig{}, err
	}
	bind, bindPort, bindWildcard, err := parseTCPAddress("bind_addr", c.BindAddr, true)
	if err != nil {
		return normalizedConfig{}, err
	}

	var advertise *net.TCPAddr
	effective := hashiraft.ServerAddress(bind.String())
	if c.AdvertiseAddr != "" {
		if bindPort == 0 {
			return normalizedConfig{}, errors.New("raft: advertise_addr cannot be used with an ephemeral bind_addr port")
		}
		parsedAdvertise, _, wildcard, parseErr := parseTCPAddress("advertise_addr", c.AdvertiseAddr, false)
		if parseErr != nil {
			return normalizedConfig{}, parseErr
		}
		if wildcard {
			return normalizedConfig{}, errors.New("raft: advertise_addr must identify a routable host, not a wildcard address")
		}
		advertise = parsedAdvertise
		c.AdvertiseAddr = advertise.String()
		effective = hashiraft.ServerAddress(advertise.String())
	} else if bindWildcard {
		return normalizedConfig{}, errors.New("raft: wildcard bind_addr requires an explicit advertise_addr")
	}

	switch c.Storage {
	case StorageMemory:
	case StorageFile:
		if c.DataDir == "" {
			return normalizedConfig{}, errors.New("raft: data_dir is required for file storage")
		}
		if strings.IndexByte(c.DataDir, 0) >= 0 {
			return normalizedConfig{}, errors.New("raft: data_dir contains a NUL byte")
		}
		absolute, absErr := filepath.Abs(filepath.Clean(c.DataDir))
		if absErr != nil {
			return normalizedConfig{}, fmt.Errorf("raft: resolve data_dir: %w", absErr)
		}
		c.DataDir = absolute
		if bindPort == 0 {
			return normalizedConfig{}, errors.New("raft: file storage requires a stable, non-zero bind_addr port")
		}
	default:
		return normalizedConfig{}, fmt.Errorf("raft: storage must be %q or %q, got %q", StorageFile, StorageMemory, c.Storage)
	}

	if !c.Bootstrap && len(c.InitialPeers) != 0 {
		return normalizedConfig{}, errors.New("raft: initial_peers requires bootstrap=true")
	}
	if c.TransportPool < 1 {
		return normalizedConfig{}, errors.New("raft: transport_pool must be positive")
	}
	if c.TransportTimeout <= 0 {
		return normalizedConfig{}, errors.New("raft: transport_timeout must be positive")
	}
	if c.ApplyTimeout <= 0 {
		return normalizedConfig{}, errors.New("raft: apply_timeout must be positive")
	}
	if c.OperationTimeout <= 0 {
		return normalizedConfig{}, errors.New("raft: operation_timeout must be positive")
	}
	if c.ShutdownTimeout <= 0 {
		return normalizedConfig{}, errors.New("raft: shutdown_timeout must be positive")
	}
	if c.SnapshotRetain < 1 {
		return normalizedConfig{}, errors.New("raft: snapshot_retain must be positive")
	}
	if c.SnapshotThreshold == 0 {
		return normalizedConfig{}, errors.New("raft: snapshot_threshold must be positive")
	}

	raftConfig := hashiraft.DefaultConfig()
	raftConfig.LocalID = hashiraft.ServerID(c.NodeID)
	raftConfig.HeartbeatTimeout = c.HeartbeatTimeout
	raftConfig.ElectionTimeout = c.ElectionTimeout
	raftConfig.CommitTimeout = c.CommitTimeout
	raftConfig.LeaderLeaseTimeout = c.LeaderLeaseTimeout
	raftConfig.MaxAppendEntries = c.MaxAppendEntries
	raftConfig.TrailingLogs = c.TrailingLogs
	raftConfig.SnapshotInterval = c.SnapshotInterval
	raftConfig.SnapshotThreshold = c.SnapshotThreshold
	if err := hashiraft.ValidateConfig(raftConfig); err != nil {
		return normalizedConfig{}, fmt.Errorf("raft: invalid timing or log configuration: %w", err)
	}

	servers, err := normalizePeers(c, effective, bindPort)
	if err != nil {
		return normalizedConfig{}, err
	}
	return normalizedConfig{
		Config:         c,
		advertise:      advertise,
		effectiveAddr:  effective,
		bindPort:       bindPort,
		initialServers: servers,
	}, nil
}

func normalizePeers(c Config, effective hashiraft.ServerAddress, bindPort int) ([]hashiraft.Server, error) {
	if len(c.InitialPeers) == 0 {
		return nil, nil
	}
	if bindPort == 0 {
		return nil, errors.New("raft: initial_peers cannot be combined with an ephemeral bind_addr port")
	}

	ids := make(map[hashiraft.ServerID]struct{}, len(c.InitialPeers))
	addresses := make(map[hashiraft.ServerAddress]struct{}, len(c.InitialPeers))
	servers := make([]hashiraft.Server, 0, len(c.InitialPeers))
	localFound := false
	for i, peer := range c.InitialPeers {
		if err := validateNodeID(peer.ID); err != nil {
			return nil, fmt.Errorf("raft: initial_peers[%d]: %w", i, err)
		}
		peer.ID = strings.TrimSpace(peer.ID)
		peer.Address = strings.TrimSpace(peer.Address)
		if err := validateNodeID(peer.ID); err != nil {
			return nil, fmt.Errorf("raft: initial_peers[%d]: %w", i, err)
		}
		address, _, wildcard, err := parseTCPAddress(fmt.Sprintf("initial_peers[%d].address", i), peer.Address, false)
		if err != nil {
			return nil, err
		}
		if wildcard {
			return nil, fmt.Errorf("raft: initial_peers[%d].address cannot be a wildcard", i)
		}
		id := hashiraft.ServerID(peer.ID)
		addr := hashiraft.ServerAddress(address.String())
		if _, exists := ids[id]; exists {
			return nil, fmt.Errorf("raft: duplicate initial peer ID %q", id)
		}
		if _, exists := addresses[addr]; exists {
			return nil, fmt.Errorf("raft: duplicate initial peer address %q", addr)
		}
		ids[id] = struct{}{}
		addresses[addr] = struct{}{}
		servers = append(servers, hashiraft.Server{ID: id, Address: addr, Suffrage: hashiraft.Voter})
		if id == hashiraft.ServerID(c.NodeID) {
			localFound = true
			if addr != effective {
				return nil, fmt.Errorf("raft: local initial peer %q has address %q, want advertise address %q", id, addr, effective)
			}
		}
	}
	if !localFound {
		return nil, fmt.Errorf("raft: initial_peers does not contain local node ID %q", c.NodeID)
	}
	return servers, nil
}

func validateNodeID(id string) error {
	switch {
	case id == "":
		return errors.New("raft: node_id is required")
	case !utf8.ValidString(id):
		return errors.New("raft: node_id must be valid UTF-8")
	case len(id) > 255:
		return errors.New("raft: node_id must not exceed 255 bytes")
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return errors.New("raft: node_id cannot contain control characters")
		}
	}
	return nil
}

func parseTCPAddress(field, value string, allowZeroPort bool) (*net.TCPAddr, int, bool, error) {
	if value == "" {
		return nil, 0, false, fmt.Errorf("raft: %s is required", field)
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return nil, 0, false, fmt.Errorf("raft: %s %q is not a valid host:port: %w", field, value, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65_535 {
		return nil, 0, false, fmt.Errorf("raft: %s %q has an invalid numeric port", field, value)
	}
	if port == 0 && !allowZeroPort {
		return nil, 0, false, fmt.Errorf("raft: %s must use a non-zero port", field)
	}
	address, err := net.ResolveTCPAddr("tcp", value)
	if err != nil {
		return nil, 0, false, fmt.Errorf("raft: resolve %s %q: %w", field, value, err)
	}
	wildcard := host == "" || address.IP == nil || address.IP.IsUnspecified()
	return address, port, wildcard, nil
}
