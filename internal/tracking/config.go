// Package tracking is the part of open and click tracking that needs no
// database: the flag, the tokens, which links a click is counted on, putting
// the pixel and the click addresses into a mail as it goes out, and asking
// core who may be tracked (hub ticket iletisim-servisi 14; design note
// notes/iletisim-servisi-arastirma/07 §4).
package tracking

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Mode is SKYMAIL_TRACKING: whether this process tracks the sends it queues.
type Mode string

const (
	// ModeOff, the default: no token is written, nothing is put into a mail,
	// a token already queued is closed when its mail goes out.
	ModeOff Mode = "off"
	// ModeOn tracks the bulk sends that ask for it, for the people core
	// lets be tracked.
	ModeOn Mode = "on"
)

// Config is open and click tracking as the environment sets it.
type Config struct {
	Mode Mode
	// BaseURL is where SkyMail's /t/ addresses are served to the public, with
	// no trailing slash (T6: https://api.yildizskylab.com/api/skymail). Every
	// tracked mail carries it.
	BaseURL string
	// CoreURL is core over the Docker network, with no trailing slash: who
	// may be tracked is asked there.
	CoreURL string
}

// On reports whether tracking is on.
func (c Config) On() bool { return c.Mode == ModeOn }

// ConfigFromEnv reads SKYMAIL_TRACKING (off, the default, or on),
// SKYMAIL_TRACKING_BASE_URL and CORE_URL. On needs both addresses. A mode
// other than off or on, or an address set but not well formed, is an error
// that stops startup, so a typo can neither turn tracking on nor send mail
// with addresses that lead nowhere. Errors name the variable; an address's
// value is never in them.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{}
	raw := strings.TrimSpace(getenv("SKYMAIL_TRACKING"))
	switch Mode(raw) {
	case "", ModeOff:
		cfg.Mode = ModeOff
	case ModeOn:
		cfg.Mode = ModeOn
	default:
		return Config{}, fmt.Errorf("SKYMAIL_TRACKING must be off or on, not %q", raw)
	}

	var err error
	if cfg.BaseURL, err = baseURL("SKYMAIL_TRACKING_BASE_URL", getenv("SKYMAIL_TRACKING_BASE_URL"), true); err != nil {
		return Config{}, err
	}
	if cfg.CoreURL, err = baseURL("CORE_URL", getenv("CORE_URL"), false); err != nil {
		return Config{}, err
	}
	if cfg.On() && cfg.BaseURL == "" {
		return Config{}, errors.New("SKYMAIL_TRACKING=on needs SKYMAIL_TRACKING_BASE_URL, the public address /t/ is served at (e.g. https://api.yildizskylab.com/api/skymail)")
	}
	if cfg.On() && cfg.CoreURL == "" {
		return Config{}, errors.New("SKYMAIL_TRACKING=on needs CORE_URL, core's address on the Docker network (e.g. http://core-backend:8080)")
	}
	return cfg, nil
}

// baseURL checks an address SkyMail adds paths to: absolute, https when
// public (http or https otherwise), with a host and no credentials, query or
// fragment. It is returned without a trailing slash; "" when unset.
func baseURL(name, raw string, public bool) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	want := "an absolute http or https URL"
	if public {
		want = "an absolute https URL"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s must be %s", name, want)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "https" && (public || scheme != "http")) || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%s must be %s", name, want)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s must not carry credentials", name)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("%s must not have a query or a fragment", name)
	}
	return raw, nil
}
