package jwt

import (
	"errors"
	"fmt"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"
)

// Claims is the map of verified JWT claims returned by Authenticate and
// published as web.Principal.Attributes.
type Claims = jwtlib.MapClaims

var errInvalidToken = errors.New("jwt: invalid token")

type compiledConfig struct {
	normalizedConfig
	method jwtlib.SigningMethod
	parser *jwtlib.Parser
}

func compileConfig(cfg Config, now func() time.Time) (*compiledConfig, error) {
	normalized, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	method, ok := signingMethod(normalized.algorithm)
	if !ok {
		return nil, fmt.Errorf("jwt: unsupported signing algorithm %q", normalized.algorithm)
	}

	options := []jwtlib.ParserOption{
		jwtlib.WithValidMethods(normalized.algorithms),
		jwtlib.WithExpirationRequired(),
		jwtlib.WithLeeway(normalized.leeway),
		jwtlib.WithStrictDecoding(),
		jwtlib.WithTimeFunc(now),
	}
	if normalized.issuer != "" {
		options = append(options, jwtlib.WithIssuer(normalized.issuer))
	}
	if len(normalized.audience) > 0 {
		options = append(options, jwtlib.WithAudience(normalized.audience...))
	}

	return &compiledConfig{
		normalizedConfig: normalized,
		method:           method,
		parser:           jwtlib.NewParser(options...),
	}, nil
}

func signingMethod(algorithm string) (jwtlib.SigningMethod, bool) {
	switch algorithm {
	case "HS256":
		return jwtlib.SigningMethodHS256, true
	case "HS384":
		return jwtlib.SigningMethodHS384, true
	case "HS512":
		return jwtlib.SigningMethodHS512, true
	default:
		return nil, false
	}
}

// Sign issues a token for subject using Config.Expire (2h by default). The
// caller's map is copied, and exp/iat/sub plus configured iss/aud are owned by
// the plugin so every returned token has a trustworthy expiration.
func (p *Plugin) Sign(subject string, claims Claims) (string, error) {
	runtime := p.compiled
	return p.sign(runtime, subject, claims, runtime.expire)
}

// SignWithTTL is Sign with a per-token positive lifetime override. It still
// always writes exp and never trusts an exp supplied in claims.
func (p *Plugin) SignWithTTL(subject string, claims Claims, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("jwt: token ttl must be greater than zero")
	}
	return p.sign(p.compiled, subject, claims, ttl)
}

func (p *Plugin) sign(runtime *compiledConfig, subject string, custom Claims, ttl time.Duration) (string, error) {
	claims := make(jwtlib.MapClaims, len(custom)+5)
	for key, value := range custom {
		claims[key] = value
	}

	now := p.clock()
	claims["iat"] = jwtlib.NewNumericDate(now)
	claims["exp"] = jwtlib.NewNumericDate(now.Add(ttl))
	if subject == "" {
		delete(claims, "sub")
	} else {
		claims["sub"] = subject
	}
	if runtime.issuer != "" {
		claims["iss"] = runtime.issuer
	}
	if len(runtime.audience) == 1 {
		claims["aud"] = runtime.audience[0]
	} else if len(runtime.audience) > 1 {
		claims["aud"] = append([]string(nil), runtime.audience...)
	}

	token := jwtlib.NewWithClaims(runtime.method, claims)
	signed, err := token.SignedString(runtime.secret)
	if err != nil {
		return "", fmt.Errorf("jwt: sign token: %w", err)
	}
	return signed, nil
}

func (runtime *compiledConfig) verify(raw string) (Claims, error) {
	claims := jwtlib.MapClaims{}
	token, err := runtime.parser.ParseWithClaims(raw, claims, func(token *jwtlib.Token) (any, error) {
		if _, ok := token.Method.(*jwtlib.SigningMethodHMAC); !ok {
			return nil, errInvalidToken
		}
		if _, ok := runtime.allowed[token.Method.Alg()]; !ok {
			return nil, errInvalidToken
		}
		return runtime.secret, nil
	})
	if err != nil || token == nil || !token.Valid {
		return nil, errInvalidToken
	}
	return claims, nil
}
