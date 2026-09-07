package casbinredis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	casbinlib "github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/persist"
	rediswatcher "github.com/casbin/redis-watcher/v2"
	goredis "github.com/redis/go-redis/v9"

	xbccasbin "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin"
)

// Factory opens one independently owned Redis watcher for each live Casbin
// enforcer. It is immutable after construction and safe for concurrent use.
type Factory struct {
	config       normalizedConfig
	constructors watcherConstructors
}

type watcherConstructors struct {
	standalone func(string, rediswatcher.WatcherOptions) (persist.Watcher, error)
	cluster    func(string, rediswatcher.WatcherOptions) (persist.Watcher, error)
}

var _ xbccasbin.WatcherFactory = (*Factory)(nil)

// New validates cfg and constructs a connection-free Factory. Redis I/O starts
// only when Open is called.
func New(cfg Config) (*Factory, error) {
	return newFactory(cfg, watcherConstructors{})
}

func newFactory(cfg Config, constructors watcherConstructors) (*Factory, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	if constructors.standalone == nil {
		constructors.standalone = rediswatcher.NewWatcher
	}
	if constructors.cluster == nil {
		constructors.cluster = rediswatcher.NewWatcherWithCluster
	}
	return &Factory{config: normalized, constructors: constructors}, nil
}

// Open creates private publisher and subscriber clients, installs Casbin's
// incremental update callback, and returns their owning watcher. Ownership of a
// successful result passes to the caller; Factory retains no watcher or client.
func (f *Factory) Open(enforcer *casbinlib.SyncedEnforcer) (persist.Watcher, error) {
	if f == nil {
		return nil, fmt.Errorf("casbin-redis: open watcher with a nil factory")
	}
	if enforcer == nil {
		return nil, fmt.Errorf("casbin-redis: open watcher requires a non-nil enforcer")
	}

	options := rediswatcher.WatcherOptions{
		Channel:                f.config.channel,
		IgnoreSelf:             f.config.ignoreSelf,
		OptionalUpdateCallback: rediswatcher.DefaultUpdateCallback(enforcer),
	}
	if f.config.mode == ModeCluster {
		return f.openCluster(options)
	}
	return f.openStandalone(options)
}

func (f *Factory) openStandalone(options rediswatcher.WatcherOptions) (persist.Watcher, error) {
	connections := newConnectionTracker(f.config.dialTimeout)
	clientOptions := goredis.Options{
		Addr:         f.config.addrs[0],
		Username:     f.config.username,
		Password:     f.config.password,
		DB:           f.config.db,
		DialTimeout:  f.config.dialTimeout,
		ReadTimeout:  f.config.readTimeout,
		WriteTimeout: f.config.writeTimeout,
		Dialer:       connections.DialContext,
	}
	publisher := goredis.NewClient(&clientOptions)
	subscriber := goredis.NewClient(&clientOptions)
	options.Options = clientOptions
	options.PubClient = publisher
	options.SubClient = subscriber

	watcher, err := f.constructors.standalone(f.config.addrs[0], options)
	if err == nil {
		connections.commit()
		return watcher, nil
	}
	if watcher != nil {
		watcher.Close()
	}
	clientCloseErr := closeRedisClients(subscriber, publisher)
	connectionCloseErr := connections.closeConstructionConnections()
	return nil, fmt.Errorf(
		"casbin-redis: open standalone watcher for %q: %w",
		f.config.addrs[0],
		errors.Join(err, clientCloseErr, connectionCloseErr),
	)
}

func (f *Factory) openCluster(options rediswatcher.WatcherOptions) (persist.Watcher, error) {
	connections := newConnectionTracker(f.config.dialTimeout)
	options.ClusterOptions = goredis.ClusterOptions{
		Addrs:        append([]string(nil), f.config.addrs...),
		Username:     f.config.username,
		Password:     f.config.password,
		DialTimeout:  f.config.dialTimeout,
		ReadTimeout:  f.config.readTimeout,
		WriteTimeout: f.config.writeTimeout,
		Dialer:       connections.DialContext,
	}

	joinedAddrs := strings.Join(f.config.addrs, ",")
	watcher, err := f.constructors.cluster(joinedAddrs, options)
	if err == nil {
		connections.commit()
		return watcher, nil
	}
	if watcher != nil {
		watcher.Close()
	}
	closeErr := connections.closeConstructionConnections()
	return nil, fmt.Errorf("casbin-redis: open cluster watcher for %q: %w", joinedAddrs, errors.Join(err, closeErr))
}

func closeRedisClients(clients ...*goredis.Client) error {
	var result error
	for _, client := range clients {
		if client == nil {
			continue
		}
		if err := client.Close(); err != nil && !errors.Is(err, goredis.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	return result
}

type trackerState uint8

const (
	trackerConstructing trackerState = iota
	trackerCommitted
	trackerFailed
)

type connectionTracker struct {
	dialer net.Dialer

	mu          sync.Mutex
	state       trackerState
	connections map[*trackedConnection]struct{}
}

func newConnectionTracker(timeout time.Duration) *connectionTracker {
	return &connectionTracker{
		dialer:      net.Dialer{Timeout: timeout},
		connections: make(map[*trackedConnection]struct{}),
	}
}

func (t *connectionTracker) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	connection, err := t.dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}

	t.mu.Lock()
	switch t.state {
	case trackerCommitted:
		t.mu.Unlock()
		return connection, nil
	case trackerFailed:
		t.mu.Unlock()
		_ = connection.Close()
		return nil, fmt.Errorf("casbin-redis: watcher construction already failed")
	default:
		tracked := &trackedConnection{Conn: connection, owner: t}
		t.connections[tracked] = struct{}{}
		t.mu.Unlock()
		return tracked, nil
	}
}

func (t *connectionTracker) commit() {
	t.mu.Lock()
	t.state = trackerCommitted
	t.connections = nil
	t.mu.Unlock()
}

func (t *connectionTracker) closeConstructionConnections() error {
	t.mu.Lock()
	t.state = trackerFailed
	connections := make([]*trackedConnection, 0, len(t.connections))
	for connection := range t.connections {
		connections = append(connections, connection)
	}
	t.connections = nil
	t.mu.Unlock()

	var result error
	for _, connection := range connections {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func (t *connectionTracker) remove(connection *trackedConnection) {
	t.mu.Lock()
	delete(t.connections, connection)
	t.mu.Unlock()
}

type trackedConnection struct {
	net.Conn
	owner *connectionTracker
}

func (c *trackedConnection) Close() error {
	c.owner.remove(c)
	return c.Conn.Close()
}
