package saml

import (
	"fmt"
	"path"
	"strings"
)

// defaultPrefix is the mount path of the adapter's public routes. Each
// protocol adapter mounts the same {prefix}/{provider}/start shape, so the
// defaults differ per protocol: two adapters sharing one prefix would register
// the same routes twice.
const defaultPrefix = "/auth/saml"

// Config is bound from plugins.saml-http.
type Config struct {
	// Prefix is the mount path of the SAML adapter's public routes. The start,
	// assertion consumer service, and metadata routes live at
	// {prefix}/{provider}/start, {prefix}/{provider}/acs, and
	// {prefix}/{provider}/metadata. It must agree with the path component of
	// the configured ACS URL of every provider: the IdP posts the response to
	// that URL, and the browser only reaches this adapter when the two match.
	Prefix string `yaml:"prefix" default:"/auth/saml"`
}

// DefaultConfig returns the bounded defaults applied when a field is unset.
func DefaultConfig() Config {
	return Config{Prefix: defaultPrefix}
}

// Validate checks every statically checkable configuration invariant.
func (c Config) Validate() error {
	_, err := normalizeConfig(c)
	return err
}

// prepareConfig implements plugin.ConfigSpec.Prepare: it validates c without
// acquiring any resource and returns it unchanged so plan(cfg) can rely on
// already-defaulted, already-validated values.
func prepareConfig(c Config) (Config, error) {
	if _, err := normalizeConfig(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// normalizedConfig is the validated, parsed form of Config. It is built once
// per plugin instance and never mutated.
type normalizedConfig struct {
	prefix string
}

func normalizeConfig(c Config) (normalizedConfig, error) {
	prefix := strings.TrimSpace(c.Prefix)
	if prefix == "" {
		prefix = defaultPrefix
	}
	for len(prefix) > 1 && strings.HasSuffix(prefix, "/") {
		prefix = strings.TrimSuffix(prefix, "/")
	}
	if err := validatePrefix(prefix); err != nil {
		return normalizedConfig{}, err
	}
	return normalizedConfig{prefix: prefix}, nil
}

// validatePrefix confines the mount path to a literal route subtree. The value
// is joined with "/:provider/start" and handed to the engine, so anything that
// could broaden or reshape the route pattern -- a path parameter, a wildcard,
// an empty segment, a query or fragment delimiter -- is rejected at
// configuration time rather than silently mounting a different route.
func validatePrefix(prefix string) error {
	if prefix == "" || prefix[0] != '/' {
		return fmt.Errorf("saml-http: prefix must be an absolute path starting with /")
	}
	if prefix == "/" {
		return fmt.Errorf("saml-http: prefix must not be the root path; the provider parameter needs a scope of its own")
	}
	if path.Clean(prefix) != prefix {
		return fmt.Errorf("saml-http: prefix must not contain empty, dot, or dot-dot segments")
	}
	for index := 0; index < len(prefix); index++ {
		character := prefix[index]
		if character <= 0x20 || character == 0x7f {
			return fmt.Errorf("saml-http: prefix must not contain whitespace or control characters")
		}
		switch character {
		case ':', '*', '{', '}', '?', '#', '\\', '%':
			return fmt.Errorf("saml-http: prefix must be a literal path without route parameters, wildcards, escapes, or query delimiters")
		}
	}
	return nil
}
