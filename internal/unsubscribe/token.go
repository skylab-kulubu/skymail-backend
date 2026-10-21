// Package unsubscribe is the part of SkyMail's own one-click unsubscribe
// that needs no database: the key ring, the token and the link (ADR 0072
// decision 4; hub ticket abonelik-cikisi 02; README, "Tek tık çıkış ucu").
//
// A token names a stream and an address and nothing else: it is not tied to
// a send or a queue row, so retention never breaks a link, and it does not
// expire. It is sealed, not only signed, so the address is not readable in
// the URL. Each token carries the version of the key that sealed it: a new
// token is sealed with the current key, an older version is kept only to
// open the tokens already mailed. A version is never removed: every link
// mailed with it would stop working.
package unsubscribe

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// StreamDuyuru is the duyuru stream, the one broadcast stream yet.
const StreamDuyuru = "duyuru"

// streamTags spell a stream in a token. A tag is never reused.
var streamTags = map[string]byte{StreamDuyuru: 1}

// MaxAddressLength bounds an address a token carries (RFC 5321's path).
const MaxAddressLength = 254

// KeySize is the size of one key of the ring: XChaCha20-Poly1305's.
const KeySize = chacha20poly1305.KeySize

// aad binds a token to this use and to its version: a key of the ring can
// open nothing sealed for another purpose.
const aad = "skymail unsubscribe v"

var (
	// ErrInvalid says a token does not open: changed, cut, or not a token.
	ErrInvalid = errors.New("unsubscribe link is not valid")
	// ErrUnknownVersion says a token names a key version the ring does not
	// hold.
	ErrUnknownVersion = errors.New("unsubscribe link names an unknown key version")
	// ErrNoKey says the ring is empty: no token can be sealed or opened.
	ErrNoKey = errors.New("unsubscribe key ring is empty")
)

var tokenEncoding = base64.RawURLEncoding.Strict()

// Subject is what a token names.
type Subject struct {
	Stream  string
	Address string
}

// Ring is the keys tokens are sealed and opened with, by version (1–255).
// The zero Ring is empty.
type Ring struct {
	current byte
	keys    map[byte]cipher.AEAD
}

// NewRing is a ring of keys (32 bytes each) by version, sealing with
// current, which must be one of them.
func NewRing(current byte, keys map[byte][]byte) (*Ring, error) {
	if current == 0 {
		return nil, errors.New("key version 0 is not a version")
	}
	ring := &Ring{current: current, keys: make(map[byte]cipher.AEAD, len(keys))}
	for version, key := range keys {
		if version == 0 {
			return nil, errors.New("key version 0 is not a version")
		}
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return nil, err
		}
		ring.keys[version] = aead
	}
	if ring.keys[current] == nil {
		return nil, errors.New("the current key version has no key")
	}
	return ring, nil
}

// Empty reports whether the ring holds no key.
func (r *Ring) Empty() bool { return r == nil || len(r.keys) == 0 }

// Current is the version new tokens are sealed with, 0 for an empty ring.
func (r *Ring) Current() byte {
	if r.Empty() {
		return 0
	}
	return r.current
}

// Versions is how many versions the ring holds.
func (r *Ring) Versions() int {
	if r.Empty() {
		return 0
	}
	return len(r.keys)
}

// NormalizeAddress is how an address is kept and compared: trimmed and
// lower-case, as mail_suppressions and mail_unsubscribes hold it.
func NormalizeAddress(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

// validAddress says address may be in a token: normalized, at most
// MaxAddressLength bytes, something on each side of an @, and no control
// character or space.
func validAddress(address string) bool {
	if address == "" || len(address) > MaxAddressLength || address != NormalizeAddress(address) {
		return false
	}
	at := strings.LastIndexByte(address, '@')
	if at < 1 || at == len(address)-1 {
		return false
	}
	for i := 0; i < len(address); i++ {
		if c := address[i]; c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

// Seal is the token for address in stream, sealed with the current key: the
// version, a random nonce, and the stream's tag and the address encrypted
// and authenticated, in unpadded base64url. address is normalized first.
func (r *Ring) Seal(stream, address string) (string, error) {
	if r.Empty() {
		return "", ErrNoKey
	}
	tag, ok := streamTags[stream]
	address = NormalizeAddress(address)
	if !ok || !validAddress(address) {
		return "", ErrInvalid
	}
	aead := r.keys[r.current]
	out := make([]byte, 1+aead.NonceSize(), 1+aead.NonceSize()+1+len(address)+aead.Overhead())
	out[0] = r.current
	if _, err := rand.Read(out[1:]); err != nil {
		return "", err
	}
	plain := append([]byte{tag}, address...)
	out = aead.Seal(out, out[1:1+aead.NonceSize()], plain, additionalData(r.current))
	return tokenEncoding.EncodeToString(out), nil
}

// Open reads a token: ErrUnknownVersion for a version the ring does not
// hold, ErrInvalid for anything that does not open, and ErrNoKey when the
// ring is empty.
func (r *Ring) Open(token string) (Subject, error) {
	if r.Empty() {
		return Subject{}, ErrNoKey
	}
	// The longest token: version, nonce, tag, the longest address, tag.
	if token == "" || len(token) > tokenEncoding.EncodedLen(1+chacha20poly1305.NonceSizeX+1+MaxAddressLength+chacha20poly1305.Overhead) {
		return Subject{}, ErrInvalid
	}
	raw, err := tokenEncoding.DecodeString(token)
	if err != nil || len(raw) < 1+chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead+2 {
		return Subject{}, ErrInvalid
	}
	aead, ok := r.keys[raw[0]]
	if !ok {
		return Subject{}, ErrUnknownVersion
	}
	nonce := raw[1 : 1+aead.NonceSize()]
	plain, err := aead.Open(nil, nonce, raw[1+aead.NonceSize():], additionalData(raw[0]))
	if err != nil || len(plain) < 2 {
		return Subject{}, ErrInvalid
	}
	stream := ""
	for name, tag := range streamTags {
		if tag == plain[0] {
			stream = name
		}
	}
	address := string(plain[1:])
	if stream == "" || !validAddress(address) {
		return Subject{}, ErrInvalid
	}
	return Subject{Stream: stream, Address: address}, nil
}

func additionalData(version byte) []byte {
	return append([]byte(aad), version)
}
