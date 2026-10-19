package ldap

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xbcio/xbc/extensions/authentication/federation"
)

// Provider is one immutable LDAP directory configuration together with the
// connection policy derived from it. Verify is safe for concurrent use.
//
// A Provider performs no network I/O at construction: New only validates and
// freezes the configuration. Every Verify call opens its own connections, so a
// provider never holds a pool and retiring one needs no shutdown call.
//
// String, Format, and LogValue render the provider with the service bind
// password redacted, including for %#v and structured log records.
type Provider struct {
	settings
	initialized bool
	resolver    hostResolver
	dialContext dialContextFunc
	directory   userDirectory
}

// userDirectory is the seam between the login flow and the directory. The real
// implementation speaks LDAP; tests replace it to exercise the flow without a
// server.
type userDirectory interface {
	// lookupUser searches for exactly one user entry and returns it, or
	// ErrInvalidCredentials for zero or multiple matches.
	lookupUser(ctx context.Context, username string) (userEntry, error)
	// authenticate binds as the user distinguished name to verify the
	// password. A rejected bind is ErrInvalidCredentials.
	authenticate(ctx context.Context, dn, password string) error
}

// userEntry is one directory entry as returned by the search. Attribute names
// in the map are lower-cased, matching LDAP's case-insensitive attribute
// descriptions; the real directory normalizes them before returning.
type userEntry struct {
	dn         string
	attributes map[string][]string
}

// New validates config without network I/O and returns a provider ready for
// Verify. The provider is immutable; replace it through the registry to apply
// a configuration change.
func New(config Config) (*Provider, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return &Provider{
		settings:    normalized,
		initialized: true,
		resolver:    netResolver{resolver: net.DefaultResolver},
		dialContext: (&net.Dialer{}).DialContext,
	}, nil
}

// Name returns the registered provider name, or the empty string for a nil
// provider.
func (p *Provider) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// String renders the provider for diagnostics with the service bind password
// redacted.
func (p *Provider) String() string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("ldap.Provider{Name:%q Issuer:%q URL:%q StartTLS:%t ServiceBindDN:%q ServiceBindPassword:[REDACTED] BaseDN:%q UserFilter:%q SubjectAttribute:%q AllowPrivate:%t DialTimeout:%s OperationTimeout:%s}",
		p.name, p.issuer, p.scheme+"://"+net.JoinHostPort(p.host, p.port), p.startTLS,
		p.serviceBindDN, p.baseDN, p.userFilter, p.subjectAttribute, p.allowPrivate,
		p.dialTimeout, p.operationTimeout)
}

// Format implements fmt.Formatter so every verb renders the redacted string,
// including %#v, which would otherwise print the unexported settings fields --
// and with them the service bind password.
//
// The receiver is a value, not a pointer. fmt dispatches methods on a struct's
// fields only when they are exported, so a dereferenced Provider value would
// otherwise render the unexported password field verbatim. A value receiver
// makes the Provider value itself satisfy fmt.Formatter. Formatting a nil
// *Provider still prints "<nil>": fmt converts a nil-pointer panic in a
// formatting method to that string.
func (p Provider) Format(state fmt.State, _ rune) {
	// p is addressable, so the pointer-receiver String is reachable through the
	// value. Calling it as p.String() rather than (&p).String() keeps a
	// Format-receiver mutation compilable, so it is killed by behavior.
	_, _ = io.WriteString(state, p.String())
}

// LogValue implements slog.LogValuer with the service bind password redacted.
func (p *Provider) LogValue() slog.Value { return slog.StringValue(p.String()) }

// usable reports whether the provider was constructed by New.
func (p *Provider) usable() error {
	if p == nil {
		return ErrNilProvider
	}
	if !p.initialized {
		return fmt.Errorf("%w: provider was not constructed by New", ErrInvalidConfig)
	}
	return nil
}

// directoryClient returns the directory implementation this provider uses.
func (p *Provider) directoryClient() userDirectory {
	if p.directory != nil {
		return p.directory
	}
	return realDirectory{provider: p}
}

// Verify authenticates username and password against the provider's directory
// and returns the verified external identity.
//
// The flow is search-then-bind. A service-account connection searches for the
// user entry with the configured filter and the RFC 4515-escaped user name;
// exactly one entry must match. A second, fresh connection then binds as that
// entry's distinguished name with the supplied password. The search connection
// is never rebound as the user, so the service identity is not left behind on
// a shared connection.
//
// Failures are deliberately coarse. An empty user name or password is rejected
// before any network call; an unknown user, a wrong password, and a directory
// entry matching more than one user all return ErrInvalidCredentials with no
// further detail. Operational failures return ErrUnavailable. No error text
// ever carries the password, a distinguished name, an attribute value, the
// filter, or the user name; the caller must map every failure onto its own
// generic login failure.
func Verify(ctx context.Context, provider *Provider, username, password string) (federation.ExternalIdentity, error) {
	if ctx == nil {
		return federation.ExternalIdentity{}, ErrNilContext
	}
	if err := provider.usable(); err != nil {
		return federation.ExternalIdentity{}, err
	}
	// Empty passwords must never reach the directory: many servers treat an
	// empty simple-bind password as an anonymous bind, which succeeds. The user
	// name is rejected on the same path so neither input shape reaches the
	// network.
	if username == "" || password == "" || len(username) > maxUsernameLength {
		return federation.ExternalIdentity{}, ErrInvalidCredentials
	}

	client := provider.directoryClient()
	entry, err := client.lookupUser(ctx, username)
	if err != nil {
		return federation.ExternalIdentity{}, err
	}
	if err := client.authenticate(ctx, entry.dn, password); err != nil {
		return federation.ExternalIdentity{}, err
	}
	return provider.identity(entry)
}

// identity builds the verified identity from the matched entry. The subject
// must come from a stable attribute: a missing, empty, ambiguous, or non-text
// subject fails the login instead of falling back to uid or mail.
func (p *Provider) identity(entry userEntry) (federation.ExternalIdentity, error) {
	subjectValues := entry.attributes[strings.ToLower(p.subjectAttribute)]
	if len(subjectValues) != 1 {
		return federation.ExternalIdentity{}, ErrInvalidDirectoryEntry
	}
	subject := subjectValues[0]
	if subject == "" || !utf8.ValidString(subject) {
		return federation.ExternalIdentity{}, ErrInvalidDirectoryEntry
	}

	claims := make(map[string]any, len(p.claimAttributes))
	for claim, attribute := range p.claimAttributes {
		values := entry.attributes[strings.ToLower(attribute)]
		if len(values) == 0 {
			return federation.ExternalIdentity{}, ErrInvalidDirectoryEntry
		}
		for _, value := range values {
			if !utf8.ValidString(value) {
				return federation.ExternalIdentity{}, ErrInvalidDirectoryEntry
			}
		}
		if len(values) == 1 {
			claims[claim] = values[0]
			continue
		}
		claims[claim] = append([]string(nil), values...)
	}

	return federation.ExternalIdentity{
		Protocol: protocolName,
		Provider: p.name,
		Issuer:   p.issuer,
		Subject:  subject,
		Claims:   claims,
		AuthTime: p.now(),
	}, nil
}

// searchAttributes is the sorted, deduplicated attribute list requested from
// the directory: the subject and every configured claim attribute.
func (p *Provider) searchAttributes() []string {
	seen := make(map[string]struct{}, 1+len(p.claimAttributes))
	attributes := make([]string, 0, 1+len(p.claimAttributes))
	add := func(name string) {
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		attributes = append(attributes, name)
	}
	add(p.subjectAttribute)
	for _, attribute := range p.claimAttributes {
		add(attribute)
	}
	return attributes
}

// searchTimeLimit converts the operation timeout into the whole-second server
// search time limit. The client-side request timeout remains authoritative.
func (p *Provider) searchTimeLimit() int {
	seconds := int(p.operationTimeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}
