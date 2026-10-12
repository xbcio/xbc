// Package autoload declares metrics in XBC's default composition.
package autoload

import (
	"github.com/xbcio/xbc/extensions/observability/metrics"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(metrics.Bundle()) }
