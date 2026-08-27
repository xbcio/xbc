// Package autoload registers the web plugin in xbc's process-wide default
// catalog. Import it for side effects from an executable:
//
//	import _ "github.com/xbcio/xbc/web/autoload"
//
// Libraries should import package web normally; doing so is side-effect free.
package autoload

import (
	"github.com/xbcio/xbc/plugin/catalog"
	"github.com/xbcio/xbc/web"
)

func init() {
	catalog.Declare(web.Definition())
}
