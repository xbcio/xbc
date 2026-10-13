package kafka

import (
	"context"
	"fmt"
)

// ConnectionRequest carries what a configured instance knows before its
// connection is resolved. With resolve_connection enabled, Brokers is an
// opaque application-defined reference -- an HTTP control-plane URL in the
// SAS deployment -- rather than a broker list, so a resolver must not treat
// it as host:port. SASL is the configured block, passed so a resolver can
// exchange it for credentials of its own; SAS indatahub trades an ACL account
// and password for SCRAM credentials.
type ConnectionRequest struct {
	// Instance is the configured instance name, empty for the default one.
	Instance string
	// Brokers is the configured brokers value, carried as opaque input.
	Brokers []string
	// SASL is the configured SASL block.
	SASL SASLConfig
}

// Connection is the effective connection a resolver returns. Brokers and SASL
// replace the configured values as whole fields; a resolver that wants to
// keep a configured value returns it unchanged. TLS is always taken from the
// configuration, never from a resolver.
type Connection struct {
	Brokers []string
	SASL    SASLConfig
}

// ConnectionResolver turns an instance's configured broker reference into the
// connection the client actually uses. An application exports it from a
// Definition and declares it selectable through plugin.ExportAs; this package
// consumes it as an optional input, so a composition without one keeps its
// dependency closure free of any control-plane client.
//
// It is called at construction time, once per instance with
// resolve_connection enabled -- and construction is what validate runs, so a
// validate call really reaches the resolver, while doctor, which only
// prepares configuration, never does. The resolver must respect its context
// deadline: a context bound by resolve_timeout is always imposed. An error it
// returns reaches the construction failure as-is, so it must not embed
// credentials; the same holds for every value returned.
type ConnectionResolver interface {
	ResolveConnection(ctx context.Context, request ConnectionRequest) (Connection, error)
}

// effectiveConfig produces the connection a client is built from. Without
// resolve_connection the configured brokers are validated as a broker list
// and used directly -- the pre-contract behavior. With it, the configured
// brokers are first validated as opaque references, the resolver replaces
// brokers and SASL wholesale, and the replacement runs through the full
// validation again: the same host:port, duplicate, TLS, and SASL rules apply
// to what a resolver returns, so a resolved plaintext PLAIN credential pair
// is refused exactly like a configured one.
func effectiveConfig(cfg Config, resolver ConnectionResolver, instance string) (normalizedConfig, error) {
	if !cfg.ResolveConnection {
		return normalizeConfig(cfg)
	}
	prepared, err := normalize(cfg, true)
	if err != nil {
		return normalizedConfig{}, err
	}
	if resolver == nil {
		return normalizedConfig{}, fmt.Errorf(
			"kafka: resolve_connection is enabled but no kafka.ConnectionResolver input is selected (instance %s)",
			instanceLabel(instance))
	}
	ctx, cancel := context.WithTimeout(context.Background(), prepared.ResolveTimeout)
	defer cancel()
	resolved, err := resolver.ResolveConnection(ctx, ConnectionRequest{
		Instance: instance,
		Brokers:  append([]string(nil), prepared.Brokers...),
		SASL:     prepared.SASL,
	})
	if err != nil {
		return normalizedConfig{}, fmt.Errorf("kafka: resolve connection for instance %s: %w", instanceLabel(instance), err)
	}
	effective := prepared.Config
	effective.Brokers = append([]string(nil), resolved.Brokers...)
	effective.SASL = resolved.SASL
	return normalizeConfig(effective)
}

// instanceLabel names an instance in an error: the configured name, or
// "default" for the unnamed one.
func instanceLabel(instance string) string {
	if instance == "" {
		return "default"
	}
	return instance
}
