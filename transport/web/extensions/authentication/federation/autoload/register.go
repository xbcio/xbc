// Package autoload declares federation's canonical Bundle in XBC's optional
// process-global composition. Import it only for side effects from an
// executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	federationweb "github.com/xbcio/xbc/transport/web/extensions/authentication/federation"
)

func init() { autoload.Declare(federationweb.Bundle()) }
