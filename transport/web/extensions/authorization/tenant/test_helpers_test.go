package tenant

import (
	"context"

	"github.com/xbcio/xbc/log"
	"github.com/xbcio/xbc/plugin"
)

type testHost struct{}

func (testHost) ExecutionContext() context.Context { return context.Background() }
func (testHost) Logger() log.Logger                { return log.Nop() }
func (testHost) TrafficGate() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}
func (testHost) SubmitTask(plugin.Identity, func(context.Context), bool) bool { return false }
func (testHost) RequestShutdown(plugin.Identity, string) bool                 { return true }

func testContext() *plugin.Context {
	return plugin.NewRuntimeContext(testHost{}, plugin.Identity{Plugin: Key, Instance: plugin.DefaultInstance})
}
