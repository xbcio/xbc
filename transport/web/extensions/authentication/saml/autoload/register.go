// Package autoload declares the SAML 2.0 Web adapter's canonical Bundle in
// XBC's optional process-global composition. Import it only for side effects
// from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	samlweb "github.com/xbcio/xbc/transport/web/extensions/authentication/saml"
)

func init() { autoload.Declare(samlweb.Bundle()) }
