// Package autoload declares the Casbin Redis watcher Bundle in XBC's optional
// process-wide composition. Libraries should import the side-effect-free parent
// package instead.
package autoload

import (
	casbinredis "github.com/xbcio/xbc/extensions/authorization/casbin-redis"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(casbinredis.Bundle()) }
