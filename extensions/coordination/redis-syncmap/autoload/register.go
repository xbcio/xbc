// Package autoload declares the Redis-backed syncmap integration in XBC's
// optional process-wide composition. Import it only for side effects from an
// executable.
package autoload

import (
	redissyncmap "github.com/xbcio/xbc/extensions/coordination/redis-syncmap"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	autoload.Declare(redissyncmap.Bundle())
}
