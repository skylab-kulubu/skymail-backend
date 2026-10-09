package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// `skymail-backend healthcheck` is the container's health check: 0 when this
// container's SkyMail answers /ready?gate=skip with 204, 1 otherwise (Docker reserves
// 2), saying why on stderr.
func TestHealthcheckCommand(t *testing.T) {
	t.Parallel()
	// The handler runs on the server's goroutines.
	var mu sync.Mutex
	status := http.StatusNoContent
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = r.URL.RequestURI()
		code := status
		mu.Unlock()
		w.WriteHeader(code)
	}))
	defer server.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))

	var errOut bytes.Buffer
	if code := runHealthcheck(nil, env(map[string]string{"APP_PORT": port}), &errOut); code != 0 {
		t.Fatalf("ready: exit %d %s", code, errOut.String())
	}
	// The task and its database, not the account access gate's Redis.
	mu.Lock()
	if asked != "/ready?gate=skip" {
		mu.Unlock()
		t.Fatalf("asked %q, want /ready?gate=skip", asked)
	}
	status = http.StatusServiceUnavailable
	mu.Unlock()
	errOut.Reset()
	if code := runHealthcheck(nil, env(map[string]string{"APP_PORT": port}), &errOut); code != 1 || !strings.Contains(errOut.String(), "503") {
		t.Fatalf("not ready: exit %d %q", code, errOut.String())
	}

	// Nothing listening.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closed, _ := net.SplitHostPort(ln.Addr().String())
	ln.Close()
	errOut.Reset()
	if code := runHealthcheck(nil, env(map[string]string{"APP_PORT": closed}), &errOut); code != 1 || errOut.Len() == 0 {
		t.Fatalf("nothing listening: exit %d %q", code, errOut.String())
	}
	if code := runHealthcheck([]string{"extra"}, env(nil), &errOut); code != 1 {
		t.Fatalf("an argument: exit %d", code)
	}
}

// Readiness's one connection lives long: re-made every hour (pgx's
// default), it could meet a database at max_connections and fail the health
// check of a SkyMail that is fine. Opening connects nothing.
func TestReadinessDatabaseKeepsItsConnection(t *testing.T) {
	t.Parallel()
	pool, err := openReadinessDatabase("postgres://skymail:secret@127.0.0.1:1/skymail?sslmode=disable&pool_max_conns=5")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	config := pool.Config()
	if config.MaxConns != readinessDatabaseConns || config.MaxConnLifetime < 24*time.Hour || config.MaxConnIdleTime < 24*time.Hour {
		t.Fatalf("MaxConns %d, MaxConnLifetime %s, MaxConnIdleTime %s", config.MaxConns, config.MaxConnLifetime, config.MaxConnIdleTime)
	}
	if _, err := openReadinessDatabase("::not a url"); err == nil || strings.Contains(err.Error(), "not a url") {
		t.Fatalf("a bad address: %v (must not quote it)", err)
	}
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
