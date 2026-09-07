// Package autoload adds the canonical webhook Bundle to XBC's optional
// process-global composition.
package autoload

import (
	"github.com/xbcio/xbc/extensions/messaging/webhook"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(webhook.Bundle()) }
