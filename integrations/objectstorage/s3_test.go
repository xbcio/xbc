package objectstorage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestS3CompatibleStoreCRUDPrefixPaginationAndSigning(t *testing.T) {
	fake := newFakeS3("objects")
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store := testS3Store(t, server.URL, func(cfg *Config) {
		cfg.S3.Prefix = "tenant/assets"
		cfg.MaxListItems = 2
	})
	ctx := context.Background()

	info, err := store.Put(ctx, "docs/a.txt", &oneByteReader{data: []byte("alpha")}, PutOptions{Size: 5})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if info.Key != "docs/a.txt" || info.Size != 5 || info.ETag == "" {
		t.Fatalf("Put() info = %+v", info)
	}
	fake.mu.Lock()
	stored := string(fake.objects["tenant/assets/docs/a.txt"].data)
	fake.mu.Unlock()
	if stored != "alpha" {
		t.Fatalf("backend object = %q", stored)
	}

	stat, err := store.Stat(ctx, "docs/a.txt")
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if stat.Key != "docs/a.txt" || stat.Size != 5 || stat.ETag != info.ETag || stat.ModifiedAt.IsZero() {
		t.Fatalf("Stat() = %+v", stat)
	}
	object, err := store.Get(ctx, "docs/a.txt")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	body, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if err := object.Body.Close(); err != nil {
		t.Fatalf("Body.Close() error = %v", err)
	}
	if string(body) != "alpha" || object.Info.Key != "docs/a.txt" || object.Info.ETag != info.ETag {
		t.Fatalf("Get() = %q, %+v", body, object.Info)
	}

	for key, value := range map[string]string{"docs/b.txt": "bravo", "docs/c.txt": "charlie", "other/z.txt": "zulu"} {
		if _, err := store.Put(ctx, key, strings.NewReader(value), PutOptions{Size: UnknownSize}); err != nil {
			t.Fatalf("Put(%q) error = %v", key, err)
		}
	}
	first, err := store.List(ctx, ListOptions{Prefix: "docs/", Limit: 2})
	if err != nil {
		t.Fatalf("first List() error = %v", err)
	}
	if got := infoKeys(first.Objects); !equalStrings(got, []string{"docs/a.txt", "docs/b.txt"}) || first.NextCursor == "" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := store.List(ctx, ListOptions{Prefix: "docs/", Cursor: first.NextCursor, Limit: 2})
	if err != nil {
		t.Fatalf("second List() error = %v", err)
	}
	if got := infoKeys(second.Objects); !equalStrings(got, []string{"docs/c.txt"}) || second.NextCursor != "" {
		t.Fatalf("second page = %+v", second)
	}
	if _, err := store.List(ctx, ListOptions{Limit: 3}); !errors.Is(err, ErrListLimitExceeded) {
		t.Fatalf("oversized List() error = %v", err)
	}

	if err := store.Delete(ctx, "docs/a.txt"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "docs/a.txt"); err != nil {
		t.Fatalf("idempotent Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "docs/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get() error = %v", err)
	}
	if _, err := store.Stat(ctx, "docs/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Stat() error = %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.unsigned != 0 || fake.requests == 0 {
		t.Fatalf("signed requests = %d/%d", fake.requests-fake.unsigned, fake.requests)
	}
	for _, requestPath := range fake.paths {
		if !strings.HasPrefix(requestPath, "/objects/") && requestPath != "/objects" {
			t.Fatalf("non-path-style request %q", requestPath)
		}
	}
}

func TestS3TLSStreamsNonSeekableBodyWithUnsignedPayload(t *testing.T) {
	fake := newFakeS3("objects")
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)
	cfg := defaultConfig()
	cfg.Backend = BackendS3
	cfg.S3.Endpoint = server.URL
	cfg.S3.TLS = true
	cfg.S3.SkipVerify = true
	cfg.S3.Bucket = "objects"
	cfg.S3.AccessKeyID = "access"
	cfg.S3.SecretKey = "secret"
	cfg.S3.Prefix = "tenant///"
	store, err := newS3Store(cfg)
	if err != nil {
		t.Fatalf("newS3Store() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.prefix != "tenant" {
		t.Fatalf("normalized prefix = %q, want tenant", store.prefix)
	}
	if _, err := store.Put(context.Background(), "stream", &oneByteReader{data: []byte("payload")}, PutOptions{Size: 7}); err != nil {
		t.Fatalf("Put(non-seekable TLS body) error = %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.unsigned != 0 || fake.unsignedPayload != 1 {
		t.Fatalf("TLS request signing: unsigned auth=%d, unsigned-payload headers=%d", fake.unsigned, fake.unsignedPayload)
	}
	if got := string(fake.objects["tenant/stream"].data); got != "payload" {
		t.Fatalf("stored TLS payload = %q", got)
	}
}

func TestS3CompatibleStoreLimitsContextAndClose(t *testing.T) {
	fake := newFakeS3("objects")
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store := testS3Store(t, server.URL, func(cfg *Config) {
		cfg.MaxObjectBytes = 4
		cfg.S3.RequestTimeout = 50 * time.Millisecond
	})
	ctx := context.Background()

	if _, err := store.Put(ctx, "declared", strings.NewReader("12345"), PutOptions{Size: 5}); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("declared oversized Put() error = %v", err)
	}
	if _, err := store.Put(ctx, "streamed", strings.NewReader("12345"), PutOptions{Size: UnknownSize}); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("streamed oversized Put() error = %v", err)
	}
	if _, err := store.Put(ctx, "short", strings.NewReader("12"), PutOptions{Size: 3}); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("short Put() error = %v", err)
	}
	if _, err := store.Put(ctx, "long", strings.NewReader("123"), PutOptions{Size: 2}); !errors.Is(err, ErrSizeMismatch) {
		t.Fatalf("long Put() error = %v", err)
	}
	for _, key := range []string{"short", "long"} {
		if _, exists := fake.get(key); exists {
			t.Fatalf("size-mismatched upload %q was committed", key)
		}
	}

	fake.set("oversized", []byte("12345"))
	if _, err := store.Get(ctx, "oversized"); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("oversized Get() error = %v", err)
	}
	if _, err := store.Stat(ctx, "oversized"); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("oversized Stat() error = %v", err)
	}

	fake.mu.Lock()
	fake.delayKey = "slow"
	fake.delay = 250 * time.Millisecond
	fake.mu.Unlock()
	if _, err := store.Stat(ctx, "slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request timeout error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.List(cancelled, ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled List() error = %v", err)
	}

	fake.set("open", []byte("1234"))
	object, err := store.Get(ctx, "open")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("repeated Close() error = %v", err)
	}
	var one [1]byte
	if _, err := object.Body.Read(one[:]); !errors.Is(err, context.Canceled) {
		t.Fatalf("outstanding body after Close error = %v", err)
	}
	if _, err := store.Stat(ctx, "open"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Stat after Close error = %v", err)
	}
}

func TestS3ResponseValidationAndCloseErrors(t *testing.T) {
	closeErr := errors.New("response close failed")
	api := &stubS3API{}
	store := newStubS3Store(api, 4)

	api.put = func(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
		return nil, nil
	}
	if _, err := store.Put(context.Background(), "empty-put", strings.NewReader("x"), PutOptions{Size: 1}); err == nil {
		t.Fatal("Put with nil output error = nil")
	}

	api.get = func(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
		return nil, nil
	}
	if _, err := store.Get(context.Background(), "empty-get"); err == nil {
		t.Fatal("Get with nil output error = nil")
	}
	api.get = func(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
		return &awss3.GetObjectOutput{}, nil
	}
	if _, err := store.Get(context.Background(), "nil-body"); err == nil {
		t.Fatal("Get with nil body error = nil")
	}

	api.get = func(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
		return &awss3.GetObjectOutput{
			Body:          &errorReadCloser{Reader: strings.NewReader("12345"), err: closeErr},
			ContentLength: aws.Int64(5),
		}, nil
	}
	if _, err := store.Get(context.Background(), "oversized"); !errors.Is(err, ErrObjectTooLarge) || !errors.Is(err, closeErr) {
		t.Fatalf("oversized Get() error = %v", err)
	}

	api.head = func(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
		return nil, nil
	}
	if _, err := store.Stat(context.Background(), "empty-stat"); err == nil {
		t.Fatal("Stat with nil output error = nil")
	}
	api.list = func(context.Context, *awss3.ListObjectsV2Input, ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
		return nil, nil
	}
	if _, err := store.List(context.Background(), ListOptions{}); err == nil {
		t.Fatal("List with nil output error = nil")
	}

	api.get = func(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
		return &awss3.GetObjectOutput{
			Body:          &errorReadCloser{Reader: strings.NewReader("ok"), err: closeErr},
			ContentLength: aws.Int64(2),
		}, nil
	}
	object, err := store.Get(context.Background(), "close-error")
	if err != nil {
		t.Fatal(err)
	}
	if err := object.Body.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Body.Close() error = %v", err)
	}
	if err := object.Body.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("repeated Body.Close() error = %v", err)
	}

	outstanding, err := store.Get(context.Background(), "store-close-error")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Store.Close() error = %v", err)
	}
	if err := store.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("repeated Store.Close() error = %v", err)
	}
	if err := outstanding.Body.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("body error after Store.Close = %v", err)
	}
}

func TestManagedBodyRunsCleanupWhenCloserIsMissing(t *testing.T) {
	var cleanups int
	body := &managedBody{reader: strings.NewReader(""), cleanup: func() { cleanups++ }}
	if err := body.Close(); err == nil {
		t.Fatal("Close() error = nil")
	}
	if err := body.Close(); err == nil {
		t.Fatal("repeated Close() error = nil")
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleanups)
	}
}

func TestS3VirtualHostedEndpointConfiguration(t *testing.T) {
	fake := newFakeS3("objects")
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg := defaultConfig()
	cfg.Backend = BackendS3
	cfg.S3.Endpoint = server.URL
	cfg.S3.TLS = false
	cfg.S3.Bucket = "objects"
	cfg.S3.AccessKeyID = "access"
	cfg.S3.SecretKey = "secret"
	cfg.S3.PathStyle = false
	store, err := newS3Store(cfg)
	if err != nil {
		t.Fatalf("newS3Store() error = %v", err)
	}
	defer store.Close()
	if store.bucket != "objects" {
		t.Fatalf("bucket = %q", store.bucket)
	}
}

func testS3Store(t *testing.T, endpoint string, mutate func(*Config)) *s3Store {
	t.Helper()
	cfg := defaultConfig()
	cfg.Backend = BackendS3
	cfg.S3.Endpoint = endpoint
	cfg.S3.TLS = false
	cfg.S3.Bucket = "objects"
	cfg.S3.AccessKeyID = "access"
	cfg.S3.SecretKey = "secret"
	cfg.S3.Region = "test-region-1"
	if mutate != nil {
		mutate(&cfg)
	}
	store, err := newS3Store(cfg)
	if err != nil {
		t.Fatalf("newS3Store() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type fakeS3Object struct {
	data     []byte
	modified time.Time
	etag     string
}

type fakeS3Server struct {
	bucket string

	mu              sync.Mutex
	objects         map[string]fakeS3Object
	requests        int
	unsigned        int
	unsignedPayload int
	paths           []string
	delayKey        string
	delay           time.Duration
}

func newFakeS3(bucket string) *fakeS3Server {
	return &fakeS3Server{bucket: bucket, objects: make(map[string]fakeS3Object)}
}

func (s *fakeS3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	s.paths = append(s.paths, r.URL.Path)
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		s.unsigned++
	}
	if r.Header.Get("X-Amz-Content-Sha256") == "UNSIGNED-PAYLOAD" {
		s.unsignedPayload++
	}
	s.mu.Unlock()

	if r.URL.Query().Get("list-type") == "2" {
		s.list(w, r)
		return
	}
	prefix := "/" + s.bucket
	if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(w, r)
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/")
	s.mu.Lock()
	delay := time.Duration(0)
	if key == s.delayKey {
		delay = s.delay
	}
	s.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	switch r.Method {
	case http.MethodPut:
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		s.set(key, data)
		s.mu.Lock()
		object := s.objects[key]
		s.mu.Unlock()
		w.Header().Set("ETag", `"`+object.etag+`"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		object, ok := s.get(key)
		if !ok {
			s.notFound(w, key)
			return
		}
		s.objectHeaders(w, object)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(object.data)
	case http.MethodHead:
		object, ok := s.get(key)
		if !ok {
			s.notFound(w, key)
			return
		}
		s.objectHeaders(w, object)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *fakeS3Server) set(key string, data []byte) {
	hash := sha256.Sum256(data)
	s.mu.Lock()
	s.objects[key] = fakeS3Object{
		data:     append([]byte(nil), data...),
		modified: time.Now().UTC().Truncate(time.Second),
		etag:     hex.EncodeToString(hash[:]),
	}
	s.mu.Unlock()
}

func (s *fakeS3Server) get(key string) (fakeS3Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	object, ok := s.objects[key]
	return object, ok
}

func (s *fakeS3Server) objectHeaders(w http.ResponseWriter, object fakeS3Object) {
	w.Header().Set("Content-Length", strconv.Itoa(len(object.data)))
	w.Header().Set("Last-Modified", object.modified.Format(http.TimeFormat))
	w.Header().Set("ETag", `"`+object.etag+`"`)
}

func (s *fakeS3Server) notFound(w http.ResponseWriter, key string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, "<Error><Code>NoSuchKey</Code><Message>missing</Message><Key>%s</Key></Error>", key)
}

func (s *fakeS3Server) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	limit, _ := strconv.Atoi(r.URL.Query().Get("max-keys"))
	if limit <= 0 {
		limit = 1000
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
	s.mu.Lock()
	keys := make([]string, 0, len(s.objects))
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if start > len(keys) {
		start = len(keys)
	}
	end := min(start+limit, len(keys))
	page := append([]string(nil), keys[start:end]...)
	objects := make(map[string]fakeS3Object, len(page))
	for _, key := range page {
		objects[key] = s.objects[key]
	}
	s.mu.Unlock()

	result := fakeListResult{
		Name:        s.bucket,
		Prefix:      prefix,
		KeyCount:    len(page),
		MaxKeys:     limit,
		IsTruncated: end < len(keys),
	}
	if result.IsTruncated {
		result.NextContinuationToken = strconv.Itoa(end)
	}
	for _, key := range page {
		object := objects[key]
		result.Contents = append(result.Contents, fakeListEntry{
			Key:          key,
			LastModified: object.modified.Format(time.RFC3339),
			ETag:         `"` + object.etag + `"`,
			Size:         len(object.data),
			StorageClass: "STANDARD",
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_ = xml.NewEncoder(w).Encode(result)
}

type fakeListResult struct {
	XMLName               xml.Name        `xml:"ListBucketResult"`
	Name                  string          `xml:"Name"`
	Prefix                string          `xml:"Prefix"`
	KeyCount              int             `xml:"KeyCount"`
	MaxKeys               int             `xml:"MaxKeys"`
	IsTruncated           bool            `xml:"IsTruncated"`
	NextContinuationToken string          `xml:"NextContinuationToken,omitempty"`
	Contents              []fakeListEntry `xml:"Contents"`
}

type fakeListEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int    `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type stubS3API struct {
	put  func(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	get  func(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	head func(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
	list func(context.Context, *awss3.ListObjectsV2Input, ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error)
}

func (s *stubS3API) PutObject(ctx context.Context, input *awss3.PutObjectInput, options ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	if s.put != nil {
		return s.put(ctx, input, options...)
	}
	_, err := io.Copy(io.Discard, input.Body)
	return &awss3.PutObjectOutput{}, err
}

func (s *stubS3API) GetObject(ctx context.Context, input *awss3.GetObjectInput, options ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	if s.get != nil {
		return s.get(ctx, input, options...)
	}
	return &awss3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("")), ContentLength: aws.Int64(0)}, nil
}

func (*stubS3API) DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	return &awss3.DeleteObjectOutput{}, nil
}

func (s *stubS3API) HeadObject(ctx context.Context, input *awss3.HeadObjectInput, options ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if s.head != nil {
		return s.head(ctx, input, options...)
	}
	return &awss3.HeadObjectOutput{ContentLength: aws.Int64(0)}, nil
}

func (s *stubS3API) ListObjectsV2(ctx context.Context, input *awss3.ListObjectsV2Input, options ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
	if s.list != nil {
		return s.list(ctx, input, options...)
	}
	return &awss3.ListObjectsV2Output{}, nil
}

func newStubS3Store(api s3API, maxBytes int64) *s3Store {
	lifetime, cancel := context.WithCancel(context.Background())
	return &s3Store{
		client:         api,
		transport:      &http.Transport{},
		bucket:         "objects",
		maxObjectBytes: maxBytes,
		maxListItems:   10,
		requestTimeout: time.Second,
		lifetime:       lifetime,
		cancel:         cancel,
		bodies:         make(map[*managedBody]struct{}),
	}
}

type errorReadCloser struct {
	io.Reader
	err error
}

func (c *errorReadCloser) Close() error { return c.err }
