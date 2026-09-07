// Package autoload declares object storage in XBC's optional process-wide
// composition. Libraries should import the side-effect-free parent package.
package autoload

import (
	"github.com/xbcio/xbc/extensions/storage/objectstorage"
	pluginautoload "github.com/xbcio/xbc/plugin/autoload"
)

func init() {
	pluginautoload.Declare(objectstorage.Bundle())
}
