package mail

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	gomail "github.com/wneessen/go-mail"

	"github.com/xbcio/xbc/extensions/reliability/health"
	"github.com/xbcio/xbc/plugin"
)

// Key is the stable configuration and runtime identity of the mail plugin.
const Key plugin.Key = "mail"

// ErrClosed is returned when a send or readiness probe runs after the client
// was stopped.
var ErrClosed = errors.New("mail: client is closed")

// Sender is the minimal contract a configured instance exports. Consumers
// that resolve it can be exercised with a test double; consumers that need
// message construction import go-mail directly, because the message value is
// the library's own type.
type Sender interface {
	DialAndSend(messages ...*gomail.Msg) error
}

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: func() Config { return Config{} },
		Prepare:  prepareConfig,
	},
	newClient,
	plugin.Options[*Client]{
		Instances:  plugin.MultipleInstances,
		Activation: plugin.WhenConfigured("plugins.mail"),
		Inputs:     plugin.Inputs(),
		Exports: plugin.Contracts(
			plugin.ExportAs[Sender](func(value *Client) Sender { return value }),
			plugin.ExportAs[health.Contributor](func(value *Client) health.Contributor { return value }),
		),
		Lifecycle: plugin.Lifecycle[*Client]{
			Stop: stopClient,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns this integration's canonical Definition handle.
func Definition() plugin.Definition { return definition }

// Bundle returns this integration's side-effect-free composition bundle.
func Bundle() plugin.Bundle { return bundle }

// Client is the primary value of one configured SMTP instance. It is a thin
// wrapper rather than an export of the library client: consumers get the send
// method they need, while the library's full method set -- including transport
// knobs this plugin owns -- stays private. The private client also keeps the
// library's credential fields out of fmt output.
//
// A Client is safe for concurrent sends. Each send dials its own connection and
// closes it afterwards, so an SMTP server's idle-connection timeout never
// affects delivery.
type Client struct {
	client            *gomail.Client
	instance          string
	address           string
	verify            bool
	connectionTimeout time.Duration
	closed            atomic.Bool
}

var (
	_ Sender             = (*Client)(nil)
	_ health.Contributor = (*Client)(nil)
)

func newClient(ctx plugin.BuildContext, cfg Config) (*Client, error) {
	return buildClient(cfg, ctx.Identity().Normalized().Instance)
}

// buildClient constructs the one client a configured instance owns. When
// verification is enabled, it dials once and closes that probe connection: the
// connection proves configuration, credentials, and server reachability, but a
// mail client sends through its own per-call connection, so keeping the probe
// open would only hold an idle socket until the server times it out.
//
// Errors name the instance and host:port only. The password never reaches an
// error, a log line, or formatted output.
func buildClient(cfg Config, instance string) (*Client, error) {
	if instance == "" {
		instance = plugin.DefaultInstance
	}
	options, err := cfg.clientOptions()
	if err != nil {
		return nil, fmt.Errorf("mail: instance %s: %w", instance, err)
	}
	client, err := gomail.NewClient(cfg.Host, options...)
	if err != nil {
		return nil, fmt.Errorf("mail: instance %s: create client for %s: %w", instance, cfg.address(), err)
	}
	value := &Client{
		client:            client,
		instance:          instance,
		address:           cfg.address(),
		verify:            cfg.Verify,
		connectionTimeout: cfg.ConnectionTimeout,
	}
	if !cfg.Verify {
		return value, nil
	}
	// The library applies its own connection timeout as a deadline on top of
	// this context, so a zero ConnectionTimeout still bounds the probe.
	probeCtx := context.Background()
	if cfg.ConnectionTimeout > 0 {
		var cancel context.CancelFunc
		probeCtx, cancel = context.WithTimeout(probeCtx, cfg.ConnectionTimeout)
		defer cancel()
	}
	if err := client.DialWithContext(probeCtx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("mail: instance %s: connect %s: %w", instance, cfg.address(), err)
	}
	if err := client.Close(); err != nil {
		return nil, fmt.Errorf("mail: instance %s: close verification connection to %s: %w", instance, cfg.address(), err)
	}
	return value, nil
}

// DialAndSend connects to the configured server, sends the messages, and closes
// the connection. It is the minimal Sender contract.
func (c *Client) DialAndSend(messages ...*gomail.Msg) error {
	return c.DialAndSendWithContext(context.Background(), messages...)
}

// DialAndSendWithContext is DialAndSend bounded by ctx.
func (c *Client) DialAndSendWithContext(ctx context.Context, messages ...*gomail.Msg) error {
	if c == nil || c.client == nil {
		return errors.New("mail: client is nil")
	}
	if ctx == nil {
		return errors.New("mail: send requires a non-nil context")
	}
	if c.closed.Load() {
		return ErrClosed
	}
	return c.client.DialAndSendWithContext(ctx, messages...)
}

// String renders the instance identity and address, never credentials.
func (c *Client) String() string {
	if c == nil {
		return "mail: <nil>"
	}
	identity := plugin.Identity{Plugin: Key, Instance: c.instance}
	return identity.String() + ": " + c.address
}

// GoString renders the same safe form for %#v.
func (c *Client) GoString() string { return c.String() }

// stopClient is the typed lifecycle adapter. The library's close is a no-op
// when no connection is open, which makes this adapter idempotent; marking the
// client closed also rejects sends accepted after shutdown began.
func stopClient(client *Client, _ context.Context) error {
	if client == nil {
		return nil
	}
	client.closed.Store(true)
	if client.client == nil {
		return nil
	}
	return client.client.Close()
}
