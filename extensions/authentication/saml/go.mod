module github.com/xbcio/xbc/extensions/authentication/saml

go 1.25.0

// The authentication contract module resolves through its published release
// tag; repository development resolves the same source through go.work.
require (
	github.com/beevik/etree v1.8.1
	github.com/russellhaering/goxmldsig v1.6.1
	github.com/xbcio/xbc/extensions/authentication v0.2.0
)

require github.com/jonboulle/clockwork v0.5.0 // indirect
