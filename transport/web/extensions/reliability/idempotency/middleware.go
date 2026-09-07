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
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/xbcio/xbc/transport/web"
)

// Handler implements web.Middleware.
func (p *Plugin) Handler() gin.HandlerFunc { return p.handle }

// Order implements web.Middleware. It runs in PhaseBusiness so authentication
// has already published the principal included in the fingerprint.
func (p *Plugin) Order() web.Order {
	return web.Order{Phase: web.PhaseBusiness}
}

func (p *Plugin) handle(c *gin.Context) {
	route, found := web.CurrentRoute(c)
	if !found || !route.Idempotent {
		c.Next()
		return
	}
	state := p.state
	key, ok := idempotencyHeader(c, state.config.header)
	if !ok || !validIdempotencyKey(key, state.config.minKeyLength, state.config.maxKeyLength) {
		abortJSON(c, http.StatusBadRequest, "invalid_idempotency_key")
		return
	}
	body, err := readBody(c.Request, state.config.maxRequestBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			abortJSON(c, http.StatusRequestEntityTooLarge, "request_too_large")
		} else {
			abortJSON(c, http.StatusBadRequest, "invalid_request_body")
		}
		return
	}
	principal, _ := web.CurrentPrincipal(c)
	rawQuery := ""
	contentType := ""
	if c.Request.URL != nil {
		rawQuery = c.Request.URL.RawQuery
	}
	contentType = strings.TrimSpace(c.GetHeader("Content-Type"))
	fingerprint := requestFingerprint(c.Request.Method, route.Path, principal.Subject, rawQuery, contentType, body)
	storageKey := digestParts(c.Request.Method, route.Path, principal.Subject, key)
	owner, err := newOwner()
	if err != nil {
		abortJSON(c, http.StatusInternalServerError, "idempotency_unavailable")
		return
	}

	result, err := state.store.Acquire(c.Request.Context(), storageKey, fingerprint, owner, state.config.pendingTTL)
	if err != nil {
		state.logger.Error("idempotency acquire failed", "error", err)
		abortJSON(c, http.StatusServiceUnavailable, "idempotency_unavailable")
		return
	}
	switch result.State {
	case Pending:
		abortJSON(c, state.config.pendingStatus, "request_in_progress")
		return
	case Conflict:
		abortJSON(c, http.StatusConflict, "idempotency_conflict")
		return
	case Completed:
		if !validReplay(result.Response, state.config.maxResponseBytes) {
			state.logger.Error("idempotency store returned an invalid replay response")
			abortJSON(c, http.StatusServiceUnavailable, "idempotency_unavailable")
			return
		}
		replay(c, result.Response)
		return
	case Acquired:
		p.executeOwned(c, state, storageKey, fingerprint, owner)
	default:
		abortJSON(c, http.StatusServiceUnavailable, "idempotency_unavailable")
	}
}

func (p *Plugin) executeOwned(c *gin.Context, state *runtimeState, key, fingerprint, owner string) {
	writer := &captureWriter{ResponseWriter: c.Writer, limit: state.config.maxResponseBytes}
	c.Writer = writer
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cancel := detachedTimeout(c.Request.Context(), state.config.operationTimeout)
		defer cancel()
		if err := state.store.Release(cleanupCtx, key, fingerprint, owner); err != nil && !errors.Is(err, ErrOwnershipLost) {
			state.logger.Error("idempotency release failed", "error", err)
		}
	}()

	c.Next()
	if c.Request.Context().Err() != nil || writer.tooLarge || writer.Status() >= http.StatusInternalServerError {
		return
	}
	response := Response{
		Status:      writer.Status(),
		ContentType: safeContentType(writer.Header().Get("Content-Type")),
		Body:        append([]byte(nil), writer.body.Bytes()...),
	}
	completeCtx, cancel := detachedTimeout(c.Request.Context(), state.config.operationTimeout)
	defer cancel()
	if err := state.store.Complete(completeCtx, key, fingerprint, owner, response, state.config.ttl); err != nil {
		state.logger.Error("idempotency completion failed", "error", err)
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

func requestFingerprint(method, route, subject, rawQuery, contentType string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	hash := sha256.New()
	writeFingerprintPart(hash, strings.ToUpper(method))
	writeFingerprintPart(hash, route)
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

func idempotencyHeader(c *gin.Context, name string) (string, bool) {
	if c == nil || c.Request == nil {
		return "", false
	}
	values := c.Request.Header.Values(name)
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

func validReplay(response Response, maximum int64) bool {
	return response.Status >= 200 && response.Status < 500 &&
		int64(len(response.Body)) <= maximum && len(response.ContentType) <= 1024 && !containsControl(response.ContentType)
}

func replay(c *gin.Context, response Response) {
	c.Header("Idempotency-Replayed", "true")
	if contentType := safeContentType(response.ContentType); contentType != "" {
		c.Header("Content-Type", contentType)
	}
	c.Status(response.Status)
	if len(response.Body) != 0 {
		_, _ = c.Writer.Write(response.Body)
	}
	c.Abort()
}

func safeContentType(value string) string {
	if containsControl(value) {
		return ""
	}
	return strings.TrimSpace(value)
}

func abortJSON(c *gin.Context, status int, code string) {
	web.AbortProblem(c, web.NewProblem(status, code))
}

func detachedTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
}

type captureWriter struct {
	gin.ResponseWriter
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

func (w *captureWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}
