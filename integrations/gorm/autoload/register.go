// Package autoload declares the GORM Bundle in XBC's optional process-wide
// default composition. Libraries should import the parent package and compose
// its Bundle explicitly instead.
package autoload

import (
	gormplugin "github.com/xbcio/xbc/integrations/gorm"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	autoload.Declare(gormplugin.Bundle())
}
