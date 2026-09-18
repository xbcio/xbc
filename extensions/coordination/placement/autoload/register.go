// Package autoload opts placement's Bundle into XBC's process-wide default
// composition. Libraries should import the side-effect-free parent package.
package autoload

import (
	"github.com/xbcio/xbc/extensions/coordination/placement"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(placement.Bundle()) }
