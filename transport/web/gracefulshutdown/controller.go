package gracefulshutdown

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/xbcio/xbc/plugin"
)

// Controller requests the framework's normal graceful shutdown path. It is
// safe for concurrent use. Request returns true only for the caller whose
// request was accepted first by the runtime.
type Controller struct {
	mu      sync.RWMutex
	request func(string) bool
	done    <-chan struct{}
}

var (
	_ plugin.Initializer = (*Controller)(nil)
	_ plugin.Closer      = (*Controller)(nil)
)

// New creates an unbound Controller. XBC binds Controllers produced by
// Definition during Init; an unbound Controller safely rejects requests.
func New() *Controller { return new(Controller) }

// Init binds this Controller to its own runtime identity and cancellation
// scope. The Controller itself is the Definition's primary value; it is never
// published through a lifecycle-time registry.
func (c *Controller) Init(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("gracefulshutdown: Init requires a plugin context")
	}
	c.bind(ctx.RequestShutdown, ctx.Done())
	return nil
}

// Request initiates graceful shutdown with an operator-facing reason. Blank
// and multiline input is normalized before it reaches runtime diagnostics.
func (c *Controller) Request(reason string) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	request := c.request
	done := c.done
	c.mu.RUnlock()
	if request == nil {
		return false
	}
	if done != nil {
		select {
		case <-done:
			return false
		default:
		}
	}
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		reason = "operator request"
	}
	return request(reason)
}

// ShuttingDown reports whether XBC has started cancellation for this process.
func (c *Controller) ShuttingDown() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	done := c.done
	c.mu.RUnlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// Stop detaches future callers; shutdown itself is orchestrated by core.
func (c *Controller) Stop(context.Context) error {
	c.unbind()
	return nil
}

func (c *Controller) bind(request func(string) bool, done <-chan struct{}) {
	c.mu.Lock()
	c.request = request
	c.done = done
	c.mu.Unlock()
}

func (c *Controller) unbind() {
	c.mu.Lock()
	c.request = nil
	c.mu.Unlock()
}
