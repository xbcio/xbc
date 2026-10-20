package idempotency

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/xbcio/xbc/extensions/authentication"
	"github.com/xbcio/xbc/transport/web"
)

// Plugin declares authentication.RequiresPrincipal at compile time. idempotency
// does not otherwise import the authentication package, so it satisfies the
// interface structurally; without this assertion, an upstream rename of the
// marker method would silently unbind it and this package would still build.
var _ authentication.RequiresPrincipal = (*Plugin)(nil)

// Handler implements web.Middleware.
func (p *Plugin) Handler() web.Handler { return p.handle }

// Order implements web.Middleware. It runs in PhaseBusiness so authentication
// has already published the principal included in the fingerprint.
func (p *Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseBusiness}
}

// RequiresPrincipal declares that this middleware cannot usefully run before
// an identity is published, which makes the framework pin it after the
// authentication middleware automatically. The PhaseBusiness/PhaseAuth hard
// boundary already guarantees this ordering; this marker documents the
// dependency at the type level too, matching tenant's own assertion (casbin
// and auditlog deliberately do not declare it -- both must keep serving
// requests that carry no principal). It does not change per-route behavior:
// a route not marked Route.Idempotent, or one that resolved to permit, still
// never reaches the no-Principal refusal in handle.
func (*Plugin) RequiresPrincipal() {}

func (p *Plugin) handle(_ context.Context, c *web.Ctx) error {
	route, found := web.CurrentRoute(c)
	if !found || !route.Idempotent {
		c.Next()
		return nil
	}
	state := p.state
	key, ok := idempotencyHeader(c, state.config.header)
	if !ok || !validIdempotencyKey(key, state.config.minKeyLength, state.config.maxKeyLength) {
		abortJSON(c, http.StatusBadRequest, "invalid_idempotency_key")
		return nil
	}
	principal, hasPrincipal := web.CurrentPrincipal(c)
	if !hasPrincipal {
		// A stored response is keyed and fingerprinted by caller identity (see
		// requestFingerprint and digestParts below). Without a Principal there is
		// no caller identity to scope the key to, so two unrelated callers reusing
		// the same Idempotency-Key value would share one storage slot and one
		// could replay the other's response. The request never reaches
		// state.store below: refusing here, before Acquire, is what keeps an
		// identity-less caller from ever occupying or reading a slot in the
		// store's namespace.
		abortJSON(c, http.StatusForbidden, "idempotency_requires_principal")
		return nil
	}
	body, err := readBody(c.Request(), state.config.maxRequestBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			abortJSON(c, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			abortJSON(c, http.StatusBadRequest, "invalid_request_body")
		}
		return nil
	}
	requestPath := ""
	rawQuery := ""
	if c.Request().URL != nil {
		requestPath = c.Request().URL.Path
		rawQuery = c.Request().URL.RawQuery
	}
	contentType := strings.TrimSpace(c.GetHeader("Content-Type"))
	// Scope stored responses to the concrete path the engine routed, not to the
	// route template: one template covers every path-parameter value, and a key
	// reused across two of them must execute both requests. The template stays
	// available on route.Path as a diagnostic field only.
	fingerprint := requestFingerprint(c.Request().Method, requestPath, principal.Subject, rawQuery, contentType, body)
	storageKey := digestParts(c.Request().Method, requestPath, principal.Subject, key)
	owner, err := newOwner()
	if err != nil {
		abortJSON(c, http.StatusInternalServerError, "idempotency_unavailable")
		return nil
	}

	result, err := state.store.Acquire(c.Request().Context(), storageKey, fingerprint, owner, state.config.pendingTTL)
	if err != nil {
		state.logger.Error("idempotency acquire failed", "error", err, "route", route.Path)
		abortJSON(c, http.StatusServiceUnavailable, "idempotency_unavailable")
		return nil
	}
	switch result.State {
	case Pending:
		abortJSON(c, state.config.pendingStatus, "request_in_progress")
		return nil
	case Conflict:
		abortJSON(c, http.StatusConflict, "idempotency_conflict")
		return nil
	case Completed:
		if !validReplay(result.Response, state.config.maxResponseBytes) {
			state.logger.Error("idempotency store returned an invalid replay response")
			abortJSON(c, http.StatusServiceUnavailable, "idempotency_unavailable")
			return nil
		}
		replay(c, result.Response)
		return nil
	case Acquired:
		p.executeOwned(c, state, storageKey, fingerprint, owner, route.Path)
	default:
		abortJSON(c, http.StatusServiceUnavailable, "idempotency_unavailable")
	}
	return nil
}

func (p *Plugin) executeOwned(c *web.Ctx, state *runtimeState, key, fingerprint, owner, routePath string) {
	original := c.Writer()
	writer := &captureWriter{ResponseWriter: original, limit: state.config.maxResponseBytes}
	c.SetWriter(writer)
	defer c.SetWriter(original)
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := detachedTimeout(c.Request().Context(), state.config.operationTimeout)
		defer cancel()
		if err := state.store.Release(cleanupCtx, key, fingerprint, owner); err != nil && !errors.Is(err, ErrOwnershipLost) {
			state.logger.Error("idempotency release failed", "error", err, "route", routePath)
		}
	}()

	c.Next()
	if c.Request().Context().Err() != nil || writer.tooLarge || writer.Status() >= http.StatusInternalServerError {
		return
	}
	// Headers are captured after the handler returns, not at commit time. A
	// handler that sets a whitelisted header after committing its response has
	// that value persisted and replayed even though the original client did not
	// receive it. A commit-time snapshot is deliberately not used: a bodyless
	// response (e.g. 204 with Location) commits only after the middleware chain
	// returns, and a snapshot at the wrapper's Write/WriteHeader would drop
	// those headers entirely.
	response := Response{
		Status:      writer.Status(),
		ContentType: replayableContentType(writer.Header().Get("Content-Type")),
		Header:      captureReplayHeaders(writer.Header()),
		Body:        append([]byte(nil), writer.body.Bytes()...),
	}
	completeCtx, cancel := detachedTimeout(c.Request().Context(), state.config.operationTimeout)
	defer cancel()
	if err := state.store.Complete(completeCtx, key, fingerprint, owner, response, state.config.ttl); err != nil {
		state.logger.Error("idempotency completion failed", "error", err, "route", routePath)
		return
	}
	committed = true
}

var errBodyTooLarge = errors.New("idempotency: request body too large")

func readBody(request *http.Request, maximum int64) ([]byte, error) {
	if request == nil || request.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maximum+1))
	closeErr := request.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > maximum {
		return nil, errBodyTooLarge
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	return body, nil
}

// requestFingerprint hashes the request identity a stored response is scoped
// to. path is the concrete request path, never the route template: two
// different path-parameter values routed through one template are different
// requests and must not share a fingerprint.
func requestFingerprint(method, path, subject, rawQuery, contentType string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	hash := sha256.New()
	writeFingerprintPart(hash, strings.ToUpper(method))
	writeFingerprintPart(hash, path)
	writeFingerprintPart(hash, subject)
	writeFingerprintPart(hash, rawQuery)
	writeFingerprintPart(hash, contentType)
	_, _ = hash.Write(bodyHash[:])
	return hex.EncodeToString(hash.Sum(nil))
}

func writeFingerprintPart(writer io.Writer, value string) {
	_, _ = fmt.Fprintf(writer, "%d:", len(value))
	_, _ = io.WriteString(writer, value)
}

func digestParts(values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		writeFingerprintPart(hash, value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func newOwner() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func idempotencyHeader(c *web.Ctx, name string) (string, bool) {
	if c == nil || c.Request() == nil {
		return "", false
	}
	values := c.Request().Header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := values[0]
	if value == "" || value != strings.TrimSpace(value) {
		return "", false
	}
	return value, true
}

func validIdempotencyKey(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for i := range len(value) {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			continue
		}
		switch ch {
		case '-', '_', '.', ':':
			continue
		default:
			return false
		}
	}
	return true
}

// replayHeaderNames is the response-header whitelist persisted with a stored
// response. Content-Type is carried separately by Response.ContentType, which
// is also the only header a record written before this whitelist existed can
// replay. Set-Cookie is deliberately absent: replaying a cookie would attach
// one caller's session to a later retry, so it is never persisted at all, not
// merely filtered on the way out.
//
// The entries use the canonical spelling http.Header stores; ETag canonicalizes
// to Etag, and http.CanonicalHeaderKey folds differently-cased stored names
// onto the same entry.
var replayHeaderNames = []string{
	"Location",
	"Content-Language",
	"Content-Disposition",
	http.CanonicalHeaderKey("ETag"),
	"Last-Modified",
	"Cache-Control",
}

const (
	maxReplayHeaderValueBytes = 1024
	maxReplayHeaderTotalBytes = 8192
)

// captureReplayHeaders copies the whitelisted headers out of a finished
// response. Values a header writer would have to sanitize -- empty values and
// values carrying control characters -- are dropped at the source, and the same
// per-value and total bounds replay validation enforces are applied here. The
// two must agree: a value admitted here but refused at replay would turn every
// retry of that key into a 503 for the record's whole TTL.
func captureReplayHeaders(header http.Header) http.Header {
	captured := make(http.Header)
	total := 0
	for _, name := range replayHeaderNames {
		for _, value := range header.Values(name) {
			if value == "" || containsControl(value) || len(value) > maxReplayHeaderValueBytes {
				continue
			}
			if total+len(name)+len(value) > maxReplayHeaderTotalBytes {
				continue
			}
			total += len(name) + len(value)
			captured[name] = append(captured[name], value)
		}
	}
	if len(captured) == 0 {
		return nil
	}
	return captured
}

// replayableContentType applies the capture-side bound to Content-Type, which
// is persisted in its own field rather than in the header map.
func replayableContentType(value string) string {
	value = safeContentType(value)
	if len(value) > maxReplayHeaderValueBytes {
		return ""
	}
	return value
}

func isReplayHeader(name string) bool {
	return slices.Contains(replayHeaderNames, http.CanonicalHeaderKey(name))
}

func validReplay(response Response, maximum int64) bool {
	return response.Status >= 200 && response.Status < 500 &&
		int64(len(response.Body)) <= maximum && len(response.ContentType) <= maxReplayHeaderValueBytes && !containsControl(response.ContentType) &&
		validReplayHeaders(response.Header)
}

// validReplayHeaders bounds what a corrupted or hostile Store implementation
// can inject into a replayed response: only whitelisted names, no control
// characters, at most 1 KiB per value and 8 KiB in total.
func validReplayHeaders(header http.Header) bool {
	total := 0
	for name, values := range header {
		if !isReplayHeader(name) || len(values) == 0 {
			return false
		}
		for _, value := range values {
			if containsControl(value) || len(value) > maxReplayHeaderValueBytes {
				return false
			}
			total += len(name) + len(value)
		}
	}
	return total <= maxReplayHeaderTotalBytes
}

func replay(c *web.Ctx, response Response) {
	c.SetHeader("Idempotency-Replayed", "true")
	if contentType := safeContentType(response.ContentType); contentType != "" {
		c.SetHeader("Content-Type", contentType)
	}
	if writer := c.Writer(); writer != nil {
		for name, values := range response.Header {
			if !isReplayHeader(name) {
				continue
			}
			canonical := http.CanonicalHeaderKey(name)
			writer.Header().Del(canonical)
			for _, value := range values {
				writer.Header().Add(canonical, value)
			}
		}
	}
	c.Status(response.Status)
	if len(response.Body) != 0 {
		_, _ = c.Writer().Write(response.Body)
	}
	c.Abort()
}

func safeContentType(value string) string {
	if containsControl(value) {
		return ""
	}
	return strings.TrimSpace(value)
}

func abortJSON(c *web.Ctx, status int, code string) {
	web.AbortProblem(c, web.NewProblem(status, code))
}

func detachedTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

type captureWriter struct {
	web.ResponseWriter
	body     bytes.Buffer
	limit    int64
	tooLarge bool
}

func (w *captureWriter) Write(data []byte) (int, error) {
	if !w.tooLarge {
		remaining := w.limit - int64(w.body.Len())
		if int64(len(data)) <= remaining {
			_, _ = w.body.Write(data)
		} else {
			w.tooLarge = true
			w.body.Reset()
		}
	}
	return w.ResponseWriter.Write(data)
}

// Unwrap exposes the writer beneath so http.ResponseController can still reach
// capabilities web.ResponseWriter leaves out, Hijack among them.
func (w *captureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

var _ web.ResponseWriter = (*captureWriter)(nil)
