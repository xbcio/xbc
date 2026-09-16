package objectstorage

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

func TestHealthChecksContributeOneReadinessCheckForARemoteBucket(t *testing.T) {
	fake := newFakeS3("objects")
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store := &managedStore{backend: testS3Store(t, server.URL, nil), probeReachability: true}

	checks := store.HealthChecks()
	if len(checks) != 1 {
		t.Fatalf("got %d checks, want 1", len(checks))
	}
	if checks[0].Name != "" {
		t.Fatalf("the check must be named by the contributing instance, got %q", checks[0].Name)
	}
	if checks[0].Kind != health.Readiness {
		t.Fatalf("got kind %q, want readiness", checks[0].Kind)
	}
	if checks[0].Timeout != 0 {
		t.Fatalf("the check must inherit the plugins.health timeout, got %s", checks[0].Timeout)
	}

	// The probe key is absent, which is the expected answer: a definitive "no
	// such object" already proves the bucket answered.
	report := health.Check(context.Background(), health.Readiness, checks, time.Second)
	if !report.Healthy() {
		t.Fatalf("a reachable bucket must report up, got %+v", report)
	}
	if _, ok := fake.get(healthProbeKey); ok {
		t.Fatal("the probe must never write the key it asks about")
	}
}

func TestHealthChecksAreSuppressedForALocalBackendAndWhenDisabled(t *testing.T) {
	directory := t.TempDir()
	local := defaultConfig()
	local.Local.Directory = directory
	localStore, err := newStore(plugin.BuildContext{}, local)
	if err != nil {
		t.Fatalf("newStore(local): %v", err)
	}
	t.Cleanup(func() { _ = localStore.Close() })
	if checks := localStore.HealthChecks(); len(checks) != 0 {
		t.Fatalf("a local directory has no peer to probe, got %+v", checks)
	}

	fake := newFakeS3("objects")
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	remote := defaultConfig()
	remote.Backend = BackendS3
	remote.S3.Endpoint = server.URL
	remote.S3.TLS = false
	remote.S3.Bucket = "objects"
	remote.S3.AccessKeyID = "access"
	remote.S3.SecretKey = "secret"
	remote.S3.HealthProbe = false
	remoteStore, err := newStore(plugin.BuildContext{}, remote)
	if err != nil {
		t.Fatalf("newStore(s3): %v", err)
	}
	t.Cleanup(func() { _ = remoteStore.Close() })
	if checks := remoteStore.HealthChecks(); len(checks) != 0 {
		t.Fatalf("s3.health_probe: false must contribute nothing, got %+v", checks)
	}
}

func TestHealthCheckReportsDownForAnUnreachableEndpoint(t *testing.T) {
	server := httptest.NewServer(newFakeS3("objects"))
	endpoint := server.URL
	server.Close()
	store := &managedStore{backend: testS3Store(t, endpoint, nil), probeReachability: true}

	report := health.Check(context.Background(), health.Readiness, store.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("an unreachable endpoint must report down, got %+v", report)
	}
	if len(report.Checks) != 1 || report.Checks[0].Error == nil {
		t.Fatalf("expected one failed check, got %+v", report.Checks)
	}
}

// A rejected credential is exactly what readiness must catch, so it must not be
// confused with the absent probe key.
func TestHealthCheckReportsDownForARejectedCredential(t *testing.T) {
	store := &managedStore{
		backend: newStubS3Store(&stubS3API{
			head: func(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
				return nil, errors.New("api error InvalidAccessKeyId: the access key is not valid")
			},
		}, 1<<20),
		probeReachability: true,
	}

	report := health.Check(context.Background(), health.Readiness, store.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a rejected credential must report down, got %+v", report)
	}
	if message := report.Checks[0].Error.Error(); !strings.Contains(message, "InvalidAccessKeyId") {
		t.Fatalf("got error %q, want it to carry the rejection", message)
	}
}

func TestHealthCheckReportsDownForAClosedStore(t *testing.T) {
	fake := newFakeS3("objects")
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store := &managedStore{backend: testS3Store(t, server.URL, nil), probeReachability: true}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	report := health.Check(context.Background(), health.Readiness, store.HealthChecks(), time.Second)
	if report.Status != health.Down {
		t.Fatalf("a closed store must report down, got %+v", report)
	}
}
