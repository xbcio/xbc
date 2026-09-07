// Package autoload declares the Elasticsearch integration in XBC's optional
// process-wide composition. Import it only for side effects from an executable.
package autoload

import (
	integration "github.com/xbcio/xbc/integrations/elasticsearch"
	"github.com/xbcio/xbc/plugin/autoload"
)

func init() { autoload.Declare(integration.Bundle()) }
