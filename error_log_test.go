package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// lockedBuffer is a log sink the server's goroutines write to.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// An IP address is personal data (hub rule): the 404 a missing row answers
// is logged with the client's class — public, private or loopback — never
// its address or its User-Agent, whoever the trusted proxy says it is.
func TestANotFoundIsLoggedWithTheClientsClassOnly(t *testing.T) {
	sink := &lockedBuffer{}
	previous := log.Logger
	log.Logger = zerolog.New(sink)
	t.Cleanup(func() { log.Logger = previous })

	// The production config, the test's socket a trusted proxy, so the
	// X-Forwarded-For below is what ctx.IP() reads.
	app := fiber.New(newFiberConfig(nil, []string{"127.0.0.0/8"}))
	app.Get("/v1/templates/:id", func(fiber.Ctx) error { return pgx.ErrNoRows })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = app.Shutdown() })

	const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) Gizli/1.0"
	for _, c := range []struct{ forwarded, class string }{
		{"203.0.113.7", "public"},
		{"2001:db8::7", "public"},
		{"10.0.3.12", "private"},
		{"172.18.0.5", "private"},
		{"192.168.1.20", "private"},
		{"fd12:3456::1", "private"},
		{"", "loopback"},
	} {
		request, err := http.NewRequest(fiber.MethodGet, "http://"+listener.Addr().String()+"/v1/templates/40000000-0000-4000-8000-000000000001", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set(fiber.HeaderUserAgent, userAgent)
		if c.forwarded != "" {
			request.Header.Set(fiber.HeaderXForwardedFor, c.forwarded)
		}
		before := len(sink.String())
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != fiber.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", c.forwarded, response.StatusCode)
		}
		written := strings.TrimSpace(sink.String()[before:])
		var line map[string]any
		if err := json.Unmarshal([]byte(written), &line); err != nil {
			t.Fatalf("%s: log %q: %v", c.forwarded, written, err)
		}
		if line["client"] != c.class {
			t.Errorf("%s: client = %v, want %s", c.forwarded, line["client"], c.class)
		}
		for _, key := range []string{"ip", "ua", "user_agent", "remote_addr"} {
			if _, ok := line[key]; ok {
				t.Errorf("%s: the line has %q: %s", c.forwarded, key, written)
			}
		}
		for _, leak := range []string{"203.0.113", "2001:db8", "10.0.3", "172.18", "192.168", "fd12", "127.0.0.1", "Gizli", "Mozilla"} {
			if strings.Contains(written, leak) {
				t.Errorf("%s: the line holds %q: %s", c.forwarded, leak, written)
			}
		}
	}
}

// The class of an address, as logs carry it instead of the address.
func TestClientClass(t *testing.T) {
	for raw, want := range map[string]string{
		"203.0.113.7":  "public",
		"8.8.8.8":      "public",
		"10.0.0.1":     "private",
		"172.16.0.1":   "private",
		"192.168.0.1":  "private",
		"100.64.0.1":   "public",
		"127.0.0.1":    "loopback",
		"::1":          "loopback",
		"fc00::1":      "private",
		"fe80::1":      "link_local",
		"169.254.0.1":  "link_local",
		"":             "unknown",
		"not-an-ip":    "unknown",
		"0.0.0.0":      "unknown",
		"10.0.0.1:443": "unknown",
	} {
		if got := clientClass(raw); got != want {
			t.Errorf("clientClass(%q) = %q, want %q", raw, got, want)
		}
	}
}
