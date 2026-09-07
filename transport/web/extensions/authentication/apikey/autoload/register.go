// Package autoload opts apikey's canonical Bundle into XBC's optional
// process-wide composition. Import this leaf adapter only for side effects from
// an executable.
package autoload

import (
	"github.com/xbcio/xbc/plugin/autoload"
	"github.com/xbcio/xbc/transport/web/extensions/authentication/apikey"
)

func init() {
	autoload.Declare(apikey.Bundle())
}
