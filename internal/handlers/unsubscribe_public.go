package handlers

import (
	"bytes"
	"context"
	"errors"
	htmlt "html/template"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/unsubscribe"
)

// SkyMail's own one-click unsubscribe (RFC 8058; ADR 0072 decision 4; hub
// ticket abonelik-cikisi 02; README, "Tek tık çıkış ucu"): the link an
// address without an account leaves the duyuru stream by.
//
//   - GET (and HEAD) shows a page and changes nothing: mail scanners and
//     browsers' previews open the link.
//   - POST with List-Unsubscribe=One-Click (a mail client's one-click,
//     form-encoded or multipart) records the unsubscribe and answers 200 ok.
//   - POST with eylem=cikis (the page's button) does the same and shows a
//     page; eylem=geri resubscribes: the record goes, and its reversal is
//     recorded as the person's, without the address.
//
// Repeating a POST answers as the first. No cookie, no sign-in, no redirect;
// every answer is no-store, no-referrer, never framed, with a CSP without
// scripts, as core's unsubscribe page. Each client address may GET 120 times
// a minute and POST 600 times (a mail provider's one-click POSTs share a few
// addresses), counted in memory only. Nothing logs the path, the token, the
// address or the client's address: one line per request says the method and
// the result.

// The page's field: eylem, not islem, which names a stream (review T6).
const (
	unsubscribeActionField = "eylem"
	unsubscribeActionLeave = "cikis"
	unsubscribeActionBack  = "geri"
)

// The per-client limits of a minute.
const (
	unsubscribeGetsPerMinute  = 120
	unsubscribePostsPerMinute = 600
	// unsubscribeClientsTracked bounds the limiter's memory; past it, new
	// clients share one count.
	unsubscribeClientsTracked = 65536
)

// unsubscribeTimeout bounds a request's database work.
const unsubscribeTimeout = 3 * time.Second

// PublicUnsubscribeStore is what the page reads and writes: never an IP, a
// referrer or a path.
type PublicUnsubscribeStore interface {
	GetUnsubscribe(context.Context, database.GetUnsubscribeParams) (database.GetUnsubscribeRow, error)
	Unsubscribe(ctx context.Context, address, stream string) (bool, error)
	Resubscribe(ctx context.Context, address, stream string) (bool, error)
}

// UnsubscribePublicHandler serves /u/.
type UnsubscribePublicHandler struct {
	db     PublicUnsubscribeStore
	cfg    unsubscribe.Config
	now    func() time.Time
	limits *clientLimiter
}

func NewUnsubscribePublicHandler(db PublicUnsubscribeStore, cfg unsubscribe.Config) *UnsubscribePublicHandler {
	return &UnsubscribePublicHandler{db: db, cfg: cfg, now: time.Now, limits: newClientLimiter()}
}

// SetUnsubscribeClock makes h's limiter read now (tests).
func SetUnsubscribeClock(h *UnsubscribePublicHandler, now func() time.Time) { h.now = now }

// Register owns the whole /u prefix, malformed addresses and other methods
// included: no error reaches the general handler, which logs the path.
func (h *UnsubscribePublicHandler) Register(app *fiber.App) {
	app.Get("/u/:token", h.Page)
	app.Head("/u/:token", h.Page)
	app.Post("/u/:token", h.Act)
	app.Use("/u", h.Other)
}

// unsubscribeHeaders are on every answer: the address carries the token.
func unsubscribeHeaders(c fiber.Ctx) {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set(fiber.HeaderReferrerPolicy, "no-referrer")
	c.Set(fiber.HeaderXFrameOptions, "DENY")
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")
	c.Set("X-Robots-Tag", "noindex")
	c.Set(fiber.HeaderContentSecurityPolicy, "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}

func (h *UnsubscribePublicHandler) finish(c fiber.Ctx, start time.Time, result *string) {
	if recover() != nil {
		*result = "unavailable"
		h.unavailablePage(c)
	}
	// Only these fields: an error can carry the token or the address.
	log.Info().Str("endpoint", "unsubscribe").Str("method", c.Method()).Str("result", *result).
		Dur("duration", time.Since(start)).Msg("mail unsubscribe")
}

// Other answers everything under /u that is not GET, HEAD or POST of one
// token: 405 for another method, 404 for another path.
func (h *UnsubscribePublicHandler) Other(c fiber.Ctx) error {
	result := "not_found"
	defer h.finish(c, time.Now(), &result)
	switch c.Method() {
	case fiber.MethodGet, fiber.MethodHead, fiber.MethodPost:
		h.notFoundPage(c)
	default:
		result = "method_not_allowed"
		c.Set(fiber.HeaderAllow, "GET, HEAD, POST")
		renderUnsubscribePage(c, fiber.StatusMethodNotAllowed, unsubscribePage{Title: "Yöntem desteklenmiyor"})
	}
	return nil
}

// Page is GET /u/{token}: it asks before it acts, or says the address is
// already out and offers to resubscribe.
func (h *UnsubscribePublicHandler) Page(c fiber.Ctx) error {
	result := "invalid"
	defer h.finish(c, time.Now(), &result)
	if !h.cfg.On {
		result = "off"
		h.notFoundPage(c)
		return nil
	}
	if !h.limits.allow(c.IP(), false, h.now()) {
		result = "throttled"
		h.throttledPage(c, false)
		return nil
	}
	subject, err := h.cfg.Ring.Open(c.Params("token"))
	if err != nil {
		result = h.linkErrorPage(c, err, false)
		return nil
	}
	ctx, cancel := context.WithTimeout(c.Context(), unsubscribeTimeout)
	defer cancel()
	_, err = h.db.GetUnsubscribe(ctx, database.GetUnsubscribeParams{Address: subject.Address, Stream: subject.Stream})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		result = "asked"
		h.askPage(c, subject)
	case err != nil:
		result = "unavailable"
		h.unavailablePage(c)
	default:
		result = "already_out"
		h.outPage(c, subject, "Çıkarıldı")
	}
	return nil
}

// Act is POST /u/{token}: a mail client's one-click or a button of the page.
func (h *UnsubscribePublicHandler) Act(c fiber.Ctx) error {
	result := "invalid"
	defer h.finish(c, time.Now(), &result)
	action := c.FormValue(unsubscribeActionField)
	oneClick := action == "" && c.FormValue("List-Unsubscribe") == "One-Click"
	if !h.cfg.On {
		result = "off"
		h.notFoundPage(c)
		return nil
	}
	if !h.limits.allow(c.IP(), true, h.now()) {
		result = "throttled"
		h.throttledPage(c, oneClick)
		return nil
	}
	if !oneClick && action != unsubscribeActionLeave && action != unsubscribeActionBack {
		renderUnsubscribePage(c, fiber.StatusBadRequest, unsubscribePage{
			Title: "İstek anlaşılamadı", Lines: []string{"E-postadaki bağlantıyı yeniden aç."},
		})
		return nil
	}
	subject, err := h.cfg.Ring.Open(c.Params("token"))
	if err != nil {
		result = h.linkErrorPage(c, err, oneClick)
		return nil
	}
	ctx, cancel := context.WithTimeout(c.Context(), unsubscribeTimeout)
	defer cancel()
	if action == unsubscribeActionBack {
		if _, err := h.db.Resubscribe(ctx, subject.Address, subject.Stream); err != nil {
			result = "unavailable"
			h.unavailablePage(c)
			return nil
		}
		result = "resubscribed"
		renderUnsubscribePage(c, fiber.StatusOK, unsubscribePage{
			Title:   "Yeniden abone oldun",
			Lines:   []string{maskAddress(subject.Address) + " yeniden SKY LAB duyuru postası alacak."},
			Links:   h.accountLinks(),
			Choices: []unsubscribeChoice{{Value: unsubscribeActionLeave, Label: "Duyurulardan çık"}},
		})
		return nil
	}
	if _, err := h.db.Unsubscribe(ctx, subject.Address, subject.Stream); err != nil {
		result = "unavailable"
		if oneClick {
			unsubscribeText(c, fiber.StatusServiceUnavailable, "unavailable")
		} else {
			h.unavailablePage(c)
		}
		return nil
	}
	result = "unsubscribed"
	if oneClick {
		// RFC 8058 §3.2: no redirect, no page to act on.
		unsubscribeText(c, fiber.StatusOK, "ok")
		return nil
	}
	h.outPage(c, subject, "Çıkarıldın")
	return nil
}

func (h *UnsubscribePublicHandler) askPage(c fiber.Ctx, subject unsubscribe.Subject) {
	renderUnsubscribePage(c, fiber.StatusOK, unsubscribePage{
		Title: "Duyuru postalarından çık",
		Lines: []string{
			"Bu adres SKY LAB duyuru postalarından çıkarılsın mı?",
			maskAddress(subject.Address),
			"Hesap ve işlem postaları (giriş kodu, başvuru onayı) bundan etkilenmez.",
		},
		Links:   h.accountLinks(),
		Choices: []unsubscribeChoice{{Value: unsubscribeActionLeave, Label: "Çık"}},
	})
}

func (h *UnsubscribePublicHandler) outPage(c fiber.Ctx, subject unsubscribe.Subject, title string) {
	renderUnsubscribePage(c, fiber.StatusOK, unsubscribePage{
		Title:   title,
		Lines:   []string{maskAddress(subject.Address) + " artık SKY LAB duyuru postası almayacak."},
		Links:   h.accountLinks(),
		Choices: []unsubscribeChoice{{Value: unsubscribeActionBack, Label: "Yeniden abone ol"}},
	})
}

func (h *UnsubscribePublicHandler) accountLinks() []unsubscribeLink {
	return []unsubscribeLink{{Label: "Hesabın varsa tercihlerini Hesap Merkezi'nde yönet", URL: h.cfg.AccountURL}}
}

// linkErrorPage answers a token that does not open: 400 (changed, cut, an
// unknown key version), 503 when the ring is empty. It returns the result
// logged.
func (h *UnsubscribePublicHandler) linkErrorPage(c fiber.Ctx, err error, oneClick bool) string {
	if errors.Is(err, unsubscribe.ErrNoKey) {
		if oneClick {
			unsubscribeText(c, fiber.StatusServiceUnavailable, "unavailable")
		} else {
			h.unavailablePage(c)
		}
		return "no_key"
	}
	if oneClick {
		unsubscribeText(c, fiber.StatusBadRequest, "invalid link")
	} else {
		renderUnsubscribePage(c, fiber.StatusBadRequest, unsubscribePage{
			Title: "Bağlantı geçersiz", Lines: []string{"Bu bağlantı geçersiz. E-postadaki bağlantıyı eksiksiz açtığından emin ol."},
		})
	}
	if errors.Is(err, unsubscribe.ErrUnknownVersion) {
		return "unknown_version"
	}
	return "invalid"
}

func (h *UnsubscribePublicHandler) unavailablePage(c fiber.Ctx) {
	c.Set(fiber.HeaderRetryAfter, "60")
	renderUnsubscribePage(c, fiber.StatusServiceUnavailable, unsubscribePage{
		Title: "Şu an yapılamıyor", Lines: []string{"Lütfen biraz sonra yeniden dene."},
	})
}

func (h *UnsubscribePublicHandler) throttledPage(c fiber.Ctx, oneClick bool) {
	c.Set(fiber.HeaderRetryAfter, "60")
	if oneClick {
		unsubscribeText(c, fiber.StatusTooManyRequests, "too many requests")
		return
	}
	renderUnsubscribePage(c, fiber.StatusTooManyRequests, unsubscribePage{
		Title: "Çok fazla istek", Lines: []string{"Bir dakika sonra yeniden dene."},
	})
}

func (h *UnsubscribePublicHandler) notFoundPage(c fiber.Ctx) {
	renderUnsubscribePage(c, fiber.StatusNotFound, unsubscribePage{Title: "Bulunamadı"})
}

// maskAddress shows an address as its first character and its domain:
// a…@example.com.
func maskAddress(address string) string {
	at := strings.LastIndexByte(address, '@')
	if at < 1 {
		return "…"
	}
	first, _ := utf8.DecodeRuneInString(address)
	return string(first) + "…" + address[at:]
}

func unsubscribeText(c fiber.Ctx, status int, text string) {
	unsubscribeHeaders(c)
	c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
	_ = c.Status(status).SendString(text)
}

type unsubscribePage struct {
	Title   string
	Lines   []string
	Links   []unsubscribeLink
	Choices []unsubscribeChoice
}

type unsubscribeLink struct{ Label, URL string }

// unsubscribeChoice is one button of the page's form, which posts to the
// page's own address (the token is in it): eylem=Value.
type unsubscribeChoice struct{ Value, Label string }

var unsubscribePageTemplate = htmlt.Must(htmlt.New("unsubscribe").Parse(`<!doctype html>
<html lang="tr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}} · SKY LAB</title>
<style>
body{margin:0;font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#f4f5f7;color:#1d2433}
main{max-width:32rem;margin:12vh auto;padding:2rem;background:#fff;border-radius:12px;box-shadow:0 1px 4px rgba(0,0,0,.08)}
h1{font-size:1.4rem;margin:0 0 1rem}p{line-height:1.5}
a{color:#1d4ed8}
button{font:inherit;padding:.7rem 1.4rem;border:0;border-radius:8px;background:#1d4ed8;color:#fff;cursor:pointer}
@media (prefers-color-scheme:dark){body{background:#11151c;color:#e6e9ef}main{background:#1b212b}a{color:#9db8ff}}
</style>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
{{range .Lines}}<p>{{.}}</p>
{{end}}{{if .Choices}}<form method="post">
{{range .Choices}}<p><button type="submit" name="eylem" value="{{.Value}}">{{.Label}}</button></p>
{{end}}</form>
{{end}}{{range .Links}}<p><a href="{{.URL}}" rel="noopener noreferrer">{{.Label}}</a></p>
{{end}}<p><small>SKY LAB · Yıldız Teknik Üniversitesi</small></p>
</main>
</body>
</html>
`))

func renderUnsubscribePage(c fiber.Ctx, status int, p unsubscribePage) {
	var out bytes.Buffer
	if err := unsubscribePageTemplate.Execute(&out, p); err != nil {
		unsubscribeText(c, fiber.StatusInternalServerError, "error")
		return
	}
	unsubscribeHeaders(c)
	c.Set(fiber.HeaderContentType, fiber.MIMETextHTMLCharsetUTF8)
	_ = c.Status(status).Send(out.Bytes())
}

// clientLimiter counts each client address's GETs and POSTs in the current
// minute, in memory only. A new minute starts every count again.
type clientLimiter struct {
	mu     sync.Mutex
	minute time.Time
	counts map[string]*[2]int
}

func newClientLimiter() *clientLimiter {
	return &clientLimiter{counts: make(map[string]*[2]int)}
}

// allow counts one request of client (a GET, or a POST) at now and says
// whether it is within the minute's limit.
func (l *clientLimiter) allow(client string, post bool, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if minute := now.Truncate(time.Minute); !minute.Equal(l.minute) {
		l.minute = minute
		clear(l.counts)
	}
	count, ok := l.counts[client]
	if !ok {
		if len(l.counts) >= unsubscribeClientsTracked {
			client = ""
			count, ok = l.counts[client]
		}
		if !ok {
			count = &[2]int{}
			l.counts[client] = count
		}
	}
	kind, limit := 0, unsubscribeGetsPerMinute
	if post {
		kind, limit = 1, unsubscribePostsPerMinute
	}
	if count[kind] >= limit {
		return false
	}
	count[kind]++
	return true
}
