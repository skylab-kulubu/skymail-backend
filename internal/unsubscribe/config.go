package unsubscribe

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// The environment (README, "Tek tık çıkış ucu"). Every key is its own
// OpenBao reference in Dokploy (ADR-0049); none is ever logged.
const (
	// LinkEnv switches SkyMail's own unsubscribe link on: the /u/ page and
	// one-click endpoint, and Link. off (the default, also when unset) or on.
	// Off, /u/ answers 404 and Link gives no link; the unsubscribe record is
	// still applied to every send, so turning it off never mails an address
	// that left.
	LinkEnv = "SKYMAIL_UNSUBSCRIBE_LINK"
	// BaseURLEnv is where /u/ is served to the public, with no trailing
	// slash: https://api.yildizskylab.com/api/skymail, as
	// SKYMAIL_TRACKING_BASE_URL. LinkEnv on needs it.
	BaseURLEnv = "SKYMAIL_UNSUBSCRIBE_BASE_URL"
	// AccountURLEnv is the Account center's communication preferences page
	// the /u/ page links a person with an account to.
	AccountURLEnv = "SKYMAIL_UNSUBSCRIBE_ACCOUNT_URL"
	// CurrentKeyEnv is the version new tokens are sealed with (1–255).
	CurrentKeyEnv = "SKYMAIL_UNSUBSCRIBE_KEY_CURRENT"
	// KeyEnvPrefix and a version name each key: SKYMAIL_UNSUBSCRIBE_KEY_V1,
	// SKYMAIL_UNSUBSCRIBE_KEY_V2, … A key is 32 random bytes in base64 (or
	// base64url, padded or not).
	KeyEnvPrefix = "SKYMAIL_UNSUBSCRIBE_KEY_V"
)

// DefaultAccountURL is the Account center's communication preferences page.
const DefaultAccountURL = "https://my.yildizskylab.com/communication-preferences"

// Config is SkyMail's own unsubscribe link as the environment sets it.
type Config struct {
	On         bool
	BaseURL    string
	AccountURL string
	Ring       *Ring
}

// Link is the one-click unsubscribe link of address in the duyuru stream:
// the base address, /u/ and the token. ok is false while the link is off,
// the ring is empty, or the address cannot be in a token; then the mail
// carries no SkyMail link.
func (c Config) Link(address string) (string, bool) {
	if !c.On || c.BaseURL == "" || c.Ring.Empty() {
		return "", false
	}
	token, err := c.Ring.Seal(StreamDuyuru, address)
	if err != nil {
		return "", false
	}
	return c.BaseURL + "/u/" + token, true
}

// ConfigFromEnv reads the link's switch, its addresses and the key ring.
// Anything malformed stops startup; errors name the variable, never a value.
// The link on with an empty ring is not an error: /u/ answers 503 until a
// key is set.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	var cfg Config
	switch raw := strings.ToLower(strings.TrimSpace(getenv(LinkEnv))); raw {
	case "", "off":
	case "on":
		cfg.On = true
	default:
		return Config{}, fmt.Errorf("%s must be off or on, not %q", LinkEnv, raw)
	}
	var err error
	if cfg.BaseURL, err = httpsURL(BaseURLEnv, getenv(BaseURLEnv)); err != nil {
		return Config{}, err
	}
	if cfg.AccountURL, err = httpsURL(AccountURLEnv, getenv(AccountURLEnv)); err != nil {
		return Config{}, err
	}
	if cfg.AccountURL == "" {
		cfg.AccountURL = DefaultAccountURL
	}
	if cfg.On && cfg.BaseURL == "" {
		return Config{}, fmt.Errorf("%s=on needs %s, the public address /u/ is served at (e.g. https://api.yildizskylab.com/api/skymail)", LinkEnv, BaseURLEnv)
	}
	if cfg.Ring, err = RingFromEnv(getenv); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// RingFromEnv reads SKYMAIL_UNSUBSCRIBE_KEY_CURRENT and every
// SKYMAIL_UNSUBSCRIBE_KEY_V<n>: the ring is exactly the versions set, so a
// version whose variable is removed (a leaked key) opens nothing from the
// next start. Neither set is an empty ring. A key without a current version,
// a current version without its key (fail fast: no start), a key that is not
// 32 bytes, or two versions holding the same key is an error.
func RingFromEnv(getenv func(string) string) (*Ring, error) {
	keys := map[byte][]byte{}
	for version := 1; version <= 255; version++ {
		name := KeyEnvPrefix + strconv.Itoa(version)
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			continue
		}
		key, err := decodeKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s must be %d random bytes in base64", name, KeySize)
		}
		for other, held := range keys {
			if bytes.Equal(held, key) {
				return nil, fmt.Errorf("%s and %s%d hold the same key; every version needs its own", name, KeyEnvPrefix, other)
			}
		}
		keys[byte(version)] = key
	}
	rawCurrent := strings.TrimSpace(getenv(CurrentKeyEnv))
	if rawCurrent == "" {
		if len(keys) > 0 {
			return nil, fmt.Errorf("%s must name the version new links are sealed with", CurrentKeyEnv)
		}
		return &Ring{}, nil
	}
	current, err := strconv.Atoi(rawCurrent)
	if err != nil || current < 1 || current > 255 {
		return nil, fmt.Errorf("%s must be a version from 1 to 255", CurrentKeyEnv)
	}
	if keys[byte(current)] == nil {
		return nil, fmt.Errorf("%s is %d but %s%d is not set", CurrentKeyEnv, current, KeyEnvPrefix, current)
	}
	return NewRing(byte(current), keys)
}

func decodeKey(raw string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err := encoding.DecodeString(raw); err == nil {
			if len(key) != KeySize {
				return nil, errors.New("wrong size")
			}
			return key, nil
		}
	}
	return nil, errors.New("not base64")
}

// httpsURL checks an absolute https address with a host and no credentials,
// query or fragment; it is returned without a trailing slash, "" when unset.
func httpsURL(name, raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%s must be an absolute https URL", name)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s must not carry credentials", name)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("%s must not have a query or a fragment", name)
	}
	return raw, nil
}
