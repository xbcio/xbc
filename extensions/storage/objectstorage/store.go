package objectstorage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// UnknownSize tells Put that the caller does not know the stream's length.
const UnknownSize int64 = -1

var (
	// ErrInvalidKey means an object key or list prefix is unsafe or malformed.
	ErrInvalidKey = errors.New("objectstorage: invalid key")
	// ErrNotFound means the requested object does not exist.
	ErrNotFound = errors.New("objectstorage: object not found")
	// ErrObjectTooLarge means an upload or download exceeded MaxObjectBytes.
	ErrObjectTooLarge = errors.New("objectstorage: object exceeds size limit")
	// ErrListLimitExceeded means a requested page is larger than MaxListItems.
	ErrListLimitExceeded = errors.New("objectstorage: list limit exceeds configured maximum")
	// ErrSizeMismatch means a stream did not contain its declared number of bytes.
	ErrSizeMismatch = errors.New("objectstorage: stream size does not match declared size")
	// ErrClosed means an operation was attempted after Store.Close.
	ErrClosed = errors.New("objectstorage: store is closed")
)

// ObjectInfo is backend-neutral object metadata. ETag is opaque: callers may
// compare values returned by the same Store, but must not infer its algorithm.
type ObjectInfo struct {
	Key        string
	Size       int64
	ModifiedAt time.Time
	ETag       string
}

// Object is a streaming Get result. The caller must close Body. Reads continue
// to observe the Get context, the configured size limit, and Store.Close.
type Object struct {
	Info ObjectInfo
	Body io.ReadCloser
}

// PutOptions describes one upload. Size must be UnknownSize or a non-negative
// byte count. The Store verifies that the complete stream matches a declared
// size; a backend may still use chunked transfer to prevent silent truncation.
type PutOptions struct {
	Size int64
}

// ListOptions requests one deterministic page. Prefix may be empty. Cursor is
// an opaque value returned by an earlier List call on the same Store. A zero
// Limit uses the Store's configured MaxListItems.
type ListOptions struct {
	Prefix string
	Cursor string
	Limit  int
}

// ListResult is one page of object metadata. NextCursor is empty when no more
// objects remain.
type ListResult struct {
	Objects    []ObjectInfo
	NextCursor string
}

// Store is the vendor-neutral object storage contract. Put and Get stream
// their payloads and enforce MaxObjectBytes without buffering whole objects.
// Delete is idempotent for a missing key. Implementations are safe for
// concurrent use; Close is idempotent and prevents new operations.
type Store interface {
	Put(ctx context.Context, key string, body io.Reader, options PutOptions) (ObjectInfo, error)
	Get(ctx context.Context, key string) (*Object, error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (ObjectInfo, error)
	List(ctx context.Context, options ListOptions) (ListResult, error)
	Close() error
}

func invalidKey(key, reason string) error {
	return fmt.Errorf("%w %q: %s", ErrInvalidKey, key, reason)
}

func notFound(key string, cause error) error {
	if cause == nil {
		return fmt.Errorf("objectstorage: %q: %w", key, ErrNotFound)
	}
	return fmt.Errorf("objectstorage: %q: %w", key, errors.Join(ErrNotFound, cause))
}

func objectTooLarge(key string, max int64) error {
	return fmt.Errorf("objectstorage: %q exceeds %d bytes: %w", key, max, ErrObjectTooLarge)
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("objectstorage: nil context")
	}
	return ctx.Err()
}

// linkedContext is canceled by either the operation context or Store.Close.
func linkedContext(operation, lifetime context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(operation)
	stop := context.AfterFunc(lifetime, cancel)
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			stop()
			cancel()
		})
	}
}

type boundedReader struct {
	ctx  context.Context
	src  io.Reader
	key  string
	max  int64
	read int64
	err  error
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.read < r.max {
		remaining := r.max - r.read
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		n, err := r.src.Read(p)
		r.read += int64(n)
		if err != nil {
			r.err = err
		}
		return n, err
	}

	var probe [1]byte
	n, err := r.src.Read(probe[:])
	if n > 0 {
		r.err = objectTooLarge(r.key, r.max)
		return 0, r.err
	}
	r.err = err
	return 0, err
}

// exactSizeReader verifies a declared stream length while it is still being
// transmitted. It deliberately does not advertise a Content-Length: the HTTP
// transport must read through the validation boundary, so both short and long
// non-seekable streams abort before a backend can accept a successful upload.
type exactSizeReader struct {
	ctx      context.Context
	src      io.Reader
	key      string
	expected int64
	read     int64
	err      error
}

func (r *exactSizeReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.read < r.expected {
		remaining := r.expected - r.read
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
		n, err := r.src.Read(p)
		r.read += int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) && r.read < r.expected {
				err = declaredSizeError(r.key, r.expected, r.read)
			}
			r.err = err
		}
		return n, err
	}

	var probe [1]byte
	n, err := r.src.Read(probe[:])
	if n > 0 {
		r.read += int64(n)
		r.err = declaredSizeError(r.key, r.expected, r.read)
		return 0, r.err
	}
	if err != nil {
		r.err = err
	}
	return 0, err
}

func declaredSizeError(key string, declared, streamed int64) error {
	return fmt.Errorf("objectstorage: %q declared %d bytes but streamed %d: %w", key, declared, streamed, ErrSizeMismatch)
}

type managedBody struct {
	reader  io.Reader
	closer  io.Closer
	cleanup func()
	onClose func(*managedBody)

	once sync.Once
	err  error
}

func (b *managedBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *managedBody) Close() error {
	b.once.Do(func() {
		defer func() {
			if b.cleanup != nil {
				b.cleanup()
			}
			if b.onClose != nil {
				b.onClose(b)
			}
		}()
		if b.closer == nil {
			b.err = errors.New("objectstorage: response body has no closer")
			return
		}
		b.err = b.closer.Close()
	})
	return b.err
}
