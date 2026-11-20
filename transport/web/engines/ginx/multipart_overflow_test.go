package ginx

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
	"net/url"
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
	reached   bool
	sentinel  bool
	bodyCap   bool
	fileBytes int64
}

// multipartProbe contributes routes that read a multipart body the ways a
// handler can. /bind goes through Ctx.Bind, so the error crosses gin's
// ShouldBind, normalizeBindError and ParamError. /raw hands the request to
// ParseMultipartForm itself and returns the error untouched, which leaves the
// mapping to the error boundary. /json, /urlencoded and /file are the
// regressions for the same configuration: only the multipart bind path may
// change, and a file part is bounded by the memory allowance only for its
// in-memory share, not its size.
type multipartProbe struct {
	mu       sync.Mutex
	observed multipartObservation
}

type multipartForm struct {
	Field string `form:"f" binding:"required"`
}

type multipartJSONForm struct {
	Value string `json:"value" binding:"required"`
}

type multipartFileForm struct {
	File *multipart.FileHeader `form:"f" binding:"required"`
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

// recordFile keeps the upload size a successful file bind read, so a row can
// prove the file part survived rather than merely that binding returned nil.
func (p *multipartProbe) recordFile(err error, size int64) {
	p.record(err)
	p.mu.Lock()
	p.observed.fileBytes = size
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
	router.POST("/json", func(_ context.Context, c *web.Ctx) error {
		var form multipartJSONForm
		err := c.Bind(&form)
		p.record(err)
		if err != nil {
			return err
		}
		c.Status(http.StatusNoContent)
		return nil
	}).Name("multipart.json").Auth(web.Public())
	router.POST("/urlencoded", func(_ context.Context, c *web.Ctx) error {
		var form multipartForm
		err := c.Bind(&form)
		p.record(err)
		if err != nil {
			return err
		}
		c.Status(http.StatusNoContent)
		return nil
	}).Name("multipart.urlencoded").Auth(web.Public())
	router.POST("/file", func(_ context.Context, c *web.Ctx) error {
		var form multipartFileForm
		err := c.Bind(&form)
		var size int64
		if err == nil {
			file, openErr := form.File.Open()
			if openErr != nil {
				err = openErr
			} else {
				size, err = io.Copy(io.Discard, file)
				_ = file.Close()
			}
		}
		p.recordFile(err, size)
		if err != nil {
			return err
		}
		c.Status(http.StatusNoContent)
		return nil
	}).Name("multipart.file").Auth(web.Public())
}

var _ web.RouteContributor = (*multipartProbe)(nil)

// serveMultipartApp boots the real composition: xbc.New, the web server and
// this adapter, with the body cap raised to 64 MiB and the default multipart
// allowance written out explicitly. The raise is what lets a large request
// through to multipart parsing, where the configured allowance -- not gin's own
// constant -- decides.
func serveMultipartApp(t *testing.T) (string, *multipartProbe) {
	t.Helper()
	return serveMultipartAppWith(t, 8<<20)
}

// serveMultipartAppWith is serveMultipartApp with web.max_multipart_memory
// chosen by the caller.
func serveMultipartAppWith(t *testing.T, maxMultipartMemory int64) (string, *multipartProbe) {
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
  max_multipart_memory: %d
`, addr, maxMultipartMemory)
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
// Problem Detail code.
func postMultipart(t *testing.T, addr, path string, build func(*multipart.Writer)) (int, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	build(writer)
	require.NoError(t, writer.Close())
	return postBody(t, addr, path, writer.FormDataContentType(), body.Bytes())
}

// postBody sends body to path and returns the status and the Problem Detail
// code. A refused connection means the server is not serving yet, so only that
// is retried.
func postBody(t *testing.T, addr, path, contentType string, body []byte) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	var response *http.Response
	require.Eventually(t, func() bool {
		var err error
		response, err = client.Post("http://"+addr+path, contentType, bytes.NewReader(body))
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

func fileField(t *testing.T, size int) func(*multipart.Writer) {
	return func(w *multipart.Writer) {
		part, err := w.CreateFormFile("f", "upload.bin")
		require.NoError(t, err)
		_, err = part.Write(bytes.Repeat([]byte("x"), size))
		require.NoError(t, err)
	}
}

// TestRealMultipartOverflowIs413ThroughTheRealServer drives oversized multipart
// requests through the real composition and shows, per row, which limit fired.
//
// What the experiment established, and these rows pin:
//
//   - Ctx.Bind parses with web.max_multipart_memory before gin's binder runs.
//     gin's ShouldBind would parse with a constant 32 MiB and ignores
//     Engine.MaxMultipartMemory, so the adapter pre-parses; the Bind rows
//     therefore overflow at the configured 8 MiB plus the 10 MiB net/http
//     reserves for non-file data, while a handler's own
//     ParseMultipartForm(1 MiB) overflows at 11 MiB.
//   - normalizeBindError leaves the sentinel alone: it is the bare
//     multipart.ErrMessageTooLarge, not a SliceValidationError, and the same
//     value reaches ParamError.
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
		{name: "bind: non-file field past the configured 8 MiB plus 10 MiB", path: "/bind", build: formField(t, 19<<20), status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", observed: multipartObservation{reached: true, sentinel: true}},
		{name: "bind: part count past the ReadForm limit", path: "/bind", build: manyParts, status: http.StatusRequestEntityTooLarge, code: "request_body_too_large", observed: multipartObservation{reached: true, sentinel: true}},
		{name: "bind: field inside the configured 8 MiB plus 10 MiB", path: "/bind", build: formField(t, 17<<20), status: http.StatusNoContent, observed: multipartObservation{reached: true}},
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

// multipartPayload is one request body and the Content-Type that describes it.
type multipartPayload struct {
	contentType string
	body        []byte
}

func multipartPayloadOf(t *testing.T, build func(*multipart.Writer)) multipartPayload {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	build(writer)
	require.NoError(t, writer.Close())
	return multipartPayload{contentType: writer.FormDataContentType(), body: body.Bytes()}
}

// TestCtxBindHonoursMaxMultipartMemory pins the effect web.max_multipart_memory
// has on Ctx.Bind, which gin's own ShouldBind does not give it: gin parses
// multipart bodies with a constant 32 MiB. net/http caps the accumulated
// non-file data at the configured allowance plus 10 MiB, so a 1 MiB allowance
// rejects past 11 MiB. The 20 MiB row is the observable difference from the old
// behavior: it sits inside gin's 32 MiB + 10 MiB budget and inside this
// server's 64 MiB body cap, so it was accepted before the adapter pre-parsed
// with the configured value.
//
// The remaining rows are the regressions that keep the pre-parse confined to
// the multipart bind path: JSON and urlencoded bodies of a size the same
// allowance would reject if it leaked out of multipart parsing, and a file part
// that spills to disk without being capped by the memory allowance.
func TestCtxBindHonoursMaxMultipartMemory(t *testing.T) {
	addr, probe := serveMultipartAppWith(t, 1<<20)

	tests := []struct {
		name     string
		path     string
		build    func(*testing.T) multipartPayload
		status   int
		code     string
		observed multipartObservation
	}{
		{
			name:   "multipart: non-file field past the configured 1 MiB plus 10 MiB",
			path:   "/bind",
			build:  func(t *testing.T) multipartPayload { return multipartPayloadOf(t, formField(t, 11<<20+512<<10)) },
			status: http.StatusRequestEntityTooLarge, code: "request_body_too_large",
			observed: multipartObservation{reached: true, sentinel: true},
		},
		{
			name:     "multipart: non-file field inside the configured 1 MiB plus 10 MiB",
			path:     "/bind",
			build:    func(t *testing.T) multipartPayload { return multipartPayloadOf(t, formField(t, 10<<20)) },
			status:   http.StatusNoContent,
			observed: multipartObservation{reached: true},
		},
		{
			name:   "multipart: 20 MiB field, inside gin's own 32 MiB budget and past the configured one",
			path:   "/bind",
			build:  func(t *testing.T) multipartPayload { return multipartPayloadOf(t, formField(t, 20<<20)) },
			status: http.StatusRequestEntityTooLarge, code: "request_body_too_large",
			observed: multipartObservation{reached: true, sentinel: true},
		},
		{
			name: "json: body is not subject to the multipart allowance",
			path: "/json",
			build: func(*testing.T) multipartPayload {
				return multipartPayload{contentType: "application/json", body: []byte(`{"value":"` + strings.Repeat("x", 2<<20) + `"}`)}
			},
			status:   http.StatusNoContent,
			observed: multipartObservation{reached: true},
		},
		{
			name: "urlencoded: body is not subject to the multipart allowance",
			path: "/urlencoded",
			build: func(*testing.T) multipartPayload {
				return multipartPayload{contentType: "application/x-www-form-urlencoded", body: []byte(url.Values{"f": {strings.Repeat("x", 1<<20)}}.Encode())}
			},
			status:   http.StatusNoContent,
			observed: multipartObservation{reached: true},
		},
		{
			name:     "multipart: file part spills past the allowance and still binds",
			path:     "/file",
			build:    func(t *testing.T) multipartPayload { return multipartPayloadOf(t, fileField(t, 3<<20)) },
			status:   http.StatusNoContent,
			observed: multipartObservation{reached: true, fileBytes: 3 << 20},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe.take()

			payload := test.build(t)
			status, code := postBody(t, addr, test.path, payload.contentType, payload.body)

			assert.Equal(t, test.status, status)
			assert.Equal(t, test.code, code)
			assert.Equal(t, test.observed, probe.take(),
				"what the handler read must show which limit the row targets")
		})
	}
}
