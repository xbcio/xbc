package gin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xbcio/xbc"
	"github.com/xbcio/xbc/plugin"
	"github.com/xbcio/xbc/transport/web"
)

// multipartObservation is what a handler saw when it read a multipart body. The
// body cap and multipart's own limits both answer 413 request_body_too_large, so
// the status cannot say which limit fired; the handler records it first-hand.
type multipartObservation struct {
	reached  bool
	sentinel bool
	bodyCap  bool
}

// multipartProbe contributes two routes that read a multipart body the two ways
// a handler can. /bind goes through Ctx.Bind, so the error crosses gin's
// ShouldBind, normalizeBindError and ParamError. /raw hands the request to
// ParseMultipartForm itself and returns the error untouched, which leaves the
// mapping to the error boundary.
type multipartProbe struct {
	mu       sync.Mutex
	observed multipartObservation
}

type multipartForm struct {
	Field string `form:"f" binding:"required"`
}

func (p *multipartProbe) record(err error) {
	var tooLarge *http.MaxBytesError
	p.mu.Lock()
	p.observed = multipartObservation{
		reached:  true,
		sentinel: errors.Is(err, multipart.ErrMessageTooLarge),
		bodyCap:  errors.As(err, &tooLarge),
	}
	p.mu.Unlock()
}

func (p *multipartProbe) take() multipartObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	observed := p.observed
	p.observed = multipartObservation{}
	return observed
}

func (p *multipartProbe) RegisterRoutes(router *web.Router) {
	router.POST("/bind", func(_ context.Context, c *web.Ctx) error {
		var form multipartForm
		err := c.Bind(&form)
		p.record(err)
		if err != nil {
			return err
		}
		c.Status(http.StatusNoContent)
		return nil
	}).Name("multipart.bind").Auth(web.Public())
	router.POST("/raw", func(_ context.Context, c *web.Ctx) error {
		err := c.Request().ParseMultipartForm(1 << 20)
		p.record(err)
		if err != nil {
			return err
		}
		c.Status(http.StatusNoContent)
		return nil
	}).Name("multipart.raw").Auth(web.Public())
}

var _ web.RouteContributor = (*multipartProbe)(nil)

// serveMultipartApp boots the real composition: xbc.New, the web server and
// this adapter, with the body cap raised to 64 MiB. The raise is what lets a
// 43 MiB request through to gin's own multipart binding, whose in-memory limit
// is a constant 32 MiB plus the 10 MiB multipart reserves for non-file data.
func serveMultipartApp(t *testing.T) (string, *multipartProbe) {
	t.Helper()
	restoreProcessGlobals(t)

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := reserved.Addr().String()
	require.NoError(t, reserved.Close())

	configPath := filepath.Join(t.TempDir(), "application.yml")
	config := fmt.Sprintf(`xbc:
  shutdown_timeout: 2s
log:
  console:
    enabled: false
  file:
    enabled: false
web:
  addr: %q
  max_request_body_bytes: 67108864
`, addr)
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	probe := &multipartProbe{}
	definition := plugin.Define("multipart-probe", func(plugin.BuildContext) (*multipartProbe, error) {
		return probe, nil
	}, plugin.Options[*multipartProbe]{
		Exports: plugin.Contracts(
			plugin.ExportAs[web.RouteContributor](func(value *multipartProbe) web.RouteContributor { return value }),
		),
	})
	app, err := xbc.New(xbc.WithBundles(web.Bundle(), Bundle(), plugin.BundleOf(definition)))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = app.Execute(ctx, []string{"--config", configPath})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("timed out waiting for Execute to return")
		}
	})
	return addr, probe
}

// postMultipart sends build's body to path and returns the status and the
// Problem Detail code. A refused connection means the server is not serving
// yet, so only that is retried.
func postMultipart(t *testing.T, addr, path string, build func(*multipart.Writer)) (int, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	build(writer)
	require.NoError(t, writer.Close())

	client := &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	var response *http.Response
	require.Eventually(t, func() bool {
		var err error
		response, err = client.Post("http://"+addr+path, writer.FormDataContentType(), bytes.NewReader(body.Bytes()))
		return err == nil
	}, 15*time.Second, 20*time.Millisecond, "the server never answered %s", path)
	defer func() { _ = response.Body.Close() }()

	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	if len(raw) == 0 {
		return response.StatusCode, ""
	}
	var problem struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(raw, &problem), "body: %s", raw)
	return response.StatusCode, problem.Code
}

func formField(t *testing.T, size int) func(*multipart.Writer) {
	return func(w *multipart.Writer) {
		require.NoError(t, w.WriteField("f", strings.Repeat("x", size)))
	}
}

// TestRealMultipartOverflowIs413ThroughTheRealServer drives oversized multipart
// requests through the real composition and shows, per row, which limit fired.
//
// What the experiment established, and these rows pin:
//
//   - gin's ShouldBind parses multipart with a constant 32 MiB, not with
//     Engine.MaxMultipartMemory, so the Bind path overflows at 32 MiB + 10 MiB of
//     non-file data (a 43 MiB field), while a handler's own
//     ParseMultipartForm(1 MiB) overflows at 11 MiB.
//   - normalizeBindError leaves the sentinel alone: gin returns the bare
//     multipart.ErrMessageTooLarge, it is not a SliceValidationError, and the
//     same value reaches ParamError. The adapter needs no change to keep it
//     matchable.
//   - A part header past the limit comes out of net/textproto with an unexported
//     error, which mime/multipart converts to the same sentinel.
//
// Every row also asserts what the handler itself read. The 64 MiB body cap is
// above every body here except the control, so a 413 with the sentinel set and
// no *http.MaxBytesError can only be multipart's own limit.
func TestRealMultipartOverflowIs413ThroughTheRealServer(t *testing.T) {
	addr, probe := serveMultipartApp(t)

	paddedHeader := func(size int) func(*multipart.Writer) {
		return func(w *multipart.Writer) {
			part, err := w.CreatePart(textproto.MIMEHeader{
				"Content-Disposition": {`form-data; name="f"`},
				"X-Padding":           {strings.Repeat("h", size)},
			})
			require.NoError(t, err)
			_, err = part.Write([]byte("v"))
			require.NoError(t, err)
		}
	}
	manyParts := func(w *multipart.Writer) {
		for i := range 1100 {
			require.NoError(t, w.WriteField(fmt.Sprintf("p%d", i), "1"))
		}
	}

	tests := []struct {
		name     string
		path     string
		build    func(*multipart.Writer)
		status   int
		code     string
		observed multipartObservation
	}{
		{name: "bind: non-file field past 32 MiB plus 10 MiB", path: "/bind", build: formField(t, 43<<20), status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", observed: multipartObservation{reached: true, sentinel: true}},
		{name: "bind: part count past the ReadForm limit", path: "/bind", build: manyParts, status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", observed: multipartObservation{reached: true, sentinel: true}},
		{name: "bind: field inside the limit is accepted", path: "/bind", build: formField(t, 41<<20), status: http.StatusNoContent, observed: multipartObservation{reached: true}},
		{name: "raw: non-file field past 1 MiB plus 10 MiB", path: "/raw", build: formField(t, 11<<20+512<<10), status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", observed: multipartObservation{reached: true, sentinel: true}},
		{name: "raw: part header past 1 MiB plus 10 MiB", path: "/raw", build: paddedHeader(11<<20 + 512<<10), status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", observed: multipartObservation{reached: true, sentinel: true}},
		{name: "raw: field inside the limit is accepted", path: "/raw", build: formField(t, 10<<20), status: http.StatusNoContent, observed: multipartObservation{reached: true}},
		{name: "control: body cap fires before the handler", path: "/bind", build: formField(t, 70<<20), status: http.StatusRequestEntityTooLarge, code: "request_body_too_large"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe.take()

			status, code := postMultipart(t, addr, test.path, test.build)

			assert.Equal(t, test.status, status)
			assert.Equal(t, test.code, code)
			assert.Equal(t, test.observed, probe.take(),
				"what the handler read must show which limit the row targets")
		})
	}
}

// TestNormalizeBindErrorKeepsTheMultipartSentinelMatchable pins the adapter half
// of the guarantee on its own: the error Ctx.Bind hands to ParamError must still
// satisfy errors.Is for the sentinel, because ParamError matches it by identity.
func TestNormalizeBindErrorKeepsTheMultipartSentinelMatchable(t *testing.T) {
	assert.ErrorIs(t, normalizeBindError(multipart.ErrMessageTooLarge), multipart.ErrMessageTooLarge)
	assert.ErrorIs(t, normalizeBindError(fmt.Errorf("bind: %w", multipart.ErrMessageTooLarge)), multipart.ErrMessageTooLarge)
}
