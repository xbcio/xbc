package session

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// CarrierCookie selects the built-in cookie carrier. It is the default and
	// the only built-in carrier that writes.
	CarrierCookie = "cookie"
	// CarrierHeader selects the built-in read-only header carrier, by default
	// Authorization: Session <id>.
	CarrierHeader = "header"
)

// Config is bound from the optional plugins.session-http section.
//
// The section is optional, and an absent section means exactly the historical
// behavior: the session ID travels in the cookie configured by plugins.session
// and nowhere else. Presenting the section selects the ordered carrier chain;
// Carriers lists the credential sources in the order they are consulted. Any
// source reporting Malformed fails the whole chain regardless of position, so a
// duplicated or unusable credential is never masked by a later usable one.
//
// Exactly one carrier may write, and the header carrier never does. A chain
// containing only the header carrier can verify existing sessions but cannot
// log a new one in.
//
// Presence of this section never activates the plugin. Activation stays tied to
// plugins.session: configuring plugins.session-http without plugins.session
// leaves the plugin disabled, exactly as before the section existed.
type Config struct {
	// Carriers is the ordered credential source list. Empty selects cookie.
	Carriers []string `yaml:"carriers"`
	// Header configures the read-only header carrier.
	Header HeaderConfig `yaml:"header"`
}

// HeaderConfig configures the read-only header carrier.
type HeaderConfig struct {
	// Name is the header name. Empty selects Authorization.
	Name string `yaml:"name"`
	// Scheme is the scheme expected before the ID. Empty selects Session; a
	// raw, scheme-less header is not supported.
	Scheme string `yaml:"scheme"`
}

// DefaultConfig returns the documented defaults: the cookie carrier alone,
// with the header carrier pre-configured for a later selection.
func DefaultConfig() Config {
	return Config{
		Carriers: []string{CarrierCookie},
		Header: HeaderConfig{
			Name:   DefaultHeaderCarrierConfig().Header,
			Scheme: DefaultHeaderCarrierConfig().Scheme,
		},
	}
}

// Validate checks every statically checkable configuration invariant.
func (config Config) Validate() error {
	_, err := normalizeConfig(config)
	return err
}

type normalizedConfig struct {
	carriers []string
	header   HeaderCarrierConfig
}

func normalizeConfig(config Config) (normalizedConfig, error) {
	carriers, err := normalizeCarriers(config.Carriers)
	if err != nil {
		return normalizedConfig{}, err
	}
	header := HeaderCarrierConfig{
		Header: strings.TrimSpace(config.Header.Name),
		Scheme: strings.TrimSpace(config.Header.Scheme),
	}
	if header.Header == "" {
		header.Header = defaultHeaderName
	}
	if !validHTTPToken(header.Header) {
		return normalizedConfig{}, errors.New("session-http: plugins.session-http.header.name must be a valid HTTP token")
	}
	if header.Scheme == "" {
		header.Scheme = defaultHeaderScheme
	}
	if !validHTTPToken(header.Scheme) {
		return normalizedConfig{}, errors.New("session-http: plugins.session-http.header.scheme must be a valid HTTP token")
	}
	return normalizedConfig{carriers: carriers, header: header}, nil
}

// normalizeCarriers lowercases and trims the configured names, rejects unknown
// carriers and duplicates, and expands an empty list to the cookie default.
func normalizeCarriers(values []string) ([]string, error) {
	carriers := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		carrier := strings.ToLower(strings.TrimSpace(value))
		switch carrier {
		case "":
			continue
		case CarrierCookie, CarrierHeader:
		default:
			return nil, fmt.Errorf(
				"session-http: plugins.session-http.carriers contains unknown carrier %q; use %q or %q",
				value, CarrierCookie, CarrierHeader)
		}
		if seen[carrier] {
			return nil, fmt.Errorf("session-http: plugins.session-http.carriers lists %q more than once", carrier)
		}
		seen[carrier] = true
		carriers = append(carriers, carrier)
	}
	if len(carriers) == 0 {
		carriers = []string{CarrierCookie}
	}
	return carriers, nil
}

// prepareConfig implements plugin.ConfigSpec.Prepare. It performs no I/O and
// returns the configuration unchanged so the planner can rely on already
// validated values.
func prepareConfig(config Config) (Config, error) {
	if _, err := normalizeConfig(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

// buildCarriers resolves the configured names to carriers. The cookie carrier
// is always the one built from the neutral plugin's published settings.
func buildCarriers(config normalizedConfig, cookie *CookieCarrier) ([]Carrier, error) {
	carriers := make([]Carrier, 0, len(config.carriers))
	for _, name := range config.carriers {
		switch name {
		case CarrierCookie:
			carriers = append(carriers, cookie)
		case CarrierHeader:
			header, err := NewHeaderCarrier(config.header)
			if err != nil {
				return nil, err
			}
			carriers = append(carriers, header)
		default:
			return nil, fmt.Errorf("session-http: unknown carrier %q", name)
		}
	}
	return carriers, nil
}
