// Package autoload adds the canonical webhook Bundle to XBC's optional
// process-global composition.
package autoload

import (
	"github.com/xbcio/xbc/integrations/webhook"
	"github.com/xbcio/xbc/internal/autoload"
)

func init() { autoload.Declare(webhook.Bundle()) }
