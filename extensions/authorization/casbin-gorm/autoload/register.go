// Package autoload declares the Casbin GORM Bundle in XBC's optional
// process-wide default composition. Libraries should import the parent package
// and compose its Bundle explicitly instead.
package autoload

import (
	casbingorm "github.com/xbcio/xbc/extensions/authorization/casbin-gorm"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	autoload.Declare(casbingorm.Bundle())
}
