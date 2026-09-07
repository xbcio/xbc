// Package autoload declares tracing in XBC's default composition.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/integrations/tracing"
)

func init() { autoload.Declare(tracing.Bundle()) }
