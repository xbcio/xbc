package session

import "encoding/base64"

func testID(seed byte, size int) string {
	raw := make([]byte, size)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
