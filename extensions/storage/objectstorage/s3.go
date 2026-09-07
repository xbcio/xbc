package objectstorage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	signerv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type s3API interface {
	PutObject(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error)
	HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
	ListObjectsV2(context.Context, *awss3.ListObjectsV2Input, ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error)
}

type s3Store struct {
	client         s3API
	transport      *http.Transport
	bucket         string
	prefix         string
	maxObjectBytes int64
	maxListItems   int
	requestTimeout time.Duration
	lifetime       context.Context
	cancel         context.CancelFunc

	stateMu    sync.Mutex
	closed     bool
	bodies     map[*managedBody]struct{}
	operations sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

func newS3Store(cfg Config) (*s3Store, error) {
	normalized, err := normalizedS3Config(cfg.S3)
	if err != nil {
		return nil, err
	}
	endpoint, err := endpointURL(normalized)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: normalized.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       normalized.IdleConnTimeout,
		TLSHandshakeTimeout:   normalized.TLSHandshakeTimeout,
		ResponseHeaderTimeout: normalized.ResponseHeaderTimeout,
		TLSClientConfig:       tlsConfig(normalized),
	}
	credentialsProvider := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
		normalized.AccessKeyID,
		normalized.SecretKey,
		normalized.SessionToken,
	))
	awsConfig := aws.Config{
		Region:                     normalized.Region,
		Credentials:                credentialsProvider,
		HTTPClient:                 &http.Client{Transport: transport},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	client := awss3.NewFromConfig(awsConfig, func(options *awss3.Options) {
		options.UsePathStyle = normalized.PathStyle
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
		}
	})
	lifetime, cancel := context.WithCancel(context.Background())
	return &s3Store{
		client:         client,
		transport:      transport,
		bucket:         normalized.Bucket,
		prefix:         normalized.Prefix,
		maxObjectBytes: cfg.MaxObjectBytes,
		maxListItems:   cfg.MaxListItems,
		requestTimeout: normalized.RequestTimeout,
		lifetime:       lifetime,
		cancel:         cancel,
		bodies:         make(map[*managedBody]struct{}),
	}, nil
}

func (s *s3Store) operationContext(ctx context.Context) (context.Context, func(), error) {
	if err := validateContext(ctx); err != nil {
		return nil, nil, err
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil, nil, ErrClosed
	}
	s.operations.Add(1)
	s.stateMu.Unlock()

	linked, cleanupLinked := linkedContext(ctx, s.lifetime)
	timed, cancel := context.WithTimeout(linked, s.requestTimeout)
	var once sync.Once
	return timed, func() {
		once.Do(func() {
			cancel()
			cleanupLinked()
			s.operations.Done()
		})
	}, nil
}

func (s *s3Store) Put(ctx context.Context, key string, body io.Reader, options PutOptions) (ObjectInfo, error) {
	if err := validateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	if body == nil {
		return ObjectInfo{}, errors.New("objectstorage: put body is nil")
	}
	if options.Size < UnknownSize {
		return ObjectInfo{}, fmt.Errorf("objectstorage: invalid declared size %d", options.Size)
	}
	if options.Size > s.maxObjectBytes {
		return ObjectInfo{}, objectTooLarge(key, s.maxObjectBytes)
	}
	opCtx, done, err := s.operationContext(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer done()

	var (
		upload      io.Reader
		streamed    func() int64
		streamError func() error
	)
	if options.Size == UnknownSize {
		limited := &boundedReader{ctx: opCtx, src: body, key: key, max: s.maxObjectBytes}
		upload = limited
		streamed = func() int64 { return limited.read }
		streamError = func() error { return limited.err }
	} else {
		exact := &exactSizeReader{ctx: opCtx, src: body, key: key, expected: options.Size}
		upload = exact
		streamed = func() int64 { return exact.read }
		streamError = func() error { return exact.err }
	}

	input := &awss3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.backendKey(key)),
		Body:   upload,
	}
	// Put accepts arbitrary streaming readers, which cannot necessarily be
	// rewound for SigV4 payload hashing. UNSIGNED-PAYLOAD signs the request and
	// headers while allowing a single-pass body over both HTTP and HTTPS. Do not
	// set ContentLength for a declared stream: chunked transfer forces net/http
	// to read the exact-size guard through EOF, so a long stream cannot be
	// silently truncated and committed by S3 before we detect the mismatch.
	callOptions := []func(*awss3.Options){func(options *awss3.Options) {
		options.APIOptions = append(options.APIOptions, signerv4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
	}}
	output, err := s.client.PutObject(opCtx, input, callOptions...)
	if streamErr := streamError(); streamErr != nil && !errors.Is(streamErr, io.EOF) {
		if err != nil {
			return ObjectInfo{}, errors.Join(streamErr, mapS3Error("put", key, err))
		}
		return ObjectInfo{}, streamErr
	}
	if err != nil {
		return ObjectInfo{}, mapS3Error("put", key, err)
	}
	if options.Size >= 0 && streamed() != options.Size {
		return ObjectInfo{}, declaredSizeError(key, options.Size, streamed())
	}
	if output == nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: s3 put %q returned an empty response", key)
	}
	return ObjectInfo{
		Key:  key,
		Size: streamed(),
		ETag: trimETag(aws.ToString(output.ETag)),
	}, nil
}

func (s *s3Store) Get(ctx context.Context, key string) (*Object, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	opCtx, done, err := s.operationContext(ctx)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			done()
		}
	}()
	output, err := s.client.GetObject(opCtx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.backendKey(key))})
	if err != nil {
		return nil, mapS3Error("get", key, err)
	}
	if output == nil || output.Body == nil {
		return nil, fmt.Errorf("objectstorage: s3 get %q returned an empty response body", key)
	}
	if output.ContentLength != nil && *output.ContentLength > s.maxObjectBytes {
		closeErr := output.Body.Close()
		return nil, errors.Join(objectTooLarge(key, s.maxObjectBytes), closeErr)
	}
	info := ObjectInfo{
		Key:        key,
		Size:       aws.ToInt64(output.ContentLength),
		ModifiedAt: aws.ToTime(output.LastModified),
		ETag:       trimETag(aws.ToString(output.ETag)),
	}
	body := &managedBody{
		reader:  &boundedReader{ctx: opCtx, src: output.Body, key: key, max: s.maxObjectBytes},
		closer:  output.Body,
		cleanup: done,
	}
	body.onClose = s.removeBody
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		_ = body.Close()
		return nil, ErrClosed
	}
	s.bodies[body] = struct{}{}
	s.stateMu.Unlock()
	keep = true
	return &Object{Info: info, Body: body}, nil
}

func (s *s3Store) removeBody(body *managedBody) {
	s.stateMu.Lock()
	delete(s.bodies, body)
	s.stateMu.Unlock()
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	opCtx, done, err := s.operationContext(ctx)
	if err != nil {
		return err
	}
	defer done()
	_, err = s.client.DeleteObject(opCtx, &awss3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.backendKey(key))})
	if err != nil {
		return mapS3Error("delete", key, err)
	}
	return nil
}

func (s *s3Store) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := validateKey(key); err != nil {
		return ObjectInfo{}, err
	}
	opCtx, done, err := s.operationContext(ctx)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer done()
	output, err := s.client.HeadObject(opCtx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.backendKey(key))})
	if err != nil {
		return ObjectInfo{}, mapS3Error("stat", key, err)
	}
	if output == nil {
		return ObjectInfo{}, fmt.Errorf("objectstorage: s3 stat %q returned an empty response", key)
	}
	if aws.ToInt64(output.ContentLength) > s.maxObjectBytes {
		return ObjectInfo{}, objectTooLarge(key, s.maxObjectBytes)
	}
	return ObjectInfo{
		Key:        key,
		Size:       aws.ToInt64(output.ContentLength),
		ModifiedAt: aws.ToTime(output.LastModified),
		ETag:       trimETag(aws.ToString(output.ETag)),
	}, nil
}

func (s *s3Store) List(ctx context.Context, options ListOptions) (ListResult, error) {
	if err := validatePrefix(options.Prefix); err != nil {
		return ListResult{}, err
	}
	limit, err := listLimit(options.Limit, s.maxListItems)
	if err != nil {
		return ListResult{}, err
	}
	opCtx, done, err := s.operationContext(ctx)
	if err != nil {
		return ListResult{}, err
	}
	defer done()
	input := &awss3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(s.backendKey(options.Prefix)),
		MaxKeys: aws.Int32(int32(limit)),
	}
	if options.Cursor != "" {
		input.ContinuationToken = aws.String(options.Cursor)
	}
	output, err := s.client.ListObjectsV2(opCtx, input)
	if err != nil {
		return ListResult{}, mapS3Error("list", options.Prefix, err)
	}
	if output == nil {
		return ListResult{}, fmt.Errorf("objectstorage: s3 list %q returned an empty response", options.Prefix)
	}
	objects := make([]ObjectInfo, 0, len(output.Contents))
	for _, object := range output.Contents {
		key, ok := s.publicKey(aws.ToString(object.Key))
		if !ok || key == "" {
			continue
		}
		objects = append(objects, ObjectInfo{
			Key:        key,
			Size:       aws.ToInt64(object.Size),
			ModifiedAt: aws.ToTime(object.LastModified),
			ETag:       trimETag(aws.ToString(object.ETag)),
		})
	}
	result := ListResult{Objects: objects}
	if aws.ToBool(output.IsTruncated) {
		result.NextCursor = aws.ToString(output.NextContinuationToken)
	}
	return result, nil
}

func (s *s3Store) backendKey(key string) string {
	if s.prefix == "" {
		return key
	}
	if key == "" {
		return s.prefix + "/"
	}
	return s.prefix + "/" + key
}

func (s *s3Store) publicKey(key string) (string, bool) {
	if s.prefix == "" {
		return key, true
	}
	prefix := s.prefix + "/"
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	return strings.TrimPrefix(key, prefix), true
}

func trimETag(etag string) string { return strings.Trim(etag, `"`) }

func mapS3Error(operation, key string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if s3NotFound(err) {
		return notFound(key, err)
	}
	return fmt.Errorf("objectstorage: s3 %s %q: %w", operation, key, err)
}

func s3NotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchObject":
			return true
		}
	}
	var responseErr *smithyhttp.ResponseError
	return errors.As(err, &responseErr) && responseErr.HTTPStatusCode() == http.StatusNotFound
}

func (s *s3Store) Close() error {
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		s.closed = true
		s.cancel()
		bodies := make([]*managedBody, 0, len(s.bodies))
		for body := range s.bodies {
			bodies = append(bodies, body)
		}
		s.stateMu.Unlock()
		for _, body := range bodies {
			if err := body.Close(); err != nil {
				s.closeErr = errors.Join(s.closeErr, fmt.Errorf("objectstorage: close S3 response body: %w", err))
			}
		}
		s.operations.Wait()
		s.transport.CloseIdleConnections()
	})
	return s.closeErr
}

var _ Store = (*s3Store)(nil)
