package tracking

import (
	"crypto/rand"
	"encoding/base64"
)

// A recipient's token is 16 random bytes (2^128: not guessed, not walked), a
// link's 8. Neither is derived from the address, the row or the order of the
// send (the Garante's advice): random, so withdrawal and erasure close a row
// rather than rotate a key, and checking one is one index read.
const (
	RecipientTokenBytes = 16
	LinkTokenBytes      = 8
)

var tokenEncoding = base64.RawURLEncoding.Strict()

// NewRecipientToken is a new recipient token from crypto/rand.
func NewRecipientToken() ([]byte, error) { return newToken(RecipientTokenBytes) }

// NewLinkToken is a new link token from crypto/rand.
func NewLinkToken() ([]byte, error) { return newToken(LinkTokenBytes) }

func newToken(size int) ([]byte, error) {
	token := make([]byte, size)
	if _, err := rand.Read(token); err != nil {
		return nil, err
	}
	return token, nil
}

// EncodeToken spells a token in a URL: base64url without padding, 22
// characters for a recipient's, 11 for a link's.
func EncodeToken(token []byte) string { return tokenEncoding.EncodeToString(token) }

// DecodeToken reads a token of size bytes from a URL. Only the spelling
// EncodeToken gives is taken: the length, the URL alphabet, no padding, and
// the spare bits of the last character zero.
func DecodeToken(s string, size int) ([]byte, bool) {
	if len(s) != tokenEncoding.EncodedLen(size) {
		return nil, false
	}
	token, err := tokenEncoding.DecodeString(s)
	if err != nil || len(token) != size {
		return nil, false
	}
	return token, true
}
