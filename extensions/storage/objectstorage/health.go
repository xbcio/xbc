package objectstorage

import (
	"context"
	"errors"
	"fmt"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

// healthProbeKey is the key the readiness check asks about. It is expected to be
// absent: a definitive "no such object" answer already proves the endpoint
// resolved, the transport completed, the credentials were accepted and the
// bucket exists, which is everything readiness needs. Nothing is ever written.
const healthProbeKey = ".xbc-health-probe"

var _ health.Contributor = (*managedStore)(nil)

// HealthChecks contributes the reachability of a remote bucket as a readiness
// check. Each configured instance contributes under its own identity:
// "objectstorage" for the default instance and "objectstorage[<instance>]" for a
// named one. The contribution is inert unless the application also selects the
// health capability Bundle.
//
// A local instance contributes nothing. It has no peer whose loss could be
// invisible, and a check that can only answer up would dilute the aggregate
// report rather than inform it.
//
// The probe is a single HeadObject, so it needs no permission beyond what Get
// and Stat already require. One caveat is worth knowing: S3 answers HeadObject
// for a missing key with 403 rather than 404 when the caller has no
// s3:ListBucket permission on the bucket. Such a deployment either grants that
// permission or sets s3.health_probe: false. Reporting 403 as reachable is
// deliberately not done -- it would also accept an expired or revoked
// credential, which is exactly the failure readiness must catch.
func (s *managedStore) HealthChecks() []health.NamedChecker {
	if s == nil || !s.probeReachability {
		return nil
	}
	return []health.NamedChecker{{
		Kind:    health.Readiness,
		Checker: health.CheckFunc(s.checkReachability),
	}}
}

func (s *managedStore) checkReachability(ctx context.Context) error {
	_, err := s.Stat(ctx, healthProbeKey)
	switch {
	case err == nil, errors.Is(err, ErrNotFound):
		return nil
	case errors.Is(err, ErrClosed):
		return fmt.Errorf("objectstorage: store is closed: %w", err)
	default:
		return fmt.Errorf("objectstorage: reachability probe: %w", err)
	}
}
