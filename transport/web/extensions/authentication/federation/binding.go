package federation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
)

const (
	// bindingSecretBytes is the entropy of a browser-binding cookie value.
	bindingSecretBytes = 32
	// bindingDigestLength is the length of a raw-base64url SHA-256 digest.
	bindingDigestLength = 43
)

// bindingDigest returns the value persisted in Transaction.Binding for a
// browser-binding cookie secret.
func bindingDigest(secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// randomToken returns size random bytes encoded as raw base64url.
func randomToken(reader io.Reader, size int) (string, error) {
	if reader == nil {
		reader = rand.Reader
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", errors.New("federation: generate random token")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
