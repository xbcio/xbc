// Package autoload declares the Redis integration in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	"github.com/xbcio/xbc/integrations/redis"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	autoload.Declare(redis.Bundle())
}
