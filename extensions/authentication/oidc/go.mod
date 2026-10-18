module github.com/xbcio/xbc/extensions/authentication/oidc

go 1.25.0

// The authentication contract module resolves through its published release
// tag; repository development resolves the same source through go.work.
require (
	github.com/go-jose/go-jose/v4 v4.1.6
	github.com/xbcio/xbc/extensions/authentication v0.2.0
)
