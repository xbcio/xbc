package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/xbcio/xbc/extensions/authentication/federation"
)

// verifyIDToken verifies the ID token signature and claims and returns the
// external identity it names. The access token participates only in the at_hash
// check; it is never stored, logged, or returned.
func (p *Provider) verifyIDToken(ctx context.Context, response tokenResponse, nonce string) (federation.ExternalIdentity, error) {
	parsed, err := jose.ParseSignedCompact(response.idToken, joseAlgorithms(p.algorithms))
	if err != nil {
		// The parse error names neither the token nor its payload; the
		// details are deliberately dropped so no IdP-supplied text can reach
		// a caller that forwards error bodies.
		return federation.ExternalIdentity{}, fmt.Errorf("%w: the JOSE header is not acceptable", ErrInvalidIDToken)
	}
	if len(parsed.Signatures) != 1 {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: exactly one signature is required", ErrInvalidIDToken)
	}
	header := parsed.Signatures[0].Header
	if header.KeyID == "" {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: the JOSE header carries no key ID", ErrInvalidIDToken)
	}

	keys, err := p.signingKeys(ctx, header.KeyID, header.Algorithm)
	if err != nil {
		return federation.ExternalIdentity{}, err
	}
	payload, verified := verifySignature(parsed, keys)
	if !verified {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: signature verification failed", ErrInvalidIDToken)
	}

	claims, err := decodeClaims(payload)
	if err != nil {
		return federation.ExternalIdentity{}, err
	}
	return p.validateClaims(claims, header.Algorithm, response.accessToken, nonce)
}

// verifySignature tries every selected key. More than one key can share a key
// ID during a rotation, and a key that does not verify simply is not the
// signing key.
func verifySignature(parsed *jose.JSONWebSignature, keys []jose.JSONWebKey) ([]byte, bool) {
	for index := range keys {
		if payload, err := parsed.Verify(keys[index]); err == nil {
			return payload, true
		}
	}
	return nil, false
}

// decodeClaims decodes the verified payload. Only a JSON object of claims is
// accepted.
func decodeClaims(payload []byte) (map[string]any, error) {
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil || claims == nil {
		return nil, fmt.Errorf("%w: payload is not a JSON object of claims", ErrInvalidIDToken)
	}
	return claims, nil
}

// validateClaims applies the OIDC ID token validation rules to the verified
// claim set and builds the external identity.
func (p *Provider) validateClaims(claims map[string]any, algorithm, accessToken, expectedNonce string) (federation.ExternalIdentity, error) {
	invalid := func(format string, arguments ...any) (federation.ExternalIdentity, error) {
		return federation.ExternalIdentity{}, fmt.Errorf("%w: %s", ErrInvalidIDToken, fmt.Sprintf(format, arguments...))
	}
	now := p.now()

	issuer, ok := stringClaim(claims, "iss")
	if !ok || issuer != p.issuer {
		return invalid("iss does not match the registered issuer")
	}

	audiences, ok := audienceClaim(claims)
	if !ok || len(audiences) == 0 {
		return invalid("aud is missing or malformed")
	}
	if !slices.Contains(audiences, p.clientID) {
		return invalid("aud does not contain this client")
	}
	authorizedParty, hasAuthorizedParty := claims["azp"]
	if len(audiences) > 1 || hasAuthorizedParty {
		value, ok := authorizedParty.(string)
		if !ok || value != p.clientID {
			return invalid("azp is required to equal this client")
		}
	}

	expiration, ok := numericClaim(claims, "exp")
	if !ok {
		return invalid("exp is missing or malformed")
	}
	if now.After(time.Unix(int64(expiration), 0).Add(p.leeway)) {
		return invalid("the ID token has expired")
	}

	if raw, present := claims["nbf"]; present {
		notBefore, ok := numericValue(raw)
		if !ok {
			return invalid("nbf is malformed")
		}
		if now.Add(p.leeway).Before(time.Unix(int64(notBefore), 0)) {
			return invalid("the ID token is not valid yet")
		}
	}

	issuedAt, ok := numericClaim(claims, "iat")
	if !ok {
		return invalid("iat is missing or malformed")
	}
	if now.Add(p.leeway).Before(time.Unix(int64(issuedAt), 0)) {
		return invalid("the ID token was issued in the future")
	}

	nonce, ok := stringClaim(claims, "nonce")
	if !ok || nonce == "" {
		return invalid("nonce is missing")
	}
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(expectedNonce)) != 1 {
		return invalid("nonce does not match the login transaction")
	}

	if raw, present := claims["at_hash"]; present {
		provided, ok := raw.(string)
		if !ok {
			return invalid("at_hash is malformed")
		}
		expected, err := accessTokenHash(algorithm, accessToken)
		if err != nil {
			return invalid("%v", err)
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			return invalid("at_hash does not match the access token")
		}
	}

	subject, ok := stringClaim(claims, "sub")
	if !ok || subject == "" {
		return invalid("sub is missing")
	}

	authTime := time.Unix(int64(issuedAt), 0).UTC()
	if raw, present := claims["auth_time"]; present {
		value, ok := numericValue(raw)
		if !ok {
			return invalid("auth_time is malformed")
		}
		authTime = time.Unix(int64(value), 0).UTC()
	}

	emailVerified := false
	if raw, present := claims["email_verified"]; present {
		value, ok := raw.(bool)
		if !ok {
			return invalid("email_verified is malformed")
		}
		emailVerified = value
	}

	normalized := make(map[string]any, len(claims))
	for name, value := range claims {
		normalized[name] = value
	}
	normalized["email_verified"] = emailVerified

	return federation.ExternalIdentity{
		Protocol: protocolName,
		Provider: p.name,
		Issuer:   issuer,
		Subject:  subject,
		Claims:   normalized,
		AuthTime: authTime,
	}, nil
}

// stringClaim returns a non-empty string claim.
func stringClaim(claims map[string]any, name string) (string, bool) {
	value, ok := claims[name].(string)
	if !ok || value == "" {
		return "", false
	}
	return value, true
}

// numericClaim returns a numeric claim.
func numericClaim(claims map[string]any, name string) (float64, bool) {
	raw, present := claims[name]
	if !present {
		return 0, false
	}
	return numericValue(raw)
}

// numericValue converts a decoded JSON number. encoding/json decodes every
// number into float64, so any other type is malformed for a NumericDate.
func numericValue(raw any) (float64, bool) {
	value, ok := raw.(float64)
	return value, ok
}

// audienceClaim normalizes aud, which is either one string or an array of
// strings.
func audienceClaim(claims map[string]any) ([]string, bool) {
	switch raw := claims["aud"].(type) {
	case string:
		if raw == "" {
			return nil, false
		}
		return []string{raw}, true
	case []any:
		audiences := make([]string, 0, len(raw))
		for _, entry := range raw {
			value, ok := entry.(string)
			if !ok || value == "" {
				return nil, false
			}
			audiences = append(audiences, value)
		}
		return audiences, true
	default:
		return nil, false
	}
}

// joseAlgorithms converts the configured allowlist into the form go-jose
// requires at parse time. Parsing with the allowlist is what rejects alg=none
// and every HMAC algorithm before any key is consulted.
func joseAlgorithms(algorithms []string) []jose.SignatureAlgorithm {
	converted := make([]jose.SignatureAlgorithm, 0, len(algorithms))
	for _, algorithm := range algorithms {
		converted = append(converted, jose.SignatureAlgorithm(algorithm))
	}
	return converted
}

// accessTokenHash computes the at_hash value for an access token: the
// base64url encoding of the left half of the hash the ID token signature
// algorithm uses.
func accessTokenHash(algorithm, accessToken string) (string, error) {
	newHash, ok := signatureHash(algorithm)
	if !ok {
		return "", fmt.Errorf("signature algorithm %q has no at_hash rule", algorithm)
	}
	hasher := newHash()
	if _, err := hasher.Write([]byte(accessToken)); err != nil {
		return "", fmt.Errorf("hashing the access token failed")
	}
	sum := hasher.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2]), nil
}
