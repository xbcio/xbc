// Package autoload declares kafka.Bundle into XBC's optional process-wide
// composition. Prefer explicit Bundle composition in applications.
package autoload

import (
	"github.com/xbcio/xbc/integrations/kafka"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(kafka.Bundle()) }
