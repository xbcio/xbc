// Package autoload declares the OIDC Web adapter's canonical Bundle in XBC's
// optional process-global composition. Import it only for side effects from an
// executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	oidcweb "github.com/xbcio/xbc/transport/web/extensions/authentication/oidc"
)

func init() { autoload.Declare(oidcweb.Bundle()) }
