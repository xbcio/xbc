package ldap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ldapclient "github.com/go-ldap/ldap/v3"
)

// realDirectory is the LDAP implementation of userDirectory. It opens a fresh
// connection per operation, so the service-account bind and the user bind never
// share a connection.
type realDirectory struct {
	provider *Provider
}

// lookupUser binds as the service account, searches for exactly one user entry
// with the escaped filter, and returns it. Zero or multiple matches are a
// uniform credential failure; a directory or transport failure is
// ErrUnavailable.
func (d realDirectory) lookupUser(ctx context.Context, username string) (userEntry, error) {
	conn, err := d.provider.connect(ctx)
	if err != nil {
		return userEntry{}, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := conn.Bind(d.provider.serviceBindDN, string(d.provider.serviceBindPassword)); err != nil {
		return userEntry{}, ErrUnavailable
	}

	filter := fmt.Sprintf(d.provider.userFilter, ldapclient.EscapeFilter(username))
	request := ldapclient.NewSearchRequest(
		d.provider.baseDN,
		ldapclient.ScopeWholeSubtree,
		ldapclient.NeverDerefAliases,
		2, // one match plus the evidence of a second
		d.provider.searchTimeLimit(),
		false,
		filter,
		d.provider.searchAttributes(),
		nil,
	)
	result, err := conn.Search(request)
	if err != nil {
		var directoryError *ldapclient.Error
		if errors.As(err, &directoryError) && directoryError.ResultCode == ldapclient.LDAPResultSizeLimitExceeded {
			// The server stopped at the size limit, so more than one entry
			// matches. Treat it exactly like a detected multiple match.
			return userEntry{}, ErrInvalidCredentials
		}
		return userEntry{}, ErrUnavailable
	}
	if len(result.Entries) != 1 {
		return userEntry{}, ErrInvalidCredentials
	}
	entry := result.Entries[0]
	if entry.DN == "" {
		return userEntry{}, ErrInvalidDirectoryEntry
	}

	attributes := make(map[string][]string, len(entry.Attributes))
	for _, attribute := range entry.Attributes {
		values := make([]string, len(attribute.ByteValues))
		for index, value := range attribute.ByteValues {
			values[index] = string(value)
		}
		attributes[strings.ToLower(attribute.Name)] = values
	}
	return userEntry{dn: entry.DN, attributes: attributes}, nil
}

// authenticate opens a fresh connection and binds as the user. Only an
// invalid-credentials result is a credential failure; every other failure is
// operational.
func (d realDirectory) authenticate(ctx context.Context, dn, password string) error {
	conn, err := d.provider.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	if err := conn.Bind(dn, password); err != nil {
		var directoryError *ldapclient.Error
		if errors.As(err, &directoryError) && directoryError.ResultCode == ldapclient.LDAPResultInvalidCredentials {
			return ErrInvalidCredentials
		}
		return ErrUnavailable
	}
	return nil
}
