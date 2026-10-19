module github.com/xbcio/xbc/extensions/authentication/ldap

go 1.25.0

// The authentication contract module resolves through its published release
// tag; repository development resolves the same source through go.work.
require (
	github.com/go-ldap/ldap/v3 v3.4.14
	github.com/xbcio/xbc/extensions/authentication v0.2.0
)

require (
	github.com/Azure/go-ntlmssp v0.1.1 // indirect
	github.com/go-asn1-ber/asn1-ber v1.5.8 // indirect
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/crypto v0.54.0 // indirect
)
