package banner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	pluginmodel "github.com/xbcio/xbc/plugin/model"
)

// recordedLine is one entry the recording logger kept.
type recordedLine struct {
	level string
	msg   string
	kv    []any
}

// recordingLogger is a log.Logger that keeps what it was told, so a test can
// assert the number of records and that no fields were attached.
type recordingLogger struct {
	lines []recordedLine
}

func (l *recordingLogger) record(level, msg string, kv []any) {
	l.lines = append(l.lines, recordedLine{level: level, msg: msg, kv: append([]any(nil), kv...)})
}

func (l *recordingLogger) Debug(msg string, kv ...any) { l.record("DEBUG", msg, kv) }
func (l *recordingLogger) Info(msg string, kv ...any)  { l.record("INFO", msg, kv) }
func (l *recordingLogger) Warn(msg string, kv ...any)  { l.record("WARN", msg, kv) }
func (l *recordingLogger) Error(msg string, kv ...any) { l.record("ERROR", msg, kv) }
func (l *recordingLogger) Fatal(msg string, kv ...any) { l.record("FATAL", msg, kv) }
func (l *recordingLogger) With(_ ...any) log.Logger    { return l }
func (l *recordingLogger) Enabled(log.Level) bool      { return true }

var _ log.Logger = (*recordingLogger)(nil)

// testHost is the RuntimeHost half of the plugin.Context the tests build
// directly.
type testHost struct {
	logger   log.Logger
	service  plugin.ServiceIdentity
	instance string
}

var _ plugin.RuntimeHost = (*testHost)(nil)

func (h *testHost) ExecutionContext() context.Context { return context.Background() }
func (h *testHost) Logger() log.Logger                { return h.logger }
func (h *testHost) ProcessInstance() string           { return h.instance }
func (h *testHost) Service() plugin.ServiceIdentity   { return h.service }

func (*testHost) IngressStopBudget() time.Duration { return 0 }
func (*testHost) TrafficGate() <-chan struct{}     { return nil }

func (*testHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool { return false }

func (*testHost) Admission(plugin.Identity) plugin.Admission { return nil }

func (*testHost) AdmissionFor(plugin.Identity, plugin.WorkloadKey) plugin.Admission { return nil }

func (*testHost) RequestShutdown(plugin.Identity, string) bool { return false }

// newPluginFromConfig builds the plugin the way the Definition's factory does.
func newPluginFromConfig(t *testing.T, mutate func(*Config)) *Plugin {
	t.Helper()
	cfg := DefaultConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatalf("normalizeConfig(%#v) = %v", cfg, err)
	}
	return &Plugin{config: normalized}
}

func newRuntimeContext(logger log.Logger, service plugin.ServiceIdentity, instance string) *plugin.Context {
	host := &testHost{logger: logger, service: service, instance: instance}
	return plugin.NewRuntimeContext(host, plugin.Identity{Plugin: Key})
}

// TestDefinitionIsCanonicalAndAlwaysEnabled pins the declaration shape: one
// stable handle, one Bundle entry, a ConfigSpec, and the zero Activation that
// makes selecting the Bundle the explicit decision.
func TestDefinitionIsCanonicalAndAlwaysEnabled(t *testing.T) {
	var zero plugin.Definition
	if Definition() == zero || Definition() != Definition() {
		t.Fatal("Definition() must return one non-zero canonical handle")
	}
	bundle := Bundle()
	if reflect.DeepEqual(bundle, plugin.Bundle{}) {
		t.Fatal("Bundle() returned an empty composition")
	}
	if !reflect.DeepEqual(bundle, Bundle()) {
		t.Fatal("Bundle() returned different canonical composition content")
	}

	entries := pluginmodel.BundleEntries(pluginmodel.Bundle(bundle))
	if len(entries) != 1 {
		t.Fatalf("Bundle() entries = %d, want 1", len(entries))
	}
	if !pluginmodel.SameDefinition(entries[0].Definition, pluginmodel.Definition(Definition())) {
		t.Fatal("Bundle() does not contain the canonical Definition handle")
	}

	descriptor, ok := pluginmodel.DescribeDefinition(pluginmodel.Definition(Definition()))
	if !ok {
		t.Fatal("Definition() returned a zero handle")
	}
	if descriptor.Key != pluginmodel.Key(Key) {
		t.Fatalf("Definition key = %v, want %v", descriptor.Key, Key)
	}
	if descriptor.Activation.Kind != pluginmodel.ActivationAlways {
		t.Fatalf("Definition activation = %+v, want the zero always-enabled policy", descriptor.Activation)
	}
	if descriptor.Primary != reflect.TypeOf((*Plugin)(nil)) {
		t.Fatalf("Definition primary = %v, want *Plugin", descriptor.Primary)
	}
	if descriptor.Config == nil {
		t.Fatal("Definition declares no ConfigSpec")
	}
	if len(descriptor.Contracts) != 0 {
		t.Fatalf("Definition contracts = %+v, want no exports beyond the primary", descriptor.Contracts)
	}
}

// TestStartRendersTheConfiguredTextAsOneASCIIRecord is the core behavior: one
// Info record carrying the multi-line art and no attached fields.
func TestStartRendersTheConfiguredTextAsOneASCIIRecord(t *testing.T) {
	logger := &recordingLogger{}
	p := newPluginFromConfig(t, func(cfg *Config) { cfg.Text = "XBC" })
	ctx := newRuntimeContext(logger, plugin.ServiceIdentity{Name: "ignored"}, "ignored")

	if err := p.start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if len(logger.lines) != 1 {
		t.Fatalf("records = %d, want exactly 1", len(logger.lines))
	}
	line := logger.lines[0]
	if line.level != "INFO" {
		t.Fatalf("record level = %s, want INFO", line.level)
	}
	if len(line.kv) != 0 {
		t.Fatalf("record fields = %v, want none", line.kv)
	}
	if lines := strings.Split(line.msg, "\n"); len(lines) != 5 {
		t.Fatalf("banner lines = %d, want the standard font's 5: %q", len(lines), line.msg)
	}
	if strings.Contains(line.msg, "\x1b") {
		t.Fatal("banner contains an ANSI escape sequence")
	}
	if !strings.Contains(line.msg, "____") {
		t.Fatalf("banner = %q, want recognizable art for XBC", line.msg)
	}
}

// TestStartPlainStyleRendersOneLine pins the escape hatch for environments
// that do not want the oversized output.
func TestStartPlainStyleRendersOneLine(t *testing.T) {
	logger := &recordingLogger{}
	p := newPluginFromConfig(t, func(cfg *Config) {
		cfg.Text = "order service"
		cfg.Style = StylePlain
	})
	ctx := newRuntimeContext(logger, plugin.ServiceIdentity{}, "")

	if err := p.start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if len(logger.lines) != 1 || logger.lines[0].msg != "order service" {
		t.Fatalf("records = %#v, want one %q message", logger.lines, "order service")
	}
}

// TestStartResolvesTheTextFromTheRuntime pins the fallback chain: configured
// text, then service name, then process instance, then the fixed default.
func TestStartResolvesTheTextFromTheRuntime(t *testing.T) {
	tests := map[string]struct {
		text     string
		service  plugin.ServiceIdentity
		instance string
		want     string
	}{
		"configured text wins": {
			text:     "checkout",
			service:  plugin.ServiceIdentity{Name: "orders"},
			instance: "host-1",
			want:     "checkout",
		},
		"service name": {
			service:  plugin.ServiceIdentity{Name: "orders"},
			instance: "host-1",
			want:     "orders",
		},
		"process instance": {
			instance: "host-1",
			want:     "host-1",
		},
		"fixed default": {
			want: defaultText,
		},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			logger := &recordingLogger{}
			p := newPluginFromConfig(t, func(cfg *Config) {
				cfg.Text = testCase.text
				cfg.Style = StylePlain
			})
			ctx := newRuntimeContext(logger, testCase.service, testCase.instance)
			if err := p.start(ctx); err != nil {
				t.Fatalf("Start() = %v", err)
			}
			if len(logger.lines) != 1 || logger.lines[0].msg != testCase.want {
				t.Fatalf("records = %#v, want one %q message", logger.lines, testCase.want)
			}
		})
	}
}

// TestStartSanitizesRuntimeText keeps a runtime-supplied name from smuggling
// a control character into the single-line style.
func TestStartSanitizesRuntimeText(t *testing.T) {
	logger := &recordingLogger{}
	p := newPluginFromConfig(t, func(cfg *Config) { cfg.Style = StylePlain })
	ctx := newRuntimeContext(logger, plugin.ServiceIdentity{Name: "orders\napi\x1b"}, "")

	if err := p.start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	if len(logger.lines) != 1 || logger.lines[0].msg != "orders api" {
		t.Fatalf("records = %#v, want one %q message", logger.lines, "orders api")
	}
}

// TestStartRequiresContext rejects the nil Context a mis-wired harness would
// pass.
func TestStartRequiresContext(t *testing.T) {
	p := newPluginFromConfig(t, nil)
	if err := p.start(nil); err == nil {
		t.Fatal("Start(nil) succeeded")
	}
}

// TestConstructingThePluginWritesNothing pins the side-effect boundary: only
// the lifecycle hook may write, so evaluating the declarations and building
// the value the factory returns is silent.
func TestConstructingThePluginWritesNothing(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	original := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = original
		_ = writer.Close()
		_ = reader.Close()
	})

	_ = newPluginFromConfig(t, nil)
	_ = Definition()
	_ = Bundle()

	if err := writer.Close(); err != nil {
		t.Fatalf("close captured stdout = %v", err)
	}
	os.Stdout = original
	written, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured stdout = %v", err)
	}
	if len(written) != 0 {
		t.Fatalf("construction wrote %q, want nothing", written)
	}
}

// TestBannerIsOneWellFormedJSONRecord pins the machine-readable shape: the
// facade's JSON encoding writes one object for the banner, with the newlines
// escaped inside the message and no ANSI escape sequence anywhere.
func TestBannerIsOneWellFormedJSONRecord(t *testing.T) {
	cfg := log.DefaultConfig()
	cfg.Console.Enabled = false
	cfg.File.Enabled = false
	if err := log.Init(cfg); err != nil {
		t.Fatalf("log.Init() = %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	var output bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{
		MessageKey:  "msg",
		LevelKey:    "level",
		EncodeLevel: zapcore.LowercaseLevelEncoder,
		TimeKey:     "ts",
		EncodeTime:  zapcore.EpochTimeEncoder,
	})
	detach := log.Attach(zapcore.NewCore(encoder, zapcore.AddSync(&output), zapcore.InfoLevel))
	t.Cleanup(detach)

	p := newPluginFromConfig(t, func(cfg *Config) { cfg.Text = "XBC" })
	ctx := newRuntimeContext(log.L(), plugin.ServiceIdentity{}, "")
	if err := p.start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	raw := output.String()
	if records := strings.Count(raw, "\n"); records != 1 {
		t.Fatalf("JSON records = %d, want exactly 1: %q", records, raw)
	}
	if strings.Contains(raw, "\x1b") {
		t.Fatalf("JSON output contains an ANSI escape sequence: %q", raw)
	}
	var record struct {
		Message string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &record); err != nil {
		t.Fatalf("JSON banner is not one well-formed object: %v: %q", err, raw)
	}
	if lines := strings.Split(record.Message, "\n"); len(lines) != 5 {
		t.Fatalf("decoded banner lines = %d, want 5: %q", len(lines), record.Message)
	}
}

// TestBannerIsOneRecordInTheConsoleFormat pins the human-readable shape: one
// prefixed record whose message continues across the banner's lines, without
// ANSI escapes.
func TestBannerIsOneRecordInTheConsoleFormat(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() = %v", err)
	}
	original := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() {
		os.Stdout = original
		_ = writer.Close()
		_ = reader.Close()
	})

	cfg := log.DefaultConfig()
	cfg.Level = "info"
	cfg.Console.Enabled = true
	cfg.Console.Format = log.FormatConsole
	cfg.Console.Color = log.ColorNever
	cfg.File.Enabled = false
	if err := log.Init(cfg); err != nil {
		t.Fatalf("log.Init() = %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	p := newPluginFromConfig(t, func(cfg *Config) { cfg.Text = "XBC" })
	ctx := newRuntimeContext(log.L(), plugin.ServiceIdentity{}, "")
	if err := p.start(ctx); err != nil {
		t.Fatalf("Start() = %v", err)
	}

	if err := log.Close(); err != nil {
		t.Fatalf("log.Close() = %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close captured stdout = %v", err)
	}
	os.Stdout = original
	captured, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured console output = %v", err)
	}

	out := string(captured)
	if records := strings.Count(out, "INFO"); records != 1 {
		t.Fatalf("console records = %d, want exactly 1: %q", records, out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("console output contains an ANSI escape sequence: %q", out)
	}
	if lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n"); len(lines) != 5 {
		t.Fatalf("console banner lines = %d, want the art's 5: %q", len(lines), out)
	}
}
