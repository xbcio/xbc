// Package autoload declares apptest's canonical Bundle in XBC's optional
// process-global composition. Import it only for side effects from a test
// binary that wants the readiness probe declared without listing the Bundle.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/testing/apptest"
)

func init() { autoload.Declare(apptest.Bundle()) }
