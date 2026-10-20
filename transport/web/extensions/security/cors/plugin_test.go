package cors

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

func TestDefinitionAndMiddlewareMetadata(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}

	middleware, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	order := middleware.Order()
	if order.Phase != web.PhaseSecurity || len(order.After) != 0 || len(order.Before) != 0 || middleware.Handler() == nil {
		t.Fatalf("unexpected middleware order: %#v", order)
	}
}

func TestConfiguredConstructionRejectsUnsafeConfiguration(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowCredentials = true
	if _, err := prepareConfig(cfg); err == nil {
		t.Fatal("prepareConfig() succeeded for wildcard origin with credentials")
	}
	if _, err := New(cfg); err == nil {
		t.Fatal("New() succeeded for wildcard origin with credentials")
	}
}

// TestDefaultConfigKeepsTheWildcard pins the deliberate decision behind the
// start warning: the default stays "*", and the plugin tells the operator
// rather than changing behaviour under existing deployments.
func TestDefaultConfigKeepsTheWildcard(t *testing.T) {
	origins := DefaultConfig().AllowOrigins
	if len(origins) != 1 || origins[0] != "*" {
		t.Fatalf("DefaultConfig().AllowOrigins = %v, want [*]", origins)
	}
}

func TestStartWarnsOnceWhenEveryOriginIsAllowed(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "default", cfg: DefaultConfig()},
		{name: "explicit wildcard", cfg: func() Config {
			cfg := DefaultConfig()
			cfg.AllowOrigins = []string{"*"}
			return cfg
		}()},
		// normalizeOrigins trims each entry before detecting the wildcard, so
		// the warning must follow the compiled policy rather than the raw
		// spelling.
		{name: "padded wildcard", cfg: func() Config {
			cfg := DefaultConfig()
			cfg.AllowOrigins = []string{" * "}
			return cfg
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := &captureLogger{}
			p, err := New(tt.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Start(runtimeContext(logger)); err != nil {
				t.Fatalf("Start() = %v, want nil", err)
			}
			entries := logger.snapshot()
			if len(entries) != 1 {
				t.Fatalf("entries = %#v, want exactly one warning", entries)
			}
			entry := entries[0]
			if entry.level != "warn" || entry.msg != "web: CORS allows every origin" {
				t.Fatalf("entry = %#v, want the wildcard warning", entry)
			}
			fields := fieldMap(entry.fields)
			credentials, ok := fields["allow_credentials"].(bool)
			if !ok || credentials {
				t.Fatalf("allow_credentials = %#v, want false", fields["allow_credentials"])
			}
			remedy, ok := fields["remedy"].(string)
			if !ok || !strings.Contains(remedy, "plugins.cors.allow_origins") {
				t.Fatalf("remedy = %#v, want a string naming plugins.cors.allow_origins", fields["remedy"])
			}
		})
	}
}

func TestStartIsSilentForExplicitOrigins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowOrigins = []string{"https://app.example.com"}
	cfg.AllowCredentials = true
	logger := &captureLogger{}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(runtimeContext(logger)); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if entries := logger.snapshot(); len(entries) != 0 {
		t.Fatalf("entries = %#v, want no warning for explicit origins", entries)
	}
	// Start is defensive about a missing context; the framework always passes
	// one, so a nil must simply report success rather than panic.
	if err := p.Start(nil); err != nil {
		t.Fatalf("Start(nil) = %v, want nil", err)
	}
}

// startHost is the smallest plugin.RuntimeHost the Start warning test needs:
// Start reads only the logger, so every other capability reports its zero
// value.
type startHost struct{ logger log.Logger }

var _ plugin.RuntimeHost = startHost{}

func (startHost) ExecutionContext() context.Context { return context.Background() }
func (h startHost) Logger() log.Logger              { return h.logger }
func (startHost) ProcessInstance() string           { return "test-process" }

func (startHost) Service() plugin.ServiceIdentity { return plugin.ServiceIdentity{} }

func (startHost) IngressStopBudget() time.Duration { return 0 }
func (startHost) TrafficGate() <-chan struct{}     { return nil }

func (startHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (startHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }

func (startHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool { return false }

func (startHost) RequestShutdown(plugin.Identity, string) bool { return false }

func runtimeContext(logger log.Logger) *plugin.Context {
	return plugin.NewRuntimeContext(startHost{logger: logger}, plugin.Identity{Plugin: Key})
}

type captured struct {
	level  string
	msg    string
	fields []any
}

type captureLogger struct {
	mu      sync.Mutex
	entries []captured
}

func (l *captureLogger) add(level, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, captured{level: level, msg: msg, fields: append([]any(nil), fields...)})
}

func (l *captureLogger) Debug(msg string, kv ...any) { l.add("debug", msg, kv...) }
func (l *captureLogger) Info(msg string, kv ...any)  { l.add("info", msg, kv...) }
func (l *captureLogger) Warn(msg string, kv ...any)  { l.add("warn", msg, kv...) }
func (l *captureLogger) Error(msg string, kv ...any) { l.add("error", msg, kv...) }
func (*captureLogger) Fatal(string, ...any)          { panic("unexpected fatal") }
func (l *captureLogger) With(...any) log.Logger      { return l }
func (*captureLogger) Enabled(log.Level) bool        { return true }

func (l *captureLogger) snapshot() []captured {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]captured(nil), l.entries...)
}

func fieldMap(values []any) map[string]any {
	result := make(map[string]any)
	for i := 0; i+1 < len(values); i += 2 {
		key, _ := values[i].(string)
		result[key] = values[i+1]
	}
	return result
}
