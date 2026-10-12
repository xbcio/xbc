// Package autoload declares tracing in XBC's default composition.
package autoload

import (
	"github.com/xbcio/xbc/extensions/observability/tracing"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(tracing.Bundle()) }
