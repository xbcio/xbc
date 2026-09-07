// Package autoload opts the cron Bundle into XBC's process-wide default
// composition. Libraries should import the side-effect-free parent package.
package autoload

import (
	"github.com/xbcio/xbc/extensions/jobs/cron"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(cron.Bundle()) }
