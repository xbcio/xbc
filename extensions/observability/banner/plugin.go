package banner

import (
	"errors"
	"strings"

	"github.com/xbcio/xbc/plugin"
)

// Key is this plugin's stable configuration and dependency identity. It owns
// the plugins.banner configuration section.
const Key plugin.Key = "banner"

// Plugin renders one startup banner during Start. It holds normalized
// configuration only: no connection, goroutine, or managed task outlives the
// hook, so the plugin has no Drain or Stop stage.
type Plugin struct {
	config normalizedConfig
}

var definition = plugin.DefineConfigured(
	Key,
	plugin.ConfigSpec[Config]{
		Defaults: DefaultConfig,
		Prepare:  prepareConfig,
	},
	func(_ plugin.BuildContext, cfg Config) (*Plugin, error) {
		normalized, err := normalizeConfig(cfg)
		if err != nil {
			return nil, err
		}
		return &Plugin{config: normalized}, nil
	},
	plugin.Options[*Plugin]{
		// Zero Activation: selecting the Bundle is the explicit decision, and
		// the universal plugins.banner.enabled=false switch still vetoes it.
		// The factory prints nothing: XBC assembles a composition with an
		// empty configuration, so a factory that rendered would emit a banner
		// for every diagnostic too.
		//
		// Start is the earliest phase whose output is a deliberate boot
		// message: factories run for doctor and validate, and Init is meant
		// for ownership transfer rather than output. The hook order is the
		// plan's dependency order, with dependency-free instances in identity
		// order, so this plugin -- which declares no inputs -- runs alongside
		// the first dependency-free plugins.
		Lifecycle: plugin.Lifecycle[*Plugin]{
			Start: (*Plugin).start,
		},
	},
)

var bundle = plugin.BundleOf(definition)

// Definition returns banner's canonical immutable declaration handle.
func Definition() plugin.Definition { return definition }

// Bundle returns banner's side-effect-free explicit composition bundle.
func Bundle() plugin.Bundle { return bundle }

// start renders and records the one banner this process emits. It sends no
// fields: the message is display text, and a single record keeps the JSON
// encoding one well-formed object while the console encoding remains the
// banner's own multiple lines.
func (p *Plugin) start(ctx *plugin.Context) error {
	if ctx == nil {
		return errors.New("banner: Start requires a plugin context")
	}
	text := p.config.text
	if text == "" {
		text = strings.TrimSpace(ctx.Service().Name)
	}
	if text == "" {
		text = strings.TrimSpace(ctx.ProcessInstance())
	}
	if text == "" {
		text = defaultText
	}
	ctx.Log().Info(renderBanner(p.config, text))
	return nil
}
