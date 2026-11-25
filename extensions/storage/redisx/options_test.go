package redisx

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultConfigMatchesDefaultTags keeps DefaultConfig a faithful copy of
// the tags the plugin's binder applies: every tagged field must carry the tag's
// value, and every untagged field must stay zero.
func TestDefaultConfigMatchesDefaultTags(t *testing.T) {
	defaults := DefaultConfig()
	assertDefaultsMatchTags(t, reflect.TypeOf(defaults), reflect.ValueOf(defaults))
}

func assertDefaultsMatchTags(t *testing.T, typ reflect.Type, value reflect.Value) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		fieldValue := value.Field(i)
		if field.Type.Kind() == reflect.Struct {
			assertDefaultsMatchTags(t, field.Type, fieldValue)
			continue
		}
		defaultTag, hasDefault := field.Tag.Lookup("default")
		if !hasDefault {
			if !fieldValue.IsZero() {
				t.Errorf("%s = %v but carries no default tag, want the zero value", field.Name, fieldValue.Interface())
			}
			continue
		}
		switch fieldValue.Kind() {
		case reflect.String:
			if got := fieldValue.String(); got != defaultTag {
				t.Errorf("%s = %q, want default tag %q", field.Name, got, defaultTag)
			}
		case reflect.Int, reflect.Int64:
			if field.Type == reflect.TypeOf(time.Duration(0)) {
				want, err := time.ParseDuration(defaultTag)
				if err != nil {
					t.Fatalf("%s default tag %q is not a duration: %v", field.Name, defaultTag, err)
				}
				if got := time.Duration(fieldValue.Int()); got != want {
					t.Errorf("%s = %v, want default tag %v", field.Name, got, want)
				}
				continue
			}
			want, err := strconv.Atoi(defaultTag)
			if err != nil {
				t.Fatalf("%s default tag %q is not an integer: %v", field.Name, defaultTag, err)
			}
			if got := int(fieldValue.Int()); got != want {
				t.Errorf("%s = %d, want default tag %d", field.Name, got, want)
			}
		case reflect.Bool:
			want, err := strconv.ParseBool(defaultTag)
			if err != nil {
				t.Fatalf("%s default tag %q is not a bool: %v", field.Name, defaultTag, err)
			}
			if got := fieldValue.Bool(); got != want {
				t.Errorf("%s = %t, want default tag %t", field.Name, got, want)
			}
		default:
			t.Fatalf("%s has kind %s, which the default tag comparison does not support", field.Name, fieldValue.Kind())
		}
	}
}

// TestOptionsApplyIndividually proves every option writes exactly its own
// field, so no With function aliases another field or silently drops a value.
func TestOptionsApplyIndividually(t *testing.T) {
	tests := []struct {
		name   string
		option Option
		want   Config
	}{
		{name: "mode", option: WithMode(ModeSentinel), want: Config{Mode: ModeSentinel}},
		{name: "addr", option: WithAddr("redis.internal:6380"), want: Config{Addr: "redis.internal:6380"}},
		{name: "addrs", option: WithAddrs("10.0.0.1:26379", "10.0.0.2:26379"), want: Config{Addrs: []string{"10.0.0.1:26379", "10.0.0.2:26379"}}},
		{name: "username", option: WithUsername("service"), want: Config{Username: "service"}},
		{name: "password", option: WithPassword("secret"), want: Config{Password: "secret"}},
		{name: "db", option: WithDB(7), want: Config{DB: 7}},
		{name: "master name", option: WithMasterName("primary"), want: Config{MasterName: "primary"}},
		{name: "sentinel username", option: WithSentinelUsername("watcher"), want: Config{SentinelUsername: "watcher"}},
		{name: "sentinel password", option: WithSentinelPassword("sentinel-secret"), want: Config{SentinelPassword: "sentinel-secret"}},
		{name: "route by latency", option: WithRouteByLatency(true), want: Config{RouteByLatency: true}},
		{name: "route randomly", option: WithRouteRandomly(true), want: Config{RouteRandomly: true}},
		{name: "read only", option: WithReadOnly(true), want: Config{ReadOnly: true}},
		{name: "max redirects", option: WithMaxRedirects(5), want: Config{MaxRedirects: 5}},
		{name: "dial timeout", option: WithDialTimeout(time.Second), want: Config{DialTimeout: time.Second}},
		{name: "read timeout", option: WithReadTimeout(2 * time.Second), want: Config{ReadTimeout: 2 * time.Second}},
		{name: "write timeout", option: WithWriteTimeout(3 * time.Second), want: Config{WriteTimeout: 3 * time.Second}},
		{name: "pool timeout", option: WithPoolTimeout(4 * time.Second), want: Config{PoolTimeout: 4 * time.Second}},
		{name: "pool size", option: WithPoolSize(20), want: Config{PoolSize: 20}},
		{name: "min idle conns", option: WithMinIdleConns(2), want: Config{MinIdleConns: 2}},
		{name: "max idle conns", option: WithMaxIdleConns(8), want: Config{MaxIdleConns: 8}},
		{name: "max active conns", option: WithMaxActiveConns(30), want: Config{MaxActiveConns: 30}},
		{name: "conn max idle time", option: WithConnMaxIdleTime(5 * time.Minute), want: Config{ConnMaxIdleTime: 5 * time.Minute}},
		{name: "conn max lifetime", option: WithConnMaxLifetime(time.Hour), want: Config{ConnMaxLifetime: time.Hour}},
		{name: "max retries", option: WithMaxRetries(5), want: Config{MaxRetries: 5}},
		{name: "ping", option: WithPing(false), want: Config{Ping: false}},
		{
			name:   "tls",
			option: WithTLS(TLSConfig{CAFile: "ca.pem", CertFile: "client.pem", KeyFile: "client-key.pem", ServerName: "redis.internal", MinVersion: "1.3", InsecureSkipVerify: true}),
			want:   Config{TLS: TLSConfig{CAFile: "ca.pem", CertFile: "client.pem", KeyFile: "client-key.pem", ServerName: "redis.internal", MinVersion: "1.3", InsecureSkipVerify: true}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, applyOptions(Config{}, []Option{test.option}))
		})
	}
}

// TestOptionsIgnoreNil proves a nil Option neither panics nor disturbs the
// options around it.
func TestOptionsIgnoreNil(t *testing.T) {
	cfg := applyOptions(DefaultConfig(), []Option{nil, WithPoolSize(17), nil})
	assert.Equal(t, 17, cfg.PoolSize)
	assert.Equal(t, DefaultConfig().Addr, cfg.Addr, "a nil Option must leave the remaining fields alone")
}

// TestOptionsCopyTheCallersConfig proves the constructor's copy owns its Addrs
// slice: neither mutating the caller's slice afterwards nor mutating the slice
// captured by WithAddrs can reach the applied configuration.
func TestOptionsCopyTheCallersConfig(t *testing.T) {
	cfg := Config{Mode: ModeSentinel, Addrs: []string{"10.0.0.1:26379"}}
	applied := applyOptions(cfg, nil)
	cfg.Addrs[0] = "10.0.0.2:26379"
	assert.Equal(t, []string{"10.0.0.1:26379"}, applied.Addrs)

	optionAddrs := []string{"10.0.0.3:26379"}
	fromOption := applyOptions(Config{}, []Option{WithAddrs(optionAddrs...)})
	optionAddrs[0] = "10.0.0.4:26379"
	assert.Equal(t, []string{"10.0.0.3:26379"}, fromOption.Addrs)
}

// TestOpenAppliesOptionsToACopy covers the override contract: options reach the
// constructed client, and Open leaves the caller's Config untouched.
func TestOpenAppliesOptionsToACopy(t *testing.T) {
	server := miniredis.RunT(t)
	closed := miniredis.RunT(t)
	unreachable := closed.Addr()
	closed.Close()

	cfg := DefaultConfig()
	cfg.Addr = unreachable
	cfg.PoolSize = 2
	original := cfg

	client, err := Open(cfg, WithAddr(server.Addr()), WithPoolSize(7), nil)
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	assert.Equal(t, original, cfg, "Open mutated the caller's Config")
	if server.CommandCount() == 0 {
		t.Fatal("Open sent no Redis command; the default ping did not reach the overridden address")
	}

	standalone, ok := client.(*goredis.Client)
	require.True(t, ok, "Open returned %T, want *goredis.Client", client)
	options := standalone.Options()
	assert.Equal(t, server.Addr(), options.Addr)
	assert.Equal(t, 7, options.PoolSize)
}

// TestNewStartsFromDefaultConfig covers the no-argument defaults New applies.
// Ping is disabled so the defaults can be observed without a live server; the
// values go-redis reports are the plugin's documented ones, not its own.
func TestNewStartsFromDefaultConfig(t *testing.T) {
	client, err := New(WithPing(false))
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	standalone, ok := client.(*goredis.Client)
	require.True(t, ok, "New returned %T, want *goredis.Client for the default standalone mode", client)
	options := standalone.Options()
	defaults := DefaultConfig()
	assert.Equal(t, "127.0.0.1:6379", options.Addr)
	assert.Equal(t, defaults.DB, options.DB)
	assert.Equal(t, defaults.DialTimeout, options.DialTimeout)
	assert.Equal(t, defaults.ReadTimeout, options.ReadTimeout)
	assert.Equal(t, defaults.WriteTimeout, options.WriteTimeout)
	assert.Equal(t, defaults.PoolTimeout, options.PoolTimeout)
	assert.Equal(t, defaults.PoolSize, options.PoolSize)
	assert.Equal(t, defaults.MinIdleConns, options.MinIdleConns)
	assert.Equal(t, defaults.MaxIdleConns, options.MaxIdleConns)
	assert.Equal(t, defaults.MaxActiveConns, options.MaxActiveConns)
	assert.Equal(t, defaults.ConnMaxIdleTime, options.ConnMaxIdleTime)
	assert.Equal(t, defaults.ConnMaxLifetime, options.ConnMaxLifetime)
	assert.Equal(t, defaults.MaxRetries, options.MaxRetries)
}

// TestConstructorsRejectMissingTopologyAddresses proves the options cannot
// smuggle a section past the mode rules: sentinel and cluster modes still
// require their seeds, through both exported constructors.
func TestConstructorsRejectMissingTopologyAddresses(t *testing.T) {
	constructors := []struct {
		name      string
		construct func(...Option) (goredis.UniversalClient, error)
	}{
		{
			name: "Open",
			construct: func(opts ...Option) (goredis.UniversalClient, error) {
				return Open(DefaultConfig(), opts...)
			},
		},
		{name: "New", construct: New},
	}
	for _, constructor := range constructors {
		t.Run(constructor.name+"/sentinel", func(t *testing.T) {
			client, err := constructor.construct(WithMode(ModeSentinel), WithMasterName("primary"))
			require.Error(t, err)
			assert.Nil(t, client)
			assert.Contains(t, err.Error(), "mode sentinel requires addrs")
		})
		t.Run(constructor.name+"/cluster", func(t *testing.T) {
			client, err := constructor.construct(WithMode(ModeCluster))
			require.Error(t, err)
			assert.Nil(t, client)
			assert.Contains(t, err.Error(), "mode cluster requires addrs")
		})
	}
}
