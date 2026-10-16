package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

type publicTrackingFake struct {
	link             database.ResolveTrackingLinkRow
	err              error
	panic            bool
	recordPanic      bool
	recordErr        error
	lookups, records int
}

func (f *publicTrackingFake) ResolveTrackingLink(context.Context, []byte) (database.ResolveTrackingLinkRow, error) {
	f.lookups++
	if f.panic {
		panic("secret-token-should-not-escape")
	}
	return f.link, f.err
}
func (f *publicTrackingFake) RecordTracking(context.Context, []byte, int64, string, string) (bool, error) {
	f.records++
	if f.recordPanic {
		panic("private-recording-panic")
	}
	if f.recordErr != nil {
		return false, f.recordErr
	}
	if f.panic {
		panic("secret-token-should-not-escape")
	}
	return f.err == nil, f.err
}
func publicTrackingWorld(f *publicTrackingFake, on bool) (*fiber.App, *TrackingPublicHandler) {
	app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error { panic("general error handler reached: " + err.Error()) }})
	mode := tracking.ModeOff
	if on {
		mode = tracking.ModeOn
	}
	h := NewTrackingPublicHandler(f, tracking.Config{Mode: mode})
	h.Register(app)
	return app, h
}
func trackingCall(t *testing.T, app *fiber.App, method, path string) (int, map[string]string, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("User-Agent", "raw-user-agent-sentinel")
	req.Header.Set("Referer", "https://referrer-sentinel.invalid/private")
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	headers := map[string]string{}
	for key, values := range resp.Header {
		headers[key] = strings.Join(values, ",")
	}
	return resp.StatusCode, headers, body
}
func TestPublicPixelCannotRevealTokenState(t *testing.T) {
	rt := tracking.EncodeToken(make([]byte, 16))
	for _, method := range []string{"GET", "HEAD"} {
		var originalHeaders map[string]string
		var originalBody []byte
		for _, scenario := range []string{"valid", "missing", "error", "panic", "off", "malformed", "prefix"} {
			f := &publicTrackingFake{}
			if scenario == "missing" {
				f.err = pgx.ErrNoRows
			}
			if scenario == "error" {
				f.err = errors.New("private-db-error")
			}
			f.panic = scenario == "panic"
			app, _ := publicTrackingWorld(f, scenario != "off")
			path := "/t/o/" + rt
			if scenario == "malformed" {
				path = "/t/o/garbage"
			}
			if scenario == "prefix" {
				path = "/t/o/" + rt + "/extra/private"
			}
			status, headers, body := trackingCall(t, app, method, path)
			if status != 200 || (method == "GET" && len(body) != 43) || headers["Content-Type"] != "image/gif" || headers["Cache-Control"] != "no-store, no-cache, must-revalidate, private, max-age=0" || headers["Set-Cookie"] != "" || headers["Access-Control-Allow-Origin"] != "" {
				t.Fatalf("%s %s: %d %v %d", method, scenario, status, headers, len(body))
			}
			if originalHeaders == nil {
				originalHeaders = headers
				originalBody = body
			} else if headers["Pragma"] != originalHeaders["Pragma"] || headers["Expires"] != originalHeaders["Expires"] || !bytes.Equal(body, originalBody) {
				t.Errorf("pixel differs for %s", scenario)
			}
		}
	}
}
func TestPublicClickRedirectsOnlyToStoredTargetAndHEADNeverRecords(t *testing.T) {
	url := "https://destination.invalid/a?fixed=1"
	rt := tracking.EncodeToken(make([]byte, 16))
	lt := tracking.EncodeToken(make([]byte, 8))
	for _, method := range []string{"GET", "HEAD"} {
		for _, recipient := range []string{rt, "invalid"} {
			f := &publicTrackingFake{link: database.ResolveTrackingLinkRow{ID: 1, Url: &url, CreatedAt: time.Now()}}
			app, _ := publicTrackingWorld(f, true)
			status, headers, _ := trackingCall(t, app, method, "/t/c/"+recipient+"/"+lt+"?url=https://attacker.invalid")
			if status != 302 || headers["Location"] != url || headers["Referrer-Policy"] != "no-referrer" {
				t.Errorf("redirect %d %v", status, headers)
			}
			expected := 0
			if method == "GET" && recipient == rt {
				expected = 1
			}
			if f.records != expected {
				t.Errorf("%s records=%d", method, f.records)
			}
		}
	}
	for _, scenario := range []string{"missing", "malformed", "expired", "erased", "unsafe", "error", "panic"} {
		f := &publicTrackingFake{link: database.ResolveTrackingLinkRow{ID: 1, Url: &url, CreatedAt: time.Now()}}
		path := "/t/c/" + rt + "/" + lt
		want := 410
		switch scenario {
		case "missing":
			f.err = pgx.ErrNoRows
			want = 404
		case "malformed":
			path += "/extra"
			want = 404
		case "expired":
			f.link.CreatedAt = time.Now().AddDate(-1, 0, -1)
		case "erased":
			reason := "erased"
			f.link.Url = nil
			f.link.UrlClearedReason = &reason
		case "unsafe":
			bad := "javascript:alert(1)"
			f.link.Url = &bad
			want = 503
		case "error":
			f.err = errors.New("private-db-error")
			want = 503
		case "panic":
			f.panic = true
			want = 503
		}
		app, _ := publicTrackingWorld(f, true)
		status, headers, _ := trackingCall(t, app, "GET", path)
		if status != want || headers["Location"] != "" || f.records != 0 {
			t.Errorf("%s: %d %v records=%d", scenario, status, headers, f.records)
		}
	}
}
func TestPublicTrackingLogsContainOnlyEndpointResultAndDuration(t *testing.T) {
	var out bytes.Buffer
	previous := log.Logger
	log.Logger = zerolog.New(&out)
	defer func() { log.Logger = previous }()
	rt := tracking.EncodeToken(bytes.Repeat([]byte{0x6a}, 16))
	lt := tracking.EncodeToken(bytes.Repeat([]byte{0x7b}, 8))
	url := "https://private-target.invalid"
	for _, err := range []error{nil, pgx.ErrNoRows, errors.New("secret-db-value-" + rt)} {
		for _, panicValue := range []bool{false, true} {
			f := &publicTrackingFake{link: database.ResolveTrackingLinkRow{ID: 1, Url: &url, CreatedAt: time.Now()}, err: err, panic: panicValue}
			app, _ := publicTrackingWorld(f, true)
			trackingCall(t, app, "GET", "/t/o/"+rt)
			trackingCall(t, app, "GET", "/t/c/"+rt+"/"+lt)
		}
	}
	for _, secret := range []string{rt, lt, "raw-user-agent-sentinel", "referrer-sentinel", "198.51.100.99", "/t/o/", "/t/c/", "private-target", "secret-db-value", "secret-token"} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("tracking log contains %q", secret)
		}
	}
	if !strings.Contains(out.String(), `"endpoint":"open"`) || !strings.Contains(out.String(), `"endpoint":"click"`) {
		t.Fatal("missing endpoint logs")
	}
}
func TestPublicTrackingBoundsProcessState(t *testing.T) {
	f := &publicTrackingFake{err: pgx.ErrNoRows}
	app, h := publicTrackingWorld(f, true)
	now := time.Now()
	h.now = func() time.Time { return now }
	h.refilled = now
	rt := tracking.EncodeToken(make([]byte, 16))
	for i := 0; i < 100; i++ {
		trackingCall(t, app, "GET", "/t/o/"+rt)
	}
	if f.records != 1 {
		t.Fatalf("negative cache: %d database calls", f.records)
	}
	for i := 0; i < 5000; i++ {
		h.missing('l', []byte{byte(i), byte(i >> 8)}, true)
	}
	if len(h.negative) > 2048 {
		t.Fatal("unbounded cache")
	}
	for i := 0; i < 100; i++ {
		h.allow()
	}
	if h.allow() {
		t.Fatal("unbounded bucket")
	}
	now = now.Add(time.Second)
	if !h.allow() {
		t.Fatal("bucket did not refill")
	}
}

func TestPublicClickRecordingFailureCannotBreakTheRedirect(t *testing.T) {
	url := "https://destination.invalid/registered"
	rt := tracking.EncodeToken(make([]byte, 16))
	lt := tracking.EncodeToken(make([]byte, 8))
	for _, panicValue := range []bool{false, true} {
		fake := &publicTrackingFake{link: database.ResolveTrackingLinkRow{ID: 1, Url: &url, CreatedAt: time.Now()}, recordErr: errors.New("private-recording-error"), recordPanic: panicValue}
		app, h := publicTrackingWorld(fake, true)
		status, headers, _ := trackingCall(t, app, "GET", "/t/c/"+rt+"/"+lt)
		if status != 302 || headers["Location"] != url || len(h.slots) != 0 {
			t.Errorf("record failure broke redirect: %d %v slots=%d", status, headers, len(h.slots))
		}
	}
}

func FuzzPublicClickNeverReflectsATarget(f *testing.F) {
	f.Add("invalid", "invalid", "https://attacker.invalid")
	f.Add(tracking.EncodeToken(make([]byte, 16)), tracking.EncodeToken(make([]byte, 8)), "https://attacker.invalid")
	f.Add("../../private", "%00?url=https://attacker.invalid", "javascript:alert(1)")
	previous := log.Logger
	log.Logger = zerolog.Nop()
	defer func() { log.Logger = previous }()
	f.Fuzz(func(t *testing.T, rt, lt, target string) {
		if len(rt) > 256 || len(lt) > 256 || len(target) > 256 {
			return
		}
		registered := "https://destination.invalid/registered?fixed=1"
		fake := &publicTrackingFake{link: database.ResolveTrackingLinkRow{ID: 1, Url: &registered, CreatedAt: time.Now()}}
		app, _ := publicTrackingWorld(fake, false)
		path := "/t/c/" + url.PathEscape(rt) + "/" + url.PathEscape(lt) + "?target=" + url.QueryEscape(target)
		_, headers, _ := trackingCall(t, app, "GET", path)
		if location := headers["Location"]; location != "" && location != registered {
			t.Fatalf("request became redirect target: %q", location)
		}
	})
}
