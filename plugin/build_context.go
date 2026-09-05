package plugin

import (
	"github.com/xbcio/xbc/internal/pluginmodel"
	"github.com/xbcio/xbc/log"
)

// BuildContext is the bounded constructor-injection context for exactly one
// factory invocation. Copies share synchronized invalidation state and panic
// after that factory returns.
type BuildContext pluginmodel.BuildContext

// Identity returns the consuming Plugin identity.
func (context BuildContext) Identity() Identity {
	return fromModelIdentity(pluginmodel.BuildContext(context).Identity())
}

// Log returns the consuming Plugin's pre-bound logger.
func (context BuildContext) Log() log.Logger {
	return pluginmodel.BuildContext(context).Log()
}
