// Package autoload declares the Casbin Redis watcher Bundle in XBC's optional
// process-wide composition. Libraries should import the side-effect-free parent
// package instead.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	casbinredis "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin-redis"
)

func init() { autoload.Declare(casbinredis.Bundle()) }
