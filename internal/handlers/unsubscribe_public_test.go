package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime/multipart"
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
	"github.com/skylab-kulubu/skymail-backend/internal/unsubscribe"
)

// unsubscribeFake is the record in memory: the addresses out of a stream and
// how many times the person resubscribed.
type unsubscribeFake struct {
	out           map[string]bool
	resubscribed  int
	reads, writes int
	err           error
	panic         bool
}

func (f *unsubscribeFake) GetUnsubscribe(_ context.Context, arg database.GetUnsubscribeParams) (database.GetUnsubscribeRow, error) {
	f.reads++
	if f.panic {
		panic("panic-with-" + arg.Address)
	}
	if f.err != nil {
		return database.GetUnsubscribeRow{}, f.err
	}
	if !f.out[arg.Stream+" "+arg.Address] {
		return database.GetUnsubscribeRow{}, pgx.ErrNoRows
	}
	return database.GetUnsubscribeRow{Address: arg.Address, Stream: arg.Stream, Source: "link"}, nil
}

func (f *unsubscribeFake) Unsubscribe(_ context.Context, address, stream string) (bool, error) {
	f.writes++
	if f.err != nil {
		return false, f.err
	}
	created := !f.out[stream+" "+address]
	f.out[stream+" "+address] = true
	return created, nil
}

func (f *unsubscribeFake) Resubscribe(_ context.Context, address, stream string) (bool, error) {
	f.writes++
	if f.err != nil {
		return false, f.err
	}
	if !f.out[stream+" "+address] {
		return false, nil
	}
	delete(f.out, stream+" "+address)
	f.resubscribed++
	return true, nil
}

func testRing(t *testing.T, current byte, versions ...byte) *unsubscribe.Ring {
	t.Helper()
	keys := map[byte][]byte{}
	for _, v := range versions {
		keys[v] = bytes.Repeat([]byte{v}, unsubscribe.KeySize)
	}
	ring, err := unsubscribe.NewRing(current, keys)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

type unsubscribeWorld struct {
	app   *fiber.App
	h     *UnsubscribePublicHandler
	store *unsubscribeFake
	ring  *unsubscribe.Ring
	logs  *bytes.Buffer
}

func newUnsubscribeWorld(t *testing.T, cfg unsubscribe.Config) *unsubscribeWorld {
	t.Helper()
	logs := &bytes.Buffer{}
	previous := log.Logger
	log.Logger = zerolog.New(logs)
	t.Cleanup(func() { log.Logger = previous })
	store := &unsubscribeFake{out: map[string]bool{}}
	app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error { panic("general error handler reached: " + err.Error()) }})
	h := NewUnsubscribePublicHandler(store, cfg)
	h.Register(app)
	return &unsubscribeWorld{app: app, h: h, store: store, ring: cfg.Ring, logs: logs}
}

func onConfig(ring *unsubscribe.Ring) unsubscribe.Config {
	return unsubscribe.Config{On: true, BaseURL: "https://api.yildizskylab.com/api/skymail", AccountURL: unsubscribe.DefaultAccountURL, Ring: ring}
}

func (w *unsubscribeWorld) token(t *testing.T, address string) string {
	t.Helper()
	token, err := w.ring.Seal(unsubscribe.StreamDuyuru, address)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type unsubscribeAnswer struct {
	status  int
	headers map[string]string
	body    string
}

func (w *unsubscribeWorld) call(t *testing.T, method, path, contentType string, body io.Reader) unsubscribeAnswer {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if contentType != "" {
		req.Header.Set(fiber.HeaderContentType, contentType)
	}
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	resp, err := w.app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	headers := map[string]string{}
	for key, values := range resp.Header {
		headers[key] = strings.Join(values, ",")
	}
	return unsubscribeAnswer{status: resp.StatusCode, headers: headers, body: string(raw)}
}

func (w *unsubscribeWorld) form(t *testing.T, path string, values url.Values) unsubscribeAnswer {
	t.Helper()
	return w.call(t, fiber.MethodPost, path, fiber.MIMEApplicationForm, strings.NewReader(values.Encode()))
}

// Every answer: no cookie, no redirect, no cache, no referrer, never framed,
// no script.
func assertUnsubscribeHeaders(t *testing.T, what string, a unsubscribeAnswer) {
	t.Helper()
	want := map[string]string{
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
	}
	for name, value := range want {
		if a.headers[name] != value {
			t.Errorf("%s: %s = %q, want %q", what, name, a.headers[name], value)
		}
	}
	for _, name := range []string{"Set-Cookie", "Location"} {
		if a.headers[name] != "" {
			t.Errorf("%s: %s = %q, want none", what, name, a.headers[name])
		}
	}
	if a.status >= 300 && a.status < 400 {
		t.Errorf("%s: %d, a redirect", what, a.status)
	}
	if strings.Contains(strings.ToLower(a.body), "<script") {
		t.Errorf("%s: the page has a script", what)
	}
}

// GET shows the question and writes nothing; HEAD neither. The address is
// shown masked: its first letter and its domain.
func TestTheUnsubscribePageAsksAndWritesNothing(t *testing.T) {
	w := newUnsubscribeWorld(t, onConfig(testRing(t, 1, 1)))
	path := "/u/" + w.token(t, "mehmet.yilmaz@example.com")
	for _, method := range []string{fiber.MethodGet, fiber.MethodHead} {
		a := w.call(t, method, path, "", nil)
		if a.status != fiber.StatusOK {
			t.Fatalf("%s = %d: %s", method, a.status, a.body)
		}
		assertUnsubscribeHeaders(t, method, a)
	}
	a := w.call(t, fiber.MethodGet, path, "", nil)
	if !strings.Contains(a.body, "Bu adres SKY LAB duyuru postalarından çıkarılsın mı?") || !strings.Contains(a.body, "m…@example.com") ||
		!strings.Contains(a.body, `name="eylem" value="cikis"`) || !strings.Contains(a.body, unsubscribe.DefaultAccountURL) {
		t.Errorf("page = %s", a.body)
	}
	if strings.Contains(a.body, "mehmet") || strings.Contains(a.body, "yilmaz") {
		t.Errorf("the page shows the address: %s", a.body)
	}
	if w.store.writes != 0 || len(w.store.out) != 0 {
		t.Errorf("GET wrote %d times", w.store.writes)
	}
}

// A mail client's one-click POST (form-encoded or multipart) records the
// unsubscribe and answers 200 ok; a second answers the same. The page then
// says the address is out and offers to resubscribe.
func TestOneClickUnsubscribesAndCanBeRepeated(t *testing.T) {
	w := newUnsubscribeWorld(t, onConfig(testRing(t, 1, 1)))
	path := "/u/" + w.token(t, "Ayse@Example.com")
	for i := 0; i < 2; i++ {
		a := w.form(t, path, url.Values{"List-Unsubscribe": {"One-Click"}})
		if a.status != fiber.StatusOK || a.body != "ok" {
			t.Fatalf("one-click %d = %d %q", i+1, a.status, a.body)
		}
		assertUnsubscribeHeaders(t, "one-click", a)
	}
	var multi bytes.Buffer
	writer := multipart.NewWriter(&multi)
	_ = writer.WriteField("List-Unsubscribe", "One-Click")
	_ = writer.Close()
	if a := w.call(t, fiber.MethodPost, path, writer.FormDataContentType(), &multi); a.status != fiber.StatusOK || a.body != "ok" {
		t.Errorf("multipart one-click = %d %q", a.status, a.body)
	}
	if len(w.store.out) != 1 || !w.store.out["duyuru ayse@example.com"] {
		t.Errorf("record = %v, want the normalized address once", w.store.out)
	}
	a := w.call(t, fiber.MethodGet, path, "", nil)
	if a.status != fiber.StatusOK || !strings.Contains(a.body, "Çıkarıldı") || !strings.Contains(a.body, `name="eylem" value="geri"`) {
		t.Errorf("page after = %d %s", a.status, a.body)
	}
}

// The page's buttons: eylem=cikis unsubscribes, eylem=geri resubscribes
// (the person's own reversal); both can be repeated. Anything else is 400.
func TestThePageButtonsLeaveAndComeBack(t *testing.T) {
	w := newUnsubscribeWorld(t, onConfig(testRing(t, 1, 1)))
	path := "/u/" + w.token(t, "deniz@example.com")
	if a := w.form(t, path, url.Values{"eylem": {"cikis"}}); a.status != fiber.StatusOK || !strings.Contains(a.body, "Çıkarıldın") {
		t.Fatalf("cikis = %d %s", a.status, a.body)
	}
	if !w.store.out["duyuru deniz@example.com"] {
		t.Fatal("cikis wrote no record")
	}
	for i := 0; i < 2; i++ {
		a := w.form(t, path, url.Values{"eylem": {"geri"}})
		if a.status != fiber.StatusOK || !strings.Contains(a.body, "Yeniden abone oldun") {
			t.Fatalf("geri %d = %d %s", i+1, a.status, a.body)
		}
		assertUnsubscribeHeaders(t, "geri", a)
	}
	if len(w.store.out) != 0 || w.store.resubscribed != 1 {
		t.Errorf("after geri: record %v, %d reversals; want none and one", w.store.out, w.store.resubscribed)
	}
	for _, values := range []url.Values{{}, {"eylem": {"islem"}}, {"List-Unsubscribe": {"yes"}}} {
		if a := w.form(t, path, values); a.status != fiber.StatusBadRequest {
			t.Errorf("POST %v = %d, want 400", values, a.status)
		}
	}
}

// A changed or cut token, or a key version the ring does not hold, is 400,
// on the page and to a one-click; it writes nothing. V1's token still works
// with V2 current.
func TestABadTokenIsRefusedAndAnOldVersionStillWorks(t *testing.T) {
	v1 := testRing(t, 1, 1)
	oldToken, err := v1.Seal(unsubscribe.StreamDuyuru, "ayse@example.com")
	if err != nil {
		t.Fatal(err)
	}
	w := newUnsubscribeWorld(t, onConfig(testRing(t, 2, 1, 2)))
	raw, _ := base64.RawURLEncoding.DecodeString(w.token(t, "ayse@example.com"))
	changed := append([]byte(nil), raw...)
	changed[len(changed)-1] ^= 1
	unknown := append([]byte(nil), raw...)
	unknown[0] = 9
	for name, token := range map[string]string{
		"changed": base64.RawURLEncoding.EncodeToString(changed),
		"cut":     base64.RawURLEncoding.EncodeToString(raw[:len(raw)-3]),
		"unknown": base64.RawURLEncoding.EncodeToString(unknown),
		"garbage": "not-a-token",
	} {
		if a := w.call(t, fiber.MethodGet, "/u/"+token, "", nil); a.status != fiber.StatusBadRequest || !strings.Contains(a.body, "Bağlantı geçersiz") {
			t.Errorf("%s GET = %d %s", name, a.status, a.body)
		}
		if a := w.form(t, "/u/"+token, url.Values{"List-Unsubscribe": {"One-Click"}}); a.status != fiber.StatusBadRequest || a.body != "invalid link" {
			t.Errorf("%s one-click = %d %q", name, a.status, a.body)
		}
	}
	if w.store.writes != 0 || w.store.reads != 0 {
		t.Errorf("bad tokens read %d and wrote %d times", w.store.reads, w.store.writes)
	}
	if a := w.form(t, "/u/"+oldToken, url.Values{"List-Unsubscribe": {"One-Click"}}); a.status != fiber.StatusOK {
		t.Errorf("a V1 token with V2 current = %d %q", a.status, a.body)
	}
}

// Off, /u/ is 404; on with an empty ring, 503; a database failure, 503. No
// other method or path under /u reaches the general error handler (which
// logs the path).
func TestTheEndpointIsOffEmptyOrUnavailable(t *testing.T) {
	ring := testRing(t, 1, 1)
	token, _ := ring.Seal(unsubscribe.StreamDuyuru, "ayse@example.com")
	off := newUnsubscribeWorld(t, unsubscribe.Config{Ring: ring})
	if a := off.call(t, fiber.MethodGet, "/u/"+token, "", nil); a.status != fiber.StatusNotFound {
		t.Errorf("off GET = %d", a.status)
	}
	if a := off.form(t, "/u/"+token, url.Values{"List-Unsubscribe": {"One-Click"}}); a.status != fiber.StatusNotFound || off.store.writes != 0 {
		t.Errorf("off one-click = %d, %d writes", a.status, off.store.writes)
	}

	empty := newUnsubscribeWorld(t, onConfig(&unsubscribe.Ring{}))
	if a := empty.call(t, fiber.MethodGet, "/u/"+token, "", nil); a.status != fiber.StatusServiceUnavailable {
		t.Errorf("empty ring GET = %d", a.status)
	}
	if a := empty.form(t, "/u/"+token, url.Values{"List-Unsubscribe": {"One-Click"}}); a.status != fiber.StatusServiceUnavailable || a.body != "unavailable" {
		t.Errorf("empty ring one-click = %d %q", a.status, a.body)
	}

	w := newUnsubscribeWorld(t, onConfig(ring))
	w.store.err = errors.New("database down")
	if a := w.call(t, fiber.MethodGet, "/u/"+token, "", nil); a.status != fiber.StatusServiceUnavailable {
		t.Errorf("database down GET = %d", a.status)
	}
	if a := w.form(t, "/u/"+token, url.Values{"List-Unsubscribe": {"One-Click"}}); a.status != fiber.StatusServiceUnavailable {
		t.Errorf("database down one-click = %d", a.status)
	}
	w.store.err, w.store.panic = nil, true
	if a := w.call(t, fiber.MethodGet, "/u/"+token, "", nil); a.status != fiber.StatusServiceUnavailable {
		t.Errorf("a panic = %d", a.status)
	}
	w.store.panic = false
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{fiber.MethodPut, "/u/" + token, fiber.StatusMethodNotAllowed},
		{fiber.MethodDelete, "/u/" + token, fiber.StatusMethodNotAllowed},
		{fiber.MethodGet, "/u/" + token + "/x", fiber.StatusNotFound},
		{fiber.MethodGet, "/u", fiber.StatusNotFound},
		{fiber.MethodPost, "/u/", fiber.StatusNotFound},
	} {
		a := w.call(t, c.method, c.path, "", nil)
		if a.status != c.status {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, a.status, c.status)
		}
		assertUnsubscribeHeaders(t, c.method+" "+c.path, a)
	}
	for _, logs := range []string{off.logs.String(), empty.logs.String(), w.logs.String()} {
		for _, secret := range []string{token, "ayse", "/u/", "198.51.100.99", "database down"} {
			if strings.Contains(logs, secret) {
				t.Errorf("the log shows %q:\n%s", secret, logs)
			}
		}
	}
	if !strings.Contains(w.logs.String(), `"endpoint":"unsubscribe"`) {
		t.Errorf("no log line per request:\n%s", w.logs.String())
	}
}

// Each client address may GET 120 times and POST 600 times a minute; the
// next minute starts again. Another address has its own count.
func TestTheEndpointLimitsEachClient(t *testing.T) {
	w := newUnsubscribeWorld(t, onConfig(testRing(t, 1, 1)))
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	SetUnsubscribeClock(w.h, func() time.Time { return now })
	path := "/u/" + w.token(t, "ayse@example.com")
	for i := 0; i < unsubscribeGetsPerMinute; i++ {
		if a := w.call(t, fiber.MethodGet, path, "", nil); a.status != fiber.StatusOK {
			t.Fatalf("GET %d = %d", i+1, a.status)
		}
	}
	if a := w.call(t, fiber.MethodGet, path, "", nil); a.status != fiber.StatusTooManyRequests {
		t.Errorf("GET %d = %d, want 429", unsubscribeGetsPerMinute+1, a.status)
	}
	if a := w.form(t, path, url.Values{"List-Unsubscribe": {"One-Click"}}); a.status != fiber.StatusOK {
		t.Errorf("a POST past the GETs' limit = %d: POSTs have their own", a.status)
	}
	limiter := newClientLimiter()
	for i := 0; i < unsubscribePostsPerMinute; i++ {
		if !limiter.allow("a", true, now) {
			t.Fatalf("POST %d refused", i+1)
		}
	}
	if limiter.allow("a", true, now) {
		t.Error("POST 601 allowed")
	}
	if !limiter.allow("b", true, now) {
		t.Error("another client refused")
	}
	if !limiter.allow("a", true, now.Add(time.Minute)) {
		t.Error("the next minute refused")
	}
}

func TestMaskAddress(t *testing.T) {
	for address, want := range map[string]string{
		"mehmet@example.com": "m…@example.com",
		"şule@örnek.com.tr":  "ş…@örnek.com.tr",
		"a@b.co":             "a…@b.co",
	} {
		if got := maskAddress(address); got != want {
			t.Errorf("maskAddress(%q) = %q, want %q", address, got, want)
		}
	}
}
