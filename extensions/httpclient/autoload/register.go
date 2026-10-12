// Package autoload adds the canonical http-client Bundle to XBC's optional
// process-global composition.
package autoload

import (
	"github.com/xbcio/xbc/extensions/httpclient"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(httpclient.Bundle()) }
