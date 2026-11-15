package mail

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/xbcio/xbc/extensions/reliability/health"
)

// HealthChecks contributes SMTP server reachability as a readiness check. The
// plugin's producing identity qualifies the check name: "mail" for the default
// instance and "mail[<instance>]" for a named one. The contribution is inert
// unless the application also selects the health capability Bundle.
//
// The probe is contributed only when the instance enables verify: an instance
// that deliberately tolerates an unavailable server at startup has already
// declared reachability non-load-bearing, so a probe would report the
// deployment as not ready for a dependency it chose not to require.
//
// The probe opens a TCP connection to the configured host:port and closes it
// immediately. It deliberately performs no SMTP or AUTH exchange: sending
// credentials on every probe interval would let a monitoring loop trip a
// provider's failed-login lockout, and it would make readiness depend on an
// account policy rather than on reachability. Construction already verifies the
// full conversation (EHLO, TLS, AUTH) once when verify is enabled.
func (c *Client) HealthChecks() []health.NamedChecker {
	if c == nil || !c.verify {
		return nil
	}
	return []health.NamedChecker{{
		Kind:    health.Readiness,
		Checker: health.CheckFunc(c.probe),
	}}
}

// probe dials the configured address and closes the result. It never renders
// credentials; the address is the only dependency detail in its errors.
func (c *Client) probe(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mail: readiness probe requires a non-nil context")
	}
	if c.closed.Load() {
		return ErrClosed
	}
	if c.connectionTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.connectionTimeout)
		defer cancel()
	}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", c.address)
	if err != nil {
		return fmt.Errorf("mail: reach %s: %w", c.address, err)
	}
	if err := connection.Close(); err != nil {
		return fmt.Errorf("mail: close probe connection to %s: %w", c.address, err)
	}
	return nil
}
