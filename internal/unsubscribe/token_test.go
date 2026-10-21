package unsubscribe

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func key(fill byte) []byte { return bytes.Repeat([]byte{fill}, KeySize) }

func ring(t *testing.T, current byte, keys map[byte][]byte) *Ring {
	t.Helper()
	r, err := NewRing(current, keys)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A token opens to its stream and normalized address, and carries neither in
// the clear: not in the URL text, not in its bytes.
func TestATokenOpensToItsAddressAndShowsNothing(t *testing.T) {
	r := ring(t, 1, map[byte][]byte{1: key(1)})
	token, err := r.Seal(StreamDuyuru, "  Mehmet.Yilmaz@Example.COM ")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Open(token)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Subject{Stream: StreamDuyuru, Address: "mehmet.yilmaz@example.com"}) {
		t.Errorf("opened %+v", got)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"mehmet", "yilmaz", "example", "duyuru"} {
		if strings.Contains(strings.ToLower(token), part) || bytes.Contains(bytes.ToLower(raw), []byte(part)) {
			t.Errorf("the token shows %q: %s", part, token)
		}
	}
	if again, _ := r.Seal(StreamDuyuru, "mehmet.yilmaz@example.com"); again == token {
		t.Error("two tokens for one address are the same: the nonce is not random")
	}
}

// A token sealed with V1 still opens once V2 is current; a new token carries
// V2; a version the ring does not hold is ErrUnknownVersion.
func TestTheRingKeepsOldVersionsForOpeningOnly(t *testing.T) {
	v1 := ring(t, 1, map[byte][]byte{1: key(1)})
	old, err := v1.Seal(StreamDuyuru, "ayse@example.com")
	if err != nil {
		t.Fatal(err)
	}
	v2 := ring(t, 2, map[byte][]byte{1: key(1), 2: key(2)})
	if got, err := v2.Open(old); err != nil || got.Address != "ayse@example.com" {
		t.Errorf("a V1 token under V2: %+v, %v", got, err)
	}
	fresh, err := v2.Seal(StreamDuyuru, "ayse@example.com")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(fresh)
	if raw[0] != 2 {
		t.Errorf("a new token carries version %d, want 2", raw[0])
	}
	if _, err := v1.Open(fresh); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("a V2 token under a ring without V2: %v, want ErrUnknownVersion", err)
	}
	// A version byte changed to one the ring holds does not open either:
	// the version is authenticated.
	raw[0] = 1
	if _, err := v2.Open(base64.RawURLEncoding.EncodeToString(raw)); !errors.Is(err, ErrInvalid) {
		t.Errorf("V2 relabelled V1: %v, want ErrInvalid", err)
	}
}

// Any changed byte, a cut, or something that is not a token is ErrInvalid;
// an empty ring is ErrNoKey.
func TestAChangedOrCutTokenDoesNotOpen(t *testing.T) {
	r := ring(t, 1, map[byte][]byte{1: key(1)})
	token, err := r.Seal(StreamDuyuru, "ayse@example.com")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(token)
	for i := 1; i < len(raw); i++ {
		changed := append([]byte(nil), raw...)
		changed[i] ^= 0x01
		if _, err := r.Open(base64.RawURLEncoding.EncodeToString(changed)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("byte %d changed: %v, want ErrInvalid", i, err)
		}
	}
	for _, bad := range []string{"", token[:len(token)-1], token[:20], token + "A", "a+b/c==", strings.Repeat("A", 1000)} {
		if _, err := r.Open(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%.30q: %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := (&Ring{}).Open(token); !errors.Is(err, ErrNoKey) {
		t.Errorf("an empty ring: %v, want ErrNoKey", err)
	}
	if _, err := (&Ring{}).Seal(StreamDuyuru, "ayse@example.com"); !errors.Is(err, ErrNoKey) {
		t.Errorf("an empty ring seals: %v", err)
	}
	for _, address := range []string{"", "ayse", "@example.com", "ayse@", "a yse@example.com", strings.Repeat("a", 250) + "@x.co"} {
		if _, err := r.Seal(StreamDuyuru, address); !errors.Is(err, ErrInvalid) {
			t.Errorf("seal %.30q: %v, want ErrInvalid", address, err)
		}
	}
	if _, err := r.Seal("islem", "ayse@example.com"); !errors.Is(err, ErrInvalid) {
		t.Errorf("seal for islem: %v, want ErrInvalid", err)
	}
}

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

var (
	keyV1 = base64.StdEncoding.EncodeToString(key(1))
	keyV2 = base64.RawURLEncoding.EncodeToString(key(2))
)

func TestTheRingAndTheLinkAreReadFromTheEnvironment(t *testing.T) {
	cfg, err := ConfigFromEnv(env(nil))
	if err != nil || cfg.On || !cfg.Ring.Empty() || cfg.AccountURL != DefaultAccountURL {
		t.Fatalf("unset: %+v, %v; want off, an empty ring, the Account center", cfg, err)
	}
	if _, ok := cfg.Link("ayse@example.com"); ok {
		t.Error("off gives a link")
	}

	cfg, err = ConfigFromEnv(env(map[string]string{
		LinkEnv: "on", BaseURLEnv: "https://api.yildizskylab.com/api/skymail/",
		CurrentKeyEnv: "2", KeyEnvPrefix + "1": keyV1, KeyEnvPrefix + "2": keyV2,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Ring.Current() != 2 || cfg.Ring.Versions() != 2 {
		t.Errorf("ring current %d of %d versions", cfg.Ring.Current(), cfg.Ring.Versions())
	}
	link, ok := cfg.Link("Ayse@Example.com")
	if !ok || !strings.HasPrefix(link, "https://api.yildizskylab.com/api/skymail/u/") {
		t.Fatalf("link %q, %v", link, ok)
	}
	got, err := cfg.Ring.Open(strings.TrimPrefix(link, "https://api.yildizskylab.com/api/skymail/u/"))
	if err != nil || got.Address != "ayse@example.com" {
		t.Errorf("the link opens to %+v, %v", got, err)
	}

	// On with no key: no error (the page answers 503), no link.
	cfg, err = ConfigFromEnv(env(map[string]string{LinkEnv: "on", BaseURLEnv: "https://api.yildizskylab.com/api/skymail"}))
	if err != nil || !cfg.Ring.Empty() {
		t.Fatalf("on without a key: %+v, %v", cfg, err)
	}
	if _, ok := cfg.Link("ayse@example.com"); ok {
		t.Error("an empty ring gives a link")
	}

	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 31))
	for name, values := range map[string]map[string]string{
		"a typo":                     {LinkEnv: "yes"},
		"on without its address":     {LinkEnv: "on"},
		"an http address":            {BaseURLEnv: "http://api.yildizskylab.com/api/skymail"},
		"an address with a query":    {BaseURLEnv: "https://api.yildizskylab.com/api/skymail?x=1"},
		"a short key":                {CurrentKeyEnv: "1", KeyEnvPrefix + "1": secret},
		"a key that is not base64":   {CurrentKeyEnv: "1", KeyEnvPrefix + "1": "not base64 at all!"},
		"a key without current":      {KeyEnvPrefix + "1": keyV1},
		"current without its key":    {CurrentKeyEnv: "2", KeyEnvPrefix + "1": keyV1},
		"current out of range":       {CurrentKeyEnv: "256", KeyEnvPrefix + "1": keyV1},
		"current not a number":       {CurrentKeyEnv: "v1", KeyEnvPrefix + "1": keyV1},
		"two versions, the same key": {CurrentKeyEnv: "2", KeyEnvPrefix + "1": keyV1, KeyEnvPrefix + "2": keyV1},
	} {
		_, err := ConfigFromEnv(env(values))
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), keyV1) {
			t.Errorf("%s: the error shows the key: %v", name, err)
		}
	}
}
