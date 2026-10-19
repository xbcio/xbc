// Package autoload declares the OAuth 2.0 Web adapter's canonical Bundle in
// XBC's optional process-global composition. Import it only for side effects
// from an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	oauth2web "github.com/xbcio/xbc/transport/web/extensions/authentication/oauth2"
)

func init() { autoload.Declare(oauth2web.Bundle()) }
