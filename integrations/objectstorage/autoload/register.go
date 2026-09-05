// Package autoload declares object storage in XBC's optional process-wide
// composition. Libraries should import the side-effect-free parent package.
package autoload

import (
	"github.com/xbcio/xbc/integrations/objectstorage"
	internalautoload "github.com/xbcio/xbc/internal/autoload"
)

func init() {
	internalautoload.Declare(objectstorage.Bundle())
}
