// Package autoload declares the outbox Bundle in XBC's optional default
// composition. Import github.com/xbcio/xbc/integrations/outbox directly for
// side-effect-free explicit composition.
package autoload

import (
	"github.com/xbcio/xbc/integrations/outbox"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(outbox.Bundle()) }
