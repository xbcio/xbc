package oauth2

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const (
	// transactionEntropyBytes is the entropy behind state and the PKCE
	// verifier: 32 bytes, well above the 128-bit floor a login transaction
	// needs.
	transactionEntropyBytes = 32
	// minCodeVerifierLength and maxCodeVerifierLength are the RFC 7636 bounds.
	minCodeVerifierLength = 43
	maxCodeVerifierLength = 128
)

// randomToken returns size bytes from the system random source, encoded as
// unpadded base64url. The encoding alphabet is URL-safe and unreserved, which
// is exactly the PKCE code-verifier alphabet.
func randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("oauth2: random source failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

// codeChallenge derives the S256 PKCE challenge of verifier.
func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// validCodeVerifier reports whether verifier satisfies RFC 7636 section 4.1:
// 43 to 128 characters from ALPHA / DIGIT / "-" / "." / "_" / "~". A verifier
// outside the shape could not have been produced by Begin, and a missing one
// means PKCE was not performed at all.
func validCodeVerifier(verifier string) bool {
	if len(verifier) < minCodeVerifierLength || len(verifier) > maxCodeVerifierLength {
		return false
	}
	for index := 0; index < len(verifier); index++ {
		character := verifier[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '.', character == '_', character == '~':
		default:
			return false
		}
	}
	return true
}
