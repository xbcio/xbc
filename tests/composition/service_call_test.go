package composition_test

// This file holds the outbound-call case: one application calls another
// through the http-client plugin, and one trace spans the call. Both
// applications are real, booted through the public facade in one process, and
// the spans are read from a real OTLP collector their two private pipelines
// export to -- because "the callee's inbound span and the caller's attempt
// span are one trace" is a property of what each process reported, which no
// single side can be asked about alone.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"

	"github.com/xbcio/xbc/extensions/httpclient"
	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
	ginengine "github.com/xbcio/xbc/transport/web/engines/gin"
	webtracing "github.com/xbcio/xbc/transport/web/extensions/observability/tracing"
	webhealth "github.com/xbcio/xbc/transport/web/extensions/reliability/health"
)

const (
	// callCallerService and callCalleeService are the two process identities
	// the applications boot under. Each span is attributed to its application
	// through the resource its owning pipeline stamped, so the assertions
	// below can never confuse one process's spans for the other's.
	callCallerService = "composition-caller"
	callCalleeService = "composition-callee"

	// callInstance is the named http-client instance the caller's route binds,
	// the same way a service names one instance per downstream.
	callInstance = "callee"

	// callCalleeLogMessage is the message the callee logs under its request
	// context; the test finds that one line in the sink.
	callCalleeLogMessage = "composition: the callee served the call"

	// The two instrumentation scopes pin which producer opened each span:
	// the caller's attempt span comes from the http-client plugin, the server
	// spans from the Web transport's tracing middleware. Neither constant is
	// exported by its package, so a rename shows up here as a failing
	// assertion rather than as a test that silently stops matching.
	callClientScope = "github.com/xbcio/xbc/extensions/httpclient"
	callServerScope = "github.com/xbcio/xbc/transport/web/extensions/observability/tracing"
)

// ── the OTLP collector both applications export to ─────────────────────

// observedSpan is the flattened view of one exported span.
type observedSpan struct {
	service  string
	scope    string
	name     string
	kind     string
	traceID  string
	spanID   string
	parentID string
}

// otlpCollector is a real OTLP/gRPC trace service. Reading what the SDKs
// exported is what makes the trace assertion an observation rather than a
// restatement: the wire headers could be compared without an exporter, but
// then the test would be checking its own extraction instead of the two
// telemetry pipelines.
type otlpCollector struct {
	tracepb.UnimplementedTraceServiceServer

	mu    sync.Mutex
	spans []observedSpan
}

func (c *otlpCollector) Export(_ context.Context, request *tracepb.ExportTraceServiceRequest) (*tracepb.ExportTraceServiceResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, resourceSpans := range request.GetResourceSpans() {
		service := ""
		for _, attribute := range resourceSpans.GetResource().GetAttributes() {
			if attribute.GetKey() == "service.name" {
				service = attribute.GetValue().GetStringValue()
			}
		}
		for _, scopeSpans := range resourceSpans.GetScopeSpans() {
			scope := scopeSpans.GetScope().GetName()
			for _, span := range scopeSpans.GetSpans() {
				c.spans = append(c.spans, observedSpan{
					service:  service,
					scope:    scope,
					name:     span.GetName(),
					kind:     span.GetKind().String(),
					traceID:  hex.EncodeToString(span.GetTraceId()),
					spanID:   hex.EncodeToString(span.GetSpanId()),
					parentID: hex.EncodeToString(span.GetParentSpanId()),
				})
			}
		}
	}
	return &tracepb.ExportTraceServiceResponse{}, nil
}

func (c *otlpCollector) snapshot() []observedSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]observedSpan(nil), c.spans...)
}

// newOTLPCollector serves OTLP/gRPC on a loopback listener for the duration
// of the test. It must be listening before either application boots: an
// exporter constructed against a dead endpoint keeps retrying in the
// background, and the test would then read an empty collector instead of a
// failure.
func newOTLPCollector(t *testing.T) (string, *otlpCollector) {
	t.Helper()
	collector := &otlpCollector{}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	tracepb.RegisterTraceServiceServer(server, collector)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String(), collector
}

// ── the two applications ───────────────────────────────────────────────

// callCallee is the callee's own route. It logs under the request context --
// which the Web tracing middleware replaced with the context carrying the
// inbound server span -- so the line it writes is the one an operator would
// correlate the trace by.
type callCallee struct{}

var _ web.RouteContributor = (*callCallee)(nil)

var callCalleeDefinition = plugin.Define(
	"composition-call-callee",
	func(plugin.BuildContext) (*callCallee, error) { return &callCallee{}, nil },
	plugin.Options[*callCallee]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *callCallee) web.RouteContributor { return value }),
		),
	},
)

func (*callCallee) RegisterRoutes(router *web.Router) {
	router.GET("/work", callCalleeWork).Name("composition.call.work").Auth(web.Public())
}

func callCalleeWork(_ context.Context, c *web.Ctx) error {
	log.TInfo(c.Request().Context(), callCalleeLogMessage, "route", "/work")
	c.JSON(http.StatusOK, map[string]string{"served_by": callCalleeService})
	return nil
}

// callCaller is the caller's route. It calls the configured downstream
// through the http-client plugin with the inbound request's context as the
// parent, which is what makes the plugin's attempt span a child of the
// caller's own server span and the callee's span a child of the attempt.
type callCaller struct {
	client *httpclient.Client
	target string
}

var _ web.RouteContributor = (*callCaller)(nil)

// callCallerBundle binds one http-client instance by name, the way a service
// root binds the client it declared in plugins.http-client.
func callCallerBundle(target string) plugin.Bundle {
	calleeClient := plugin.RefToInstance[*httpclient.Client](httpclient.Key, callInstance)
	return plugin.BundleOf(plugin.Define(
		"composition-call-caller",
		func(ctx plugin.BuildContext) (*callCaller, error) {
			return &callCaller{client: calleeClient.Get(ctx).Value, target: target}, nil
		},
		plugin.Options[*callCaller]{
			Inputs: plugin.Inputs(calleeClient),
			Exports: plugin.Contracts(
				plugin.ExportAs[web.RouteContributor](func(value *callCaller) web.RouteContributor { return value }),
			),
		},
	))
}

func (p *callCaller) RegisterRoutes(router *web.Router) {
	router.GET("/call", p.handleCall).Name("composition.call.call").Auth(web.Public())
}

func (p *callCaller) handleCall(_ context.Context, c *web.Ctx) error {
	request, err := http.NewRequestWithContext(c.Request().Context(), http.MethodGet, p.target, nil)
	if err != nil {
		return err
	}
	response, err := p.client.Client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	c.Data(response.StatusCode, "application/json", body)
	return nil
}

// callConfig renders one application's configuration: the process identity
// every consumer stamps, one Web listener, and the tracing pipeline that
// exports to the collector. Both applications write to the same log sink --
// the backend is process-global, and booting two applications in one process
// is exactly what makes that visible -- so the assertion identifies the
// callee's line by its message rather than by the file it is in.
func callConfig(serviceName, addr, collectorAddr, logPath string) string {
	return fmt.Sprintf(`xbc:
  shutdown_timeout: 15s
  service:
    name: %s
log:
  console:
    enabled: false
  file:
    enabled: true
    path: %q
web:
  addr: %q
plugins:
  tracing:
    exporter:
      protocol: grpc
      endpoint: %q
      insecure: true
    batch:
      batch_timeout: 200ms
`, serviceName, logPath, addr, collectorAddr)
}

// ── the case ───────────────────────────────────────────────────────────

// TestOneTraceSpansBothApplicationsAndTheCalleesLogLine boots two real
// applications, has the caller's route call the callee's route through the
// http-client plugin, and asserts on what the two pipelines exported: the
// callee's inbound span is a child of the caller's attempt span in one trace,
// on top of a caller-side server span that is the attempt's own parent, and
// the callee's log line carries the trace_id of that same trace.
//
// The applications are stopped before the assertions read the collector:
// each process's tracing Stop flushes its pipeline, so the collected spans
// are complete by the time the test looks, with no waiting on a batch timer.
func TestOneTraceSpansBothApplicationsAndTheCalleesLogLine(t *testing.T) {
	collectorAddr, collector := newOTLPCollector(t)
	callerAddr := dualReserveAddr(t)
	calleeAddr := dualReserveAddr(t)
	logPath := filepath.Join(t.TempDir(), "composition.jsonl")

	runCallee := dualBootApp(t, callConfig(callCalleeService, calleeAddr, collectorAddr, logPath),
		web.Bundle(), ginengine.Bundle(), webhealth.Bundle(), webtracing.Bundle(),
		plugin.BundleOf(callCalleeDefinition),
	)
	// The callee finishes booting -- and settling -- before the caller's boot
	// runs, because the two share the process-global log backend: the second
	// Init closes the sink the first opened, and a write still in flight from
	// a process that is still starting would be discarded rather than
	// recorded. Booting in order also makes the caller's sink the one both
	// applications' request-time lines are written to.
	dualWaitFor(t, 10*time.Second, "the callee to serve", func() error { return dualHTTPReady(calleeAddr) })

	runCaller := dualBootApp(t, callConfig(callCallerService, callerAddr, collectorAddr, logPath)+fmt.Sprintf(`  http-client:
    %s:
      base_url: %q
      timeout: 5s
`, callInstance, "http://"+calleeAddr),
		web.Bundle(), ginengine.Bundle(), webhealth.Bundle(), webtracing.Bundle(),
		httpclient.Bundle(), callCallerBundle("http://"+calleeAddr+"/work"),
	)
	dualWaitFor(t, 10*time.Second, "the caller to serve", func() error { return dualHTTPReady(callerAddr) })

	result := dualGet(callerAddr, "/call", "", 5*time.Second)
	require.NoError(t, result.err)
	require.Equal(t, http.StatusOK, result.status)
	require.Contains(t, result.body, callCalleeService, "the caller must have reached the callee")

	runCaller.cancel()
	require.NoError(t, runCaller.wait(t, 20*time.Second).err)
	runCallee.cancel()
	require.NoError(t, runCallee.wait(t, 20*time.Second).err)

	spans := collector.snapshot()
	caller := callFindSpan(t, spans, callCallerService, "GET /call", "SPAN_KIND_SERVER")
	attempt := callFindSpan(t, spans, callCallerService, "HTTP GET", "SPAN_KIND_CLIENT")
	callee := callFindSpan(t, spans, callCalleeService, "GET /work", "SPAN_KIND_SERVER")
	require.Equal(t, callClientScope, attempt.scope, "the attempt span must come from the http-client plugin")
	require.Equal(t, callServerScope, caller.scope)
	require.Equal(t, callServerScope, callee.scope)
	require.Len(t, callSpans(spans, callCallerService, "HTTP GET"), 1,
		"one call is one attempt when the callee answers on the first try")

	require.Equal(t, caller.traceID, attempt.traceID, "the attempt span belongs to the caller's trace")
	require.Equal(t, caller.spanID, attempt.parentID, "the attempt span's parent is the caller's inbound server span")
	require.Equal(t, attempt.traceID, callee.traceID,
		"the callee's inbound span and the caller's attempt span are one trace")
	require.Equal(t, attempt.spanID, callee.parentID,
		"the callee's inbound span is a child of the attempt span that carried the traceparent")

	require.Equal(t, callee.traceID, callLogTraceID(t, logPath),
		"the callee's own log line carries the trace_id of the trace the call arrived in")
}

// ── span helpers ───────────────────────────────────────────────────────

// callSpans returns every collected span a service reported under one name.
func callSpans(spans []observedSpan, service, name string) []observedSpan {
	var found []observedSpan
	for _, span := range spans {
		if span.service == service && span.name == name {
			found = append(found, span)
		}
	}
	return found
}

// callFindSpan returns the one span a (service, name, kind) selection must
// resolve to. Zero matches means the pipeline never produced it and more than
// one means the assertion would be reading a span other than the call under
// test -- the readyz probes both applications answered, for example.
func callFindSpan(t *testing.T, spans []observedSpan, service, name, kind string) observedSpan {
	t.Helper()
	var found []observedSpan
	for _, span := range callSpans(spans, service, name) {
		if span.kind == kind {
			found = append(found, span)
		}
	}
	require.Len(t, found, 1, "want exactly one %s %q span from %s; the collector holds %d spans", kind, name, service, len(spans))
	return found[0]
}

// callLogTraceID reads the callee's own log line back from the shared JSON
// sink and returns the trace_id it carries. The line is the observable an
// operator correlates a request by, so the assertion is on the rendered
// record rather than on the logger call.
func callLogTraceID(t *testing.T, logPath string) string {
	t.Helper()
	content, err := os.ReadFile(logPath)
	require.NoError(t, err)
	for line := range strings.Lines(string(content)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record), "every sink line is one JSON object: %q", line)
		if record["msg"] != callCalleeLogMessage {
			continue
		}
		traceID, _ := record["trace_id"].(string)
		require.NotEmpty(t, traceID, "the callee's log line must carry the trace_id: %q", line)
		return traceID
	}
	t.Fatalf("the callee's log line %q is not in %s:\n%s", callCalleeLogMessage, logPath, content)
	return ""
}
