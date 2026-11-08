package teams

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fixedToken string

func (t fixedToken) ServiceToken(context.Context) (string, error) { return string(t), nil }

type failingToken struct{}

func (failingToken) ServiceToken(context.Context) (string, error) {
	return "", errors.New("keycloak down")
}

const person = "0b6f7a52-5a1e-4c55-9b5e-0d4f9c1d2a10"

var (
	weblab   = uuid.MustParse("6d3c9f8e-2a4b-4c1d-8e7f-1a2b3c4d5e6f")
	frontend = uuid.MustParse("7e4d0a9f-3b5c-4d2e-9f80-2b3c4d5e6f70")
	algolab  = uuid.MustParse("8f5e1b0a-4c6d-4e3f-8091-3c4d5e6f7081")
)

// core answers the person's Teams; calls counts the requests.
func core(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/internal/v1/users/"+person+"/teams" || r.Header.Get("Authorization") != "Bearer svc" || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

const answerBody = `{"teams":[
 {"id":"` + "8f5e1b0a-4c6d-4e3f-8091-3c4d5e6f7081" + `","path":"/UYELER/ARGE/ALGOLAB","code":"ALGOLAB","displayName":{"tr":"Algo Lab"},"role":"member"},
 {"id":"` + "7e4d0a9f-3b5c-4d2e-9f80-2b3c4d5e6f70" + `","path":"/UYELER/ARGE/WEBLAB/FRONTEND","code":"FRONTEND","displayName":{"tr":"Ön Yüz","en":"Frontend"},"role":"leader"},
 {"id":"` + "6d3c9f8e-2a4b-4c1d-8e7f-1a2b3c4d5e6f" + `","path":"/UYELER/ARGE/WEBLAB","code":"WEBLAB","displayName":{"tr":"Web Lab"},"role":"leader"}
]}`

// Only the leader rows, in path order, with their names; member rows are
// never Team mail's.
func TestLedIsTheLeaderRowsInPathOrder(t *testing.T) {
	server, _ := core(t, http.StatusOK, answerBody)
	led, err := NewCore(server.URL, fixedToken("svc"), nil).LedNow(context.Background(), person)
	if err != nil {
		t.Fatal(err)
	}
	if len(led) != 2 || led[0].ID != weblab || led[1].ID != frontend {
		t.Fatalf("led = %+v", led)
	}
	if led[1].DisplayName.TR != "Ön Yüz" || led[1].DisplayName.EN != "Frontend" || led[1].Code != "FRONTEND" {
		t.Fatalf("frontend = %+v", led[1])
	}
	for _, team := range led {
		if team.ID == algolab {
			t.Fatal("a member row was listed")
		}
	}
}

// A Leader group's own path is never a Team, whatever core says.
func TestALeaderGroupIsNeverLed(t *testing.T) {
	server, _ := core(t, http.StatusOK, `{"teams":[
	 {"id":"`+weblab.String()+`","path":"/UYELER/ARGE/WEBLAB/LIDERLER","code":"LIDERLER","role":"leader"},
	 {"id":"`+frontend.String()+`","path":"/UYELER/ARGE/WEBLAB/KOORDINATORLER","code":"KOORDINATORLER","role":"leader"}]}`)
	led, err := NewCore(server.URL, fixedToken("svc"), nil).LedNow(context.Background(), person)
	if err != nil || len(led) != 0 {
		t.Fatalf("led = %+v, %v", led, err)
	}
}

// Core not answering, or answering off the contract, is no answer; SkyMail
// never falls back to a reading of its own.
func TestCoreNotAnsweringIsNoAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"503":             {http.StatusServiceUnavailable, `{"code":"keycloak_unavailable"}`},
		"403":             {http.StatusForbidden, `{"code":"person_teams_role_missing"}`},
		"no teams":        {http.StatusOK, `{}`},
		"not json":        {http.StatusOK, `<html>`},
		"bad id":          {http.StatusOK, `{"teams":[{"id":"x","path":"/UYELER/A","role":"leader"}]}`},
		"bad role":        {http.StatusOK, `{"teams":[{"id":"` + weblab.String() + `","path":"/UYELER/A","role":"owner"}]}`},
		"path not a path": {http.StatusOK, `{"teams":[{"id":"` + weblab.String() + `","path":"UYELER","role":"leader"}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			server, _ := core(t, tc.status, tc.body)
			if led, err := NewCore(server.URL, fixedToken("svc"), nil).LedNow(context.Background(), person); err == nil {
				t.Fatalf("led = %+v, want an error", led)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		if _, err := NewCore("http://127.0.0.1:1", fixedToken("svc"), nil).LedNow(context.Background(), person); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("no service token", func(t *testing.T) {
		server, calls := core(t, http.StatusOK, answerBody)
		if _, err := NewCore(server.URL, failingToken{}, nil).LedNow(context.Background(), person); err == nil || calls.Load() != 0 {
			t.Fatalf("err %v, calls %d", err, calls.Load())
		}
	})
	t.Run("sub no id", func(t *testing.T) {
		server, calls := core(t, http.StatusOK, answerBody)
		if _, err := NewCore(server.URL, fixedToken("svc"), nil).LedNow(context.Background(), "../x"); err == nil || calls.Load() != 0 {
			t.Fatalf("err %v, calls %d", err, calls.Load())
		}
	})
}

// Led answers from what core said within a minute; LedNow always asks.
// A failure is never kept.
func TestLedKeepsAnAnswerForAMinute(t *testing.T) {
	server, calls := core(t, http.StatusOK, answerBody)
	c := NewCore(server.URL, fixedToken("svc"), nil)
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	ctx := context.Background()
	for range 3 {
		if _, err := c.Led(ctx, person); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("core asked %d times within the minute, want 1", calls.Load())
	}
	if _, err := c.LedNow(ctx, person); err != nil || calls.Load() != 2 {
		t.Fatalf("LedNow: %v, calls %d", err, calls.Load())
	}
	now = now.Add(CacheTTL)
	if _, err := c.Led(ctx, person); err != nil || calls.Load() != 3 {
		t.Fatalf("after a minute: %v, calls %d", err, calls.Load())
	}

	failing, failingCalls := core(t, http.StatusServiceUnavailable, `{}`)
	f := NewCore(failing.URL, fixedToken("svc"), nil)
	for range 2 {
		if _, err := f.Led(ctx, person); err == nil {
			t.Fatal("no error")
		}
	}
	if failingCalls.Load() != 2 {
		t.Fatalf("a failure was kept: core asked %d times", failingCalls.Load())
	}
}

func TestSwitchFromEnv(t *testing.T) {
	for raw, want := range map[string]bool{"": false, "off": false, " ON ": true, "on": true} {
		got, err := SwitchFromEnv(func(string) string { return raw })
		if err != nil || got != want {
			t.Errorf("%q = %v, %v", raw, got, err)
		}
	}
	if _, err := SwitchFromEnv(func(string) string { return "yes" }); err == nil {
		t.Error("yes was accepted")
	}
}

func TestWithin(t *testing.T) {
	for _, tc := range []struct {
		path, parent string
		want         bool
	}{
		{"/UYELER/ARGE/WEBLAB", "/UYELER/ARGE/WEBLAB", true},
		{"/UYELER/ARGE/WEBLAB/FRONTEND", "/UYELER/ARGE/WEBLAB", true},
		{"/UYELER/ARGE/WEBLAB2", "/UYELER/ARGE/WEBLAB", false},
		{"/UYELER/ARGE", "/UYELER/ARGE/WEBLAB", false},
	} {
		if got := Within(tc.path, tc.parent); got != tc.want {
			t.Errorf("Within(%q, %q) = %v", tc.path, tc.parent, got)
		}
	}
}
