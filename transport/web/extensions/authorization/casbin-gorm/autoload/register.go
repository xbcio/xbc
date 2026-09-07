// Package autoload declares the Casbin GORM Bundle in XBC's optional
// process-wide default composition. Libraries should import the parent package
// and compose its Bundle explicitly instead.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	casbingorm "github.com/xbcio/xbc/transport/web/extensions/authorization/casbin-gorm"
)

func init() {
	autoload.Declare(casbingorm.Bundle())
}
