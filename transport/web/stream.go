package web

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// StreamWriter is the writer BeginStream returns. Write flushes; Flush is
// exposed for callers that write through several calls.
//
// The interface is deliberately narrower than any engine's writer: it speaks
// only io.Writer and http.Flusher vocabulary, so a handler that streams stays
// portable across engines and the framework stays free of engine imports.
type StreamWriter interface {
	io.Writer
	Flush()
}

// streamWriter is the StreamWriter BeginStream returns. It holds the neutral
// writer and nothing else, and both methods have value receivers, so a call
// through StreamWriter copies one interface word and allocates nothing.
type streamWriter struct {
	w ResponseWriter
}

// Write delegates to the neutral writer and then flushes whatever it accepted.
//
// The flush happens even when the write reported an error. A partial write
// that stays buffered would leave the client with bytes the handler already
// considers sent, and the error is the caller's signal to stop writing, not a
// reason to strand the bytes that did get through.
func (s streamWriter) Write(data []byte) (int, error) {
	n, err := s.w.Write(data)
	s.w.Flush()
	return n, err
}

// Flush forwards to the neutral writer, which is where an engine's own flush
// (and any buffering wrapper's commit-then-forward) lives.
func (s streamWriter) Flush() { s.w.Flush() }

// BeginStream commits a streaming response and returns a writer that flushes
// after every write.
//
// On success it sets Content-Type when contentType is non-empty, records the
// status with WriteHeader, and then flushes to commit it. The flush is not
// decorative: the neutral writer contract commits on WriteHeader, but gin's
// own writer is two-phase -- WriteHeader only records a pending status and the
// engine commits it from WriteHeaderNow, which Flush calls. Flushing here is
// what makes the headers reach the client before the first body byte, which in
// turn is what lets the gzip and timeout middleware recognize a streaming
// response and switch to their pass-through paths.
//
// Once the headers are committed, BeginStream lifts the connection's write
// deadline with http.NewResponseController. WriteTimeout is one absolute
// deadline for a whole response, so a stream longer than web.write_timeout
// would otherwise be severed mid-body; the lift is per response, so ordinary
// requests keep their bound. http.ErrNotSupported is tolerated: a writer with
// no deadline support (a recorder, or an engine without a real network
// connection) has no deadline to lift. Every other error is returned, because
// the caller must know the deadline is still in force. The response is already
// committed at that point, so the error cannot be rendered; return it from the
// Handler and let the error boundary log it.
//
// Lifting the write deadline does not lift the timeout plugin's context
// deadline. When plugins.timeout composes a timeout middleware, its 504 is
// abandoned once the stream begins, but the handler's context is still
// cancelled at plugins.timeout.duration unless the request carries
// "Accept: text/event-stream" or an Upgrade, or the path or route is excluded
// from the middleware. A streaming handler must treat context cancellation as
// its own stop signal and return.
//
// BeginStream refuses a nil Ctx, a Ctx with no writer, and a response that is
// already committed: re-committing would either be ignored or corrupt the
// status line, so the second call reports "response already written" instead.
//
// After BeginStream returns, the response is committed and a subsequent
// handler error can no longer be rendered -- the error boundary logs it and
// does not write. A streaming handler that detects a failure must send an
// error event (or write a chunk) before returning.
//
// Streaming does not combine with the idempotency plugin: that plugin captures
// a bounded response body so it can replay it, and a stream cannot be
// captured, so a route must not enable both. The helper is also
// protocol-neutral: it sets no Connection header and reaches the connection
// through http.ResponseController, which HTTP/2 implements.
func (c *Ctx) BeginStream(status int, contentType string) (StreamWriter, error) {
	if c == nil {
		return nil, errors.New("web: BeginStream requires a non-nil Ctx")
	}
	writer := c.Writer()
	if writer == nil {
		return nil, errors.New("web: BeginStream requires a non-nil response writer")
	}
	if writer.Written() {
		return nil, errors.New("web: BeginStream cannot start: response already written")
	}
	if contentType != "" {
		writer.Header().Set("Content-Type", contentType)
	}
	writer.WriteHeader(status)
	// Commit the status and headers. See the method comment for why a writer
	// that records the status until the engine commits needs this.
	writer.Flush()

	if err := http.NewResponseController(writer).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return nil, fmt.Errorf("web: BeginStream cannot lift the write deadline: %w", err)
	}
	return streamWriter{w: writer}, nil
}

// SSE encodes Server-Sent Events over a stream begun by BeginStream. Obtain
// one from Ctx.SSE.
type SSE struct {
	w StreamWriter
}

// SSE begins a text/event-stream response with the headers an event stream
// needs and returns an encoder for it.
//
// The three headers are set before BeginStream commits the response, because a
// header set after the status line never reaches the client: Content-Type
// announces the event stream, Cache-Control stops an intermediary caching it,
// and X-Accel-Buffering asks nginx not to buffer the response (a proxy hint
// that is harmless when no such proxy is in front). The stream is begun with
// status 200: once the status line is committed, it cannot change.
//
// Like BeginStream, SSE refuses a nil Ctx and commits the response. An
// encoder failure after this point is reported to the client as an error event
// by the handler, not as an HTTP status.
func (c *Ctx) SSE() (*SSE, error) {
	if c == nil {
		return nil, errors.New("web: SSE requires a non-nil Ctx")
	}
	c.SetHeader("Content-Type", "text/event-stream")
	c.SetHeader("Cache-Control", "no-cache")
	c.SetHeader("X-Accel-Buffering", "no")
	writer, err := c.BeginStream(http.StatusOK, "text/event-stream")
	if err != nil {
		return nil, err
	}
	return &SSE{w: writer}, nil
}

// Send writes one event and flushes it.
//
// The wire form follows the SSE grammar: an optional "event:" line, one
// "data:" line per line of data (the rule that carries newlines in a
// line-oriented protocol), a blank line terminating the event, and a flush so
// the event reaches the client without waiting for the next one.
//
// An event name, when present, must be an HTTP token. Anything else could
// introduce a field separator or a second line into the framing, so it is
// refused before any byte is written. Data is split on CRLF, CR, or LF -- every
// line terminator the SSE grammar recognizes -- with one "data:" line per
// segment, and every other byte is written verbatim: it is never re-encoded
// and never interpreted. Splitting on a bare CR is what keeps payload text
// from forging a field, because a client reads a CR inside a data line as the
// end of that line. An empty segment -- the empty data argument, or the empty
// tail after a trailing terminator -- is a valid line and is written as
// "data: " followed by the newline.
func (s *SSE) Send(event string, data []byte) error {
	if s == nil || s.w == nil {
		return errors.New("web: SSE Send requires an encoder from Ctx.SSE")
	}
	if event != "" && !validEventName(event) {
		return fmt.Errorf("web: SSE event name %q is not a valid token", event)
	}

	// One Write for the whole event keeps the framing in a single chunk (and
	// therefore a single flush, which StreamWriter.Write performs) instead of
	// one flush per field line.
	frame := make([]byte, 0, len(event)+len(data)+16)
	if event != "" {
		frame = append(frame, "event: "...)
		frame = append(frame, event...)
		frame = append(frame, '\n')
	}
	frame = appendSSEDataLines(frame, data)
	frame = append(frame, '\n')

	if _, err := s.w.Write(frame); err != nil {
		return fmt.Errorf("web: SSE write event: %w", err)
	}
	return nil
}

// appendSSEDataLines appends one "data: " line per line of data. Per the SSE
// line grammar a line ends at CRLF, CR, or LF, so the payload is split on any
// of them; nothing else in the payload is inspected, and every other byte is
// preserved verbatim. Splitting on a bare CR is a safety property, not only a
// grammar one: a client reads "data: x\revent: hijack" as a data line followed
// by an "event:" field, so a CR left inside the payload would let untrusted
// text forge protocol fields and truncate the data.
//
// An empty segment -- empty data, a leading terminator, two adjacent
// terminators, or the empty tail after a trailing terminator -- is written as
// a bare "data: " line, which carries an empty data value.
func appendSSEDataLines(frame, data []byte) []byte {
	for {
		frame = append(frame, "data: "...)
		terminator := -1
		for i := 0; i < len(data); i++ {
			if data[i] == '\r' || data[i] == '\n' {
				terminator = i
				break
			}
		}
		if terminator < 0 {
			frame = append(frame, data...)
			frame = append(frame, '\n')
			return frame
		}
		frame = append(frame, data[:terminator]...)
		frame = append(frame, '\n')
		// A CRLF pair is one terminator; consuming both keeps the LF from
		// producing a spurious empty data line.
		if data[terminator] == '\r' && terminator+1 < len(data) && data[terminator+1] == '\n' {
			data = data[terminator+2:]
		} else {
			data = data[terminator+1:]
		}
	}
}

// Comment writes a comment line and flushes it. It is the documented way to
// keep an idle stream alive through a proxy that would otherwise close a
// quiet connection: SSE clients ignore a line beginning with ':'.
//
// The text must not contain a control character. CR and LF would terminate
// the comment line and splice the rest into the event framing; every other
// control character is refused for the same reason the event token is
// validated -- an encoder must not turn caller data into protocol syntax.
func (s *SSE) Comment(text string) error {
	if s == nil || s.w == nil {
		return errors.New("web: SSE Comment requires an encoder from Ctx.SSE")
	}
	if !validComment(text) {
		return errors.New("web: SSE comment text contains a control character")
	}

	frame := make([]byte, 0, len(text)+4)
	frame = append(frame, ':', ' ')
	frame = append(frame, text...)
	frame = append(frame, '\n', '\n')

	if _, err := s.w.Write(frame); err != nil {
		return fmt.Errorf("web: SSE write comment: %w", err)
	}
	return nil
}

// eventTokenPunctuation is the punctuation RFC 9110 allows in a token, which
// is the grammar an SSE event name has to satisfy.
const eventTokenPunctuation = "!#$%&'*+-.^_`|~"

// validEventName reports whether event is an HTTP token: one or more letters,
// digits, or token punctuation characters, and nothing else. The caller only
// reaches it for a non-empty name; an empty name means "no event field".
func validEventName(event string) bool {
	for i := 0; i < len(event); i++ {
		c := event[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte(eventTokenPunctuation, c) >= 0:
		default:
			return false
		}
	}
	return true
}

// validComment reports whether text can be written as one SSE comment line.
// unicode.IsControl covers CR, LF, the rest of the C0 range, DEL, and the C1
// controls; none of them may appear, because each could terminate the comment
// and turn the remaining bytes into protocol syntax.
func validComment(text string) bool {
	for _, r := range text {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
