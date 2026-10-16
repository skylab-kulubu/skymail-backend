package handlers

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// PublicTrackingStore never receives an IP, referrer, or raw request path.
type PublicTrackingStore interface {
	ResolveTrackingLink(context.Context, []byte) (database.ResolveTrackingLinkRow, error)
	RecordTracking(context.Context, []byte, int64, string, string) (bool, error)
}

var transparentPixel = []byte{'G', 'I', 'F', '8', '9', 'a', 1, 0, 1, 0, 0x80, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0x21, 0xf9, 4, 1, 0, 0, 0, 0, 0x2c, 0, 0, 0, 0, 1, 0, 1, 0, 0, 2, 2, 0x44, 1, 0, 0x3b}

// TrackingPublicHandler has bounded process-local state: an event bucket,
// eight concurrent database operations and at most 2048 short negative cache
// entries. Redirect resolution continues while recording is throttled.
type TrackingPublicHandler struct {
	db       PublicTrackingStore
	on       bool
	now      func() time.Time
	mu       sync.Mutex
	tokens   float64
	refilled time.Time
	negative map[[32]byte]time.Time
	slots    chan struct{}
}

func NewTrackingPublicHandler(db PublicTrackingStore, cfg tracking.Config) *TrackingPublicHandler {
	return &TrackingPublicHandler{db: db, on: cfg.On(), now: time.Now, tokens: 50, refilled: time.Now(), negative: make(map[[32]byte]time.Time), slots: make(chan struct{}, 8)}
}

// Register owns the entire two route prefixes, including malformed URLs and
// unsupported methods. No error can reach the general path-logging handler.
func (h *TrackingPublicHandler) Register(app *fiber.App) {
	app.Get("/t/o/:rt", h.Open)
	app.Head("/t/o/:rt", h.Open)
	app.Use("/t/o", h.Open)
	app.Get("/t/c/:rt/:lt", h.Click)
	app.Head("/t/c/:rt/:lt", h.Click)
	app.Use("/t/c", h.Click)
}

func trackingHeaders(c fiber.Ctx) {
	c.Set(fiber.HeaderCacheControl, "no-store")
	c.Set("Referrer-Policy", "no-referrer")
	c.Set("X-Robots-Tag", "noindex")
}
func pixel(c fiber.Ctx) {
	trackingHeaders(c)
	c.Set(fiber.HeaderCacheControl, "no-store, no-cache, must-revalidate, private, max-age=0")
	c.Set("Pragma", "no-cache")
	c.Set("Expires", "0")
	c.Set(fiber.HeaderContentType, "image/gif")
	_ = c.Status(fiber.StatusOK).Send(transparentPixel)
}
func trackingPage(c fiber.Ctx, status int, text string) {
	trackingHeaders(c)
	c.Set(fiber.HeaderContentType, fiber.MIMETextPlainCharsetUTF8)
	_ = c.Status(status).SendString(text)
}

func (h *TrackingPublicHandler) finish(c fiber.Ctx, endpoint string, start time.Time, result *string) {
	if recover() != nil {
		*result = "unavailable"
		c.Response().Header.Del(fiber.HeaderLocation)
		if endpoint == "open" {
			pixel(c)
		} else {
			trackingPage(c, fiber.StatusServiceUnavailable, "Bağlantı şu anda açılamıyor.")
		}
	}
	// Only these fields: errors themselves can contain tokens or SQL values.
	log.Info().Str("endpoint", endpoint).Str("result", *result).Dur("duration", time.Since(start)).Msg("mail tracking")
}

func (h *TrackingPublicHandler) Open(c fiber.Ctx) (err error) {
	result := "ignored"
	defer h.finish(c, "open", time.Now(), &result)
	pixel(c)
	if c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead {
		return nil
	}
	token, ok := tracking.DecodeToken(c.Params("rt"), tracking.RecipientTokenBytes)
	if !ok || !h.on {
		return nil
	}
	result = h.record(c, token, 0)
	return nil
}

func (h *TrackingPublicHandler) Click(c fiber.Ctx) (err error) {
	result := "invalid"
	defer h.finish(c, "click", time.Now(), &result)
	trackingHeaders(c)
	if c.Method() != fiber.MethodGet && c.Method() != fiber.MethodHead {
		trackingPage(c, fiber.StatusMethodNotAllowed, "Yöntem desteklenmiyor.")
		return nil
	}
	token, ok := tracking.DecodeToken(c.Params("lt"), tracking.LinkTokenBytes)
	if !ok || h.missing('l', token, false) {
		trackingPage(c, fiber.StatusNotFound, "Bu bağlantı geçerli değil.")
		return nil
	}
	ctx, cancel := context.WithTimeout(c.Context(), 500*time.Millisecond)
	defer cancel()
	if !h.acquire(ctx) {
		result = "unavailable"
		trackingPage(c, fiber.StatusServiceUnavailable, "Bağlantı şu anda açılamıyor.")
		return nil
	}
	link, lookupErr := func() (database.ResolveTrackingLinkRow, error) {
		defer func() { <-h.slots }()
		return h.db.ResolveTrackingLink(ctx, token)
	}()
	if lookupErr != nil {
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			h.missing('l', token, true)
			trackingPage(c, fiber.StatusNotFound, "Bu bağlantı geçerli değil.")
		} else {
			result = "unavailable"
			trackingPage(c, fiber.StatusServiceUnavailable, "Bağlantı şu anda açılamıyor.")
		}
		return nil
	}
	switch {
	case link.UrlClearedReason != nil && *link.UrlClearedReason == "erased":
		result = "erased"
		trackingPage(c, fiber.StatusGone, "Bu bağlantı kaldırıldı.")
		return nil
	case link.Url == nil || !link.CreatedAt.AddDate(1, 0, 0).After(h.now()):
		result = "expired"
		trackingPage(c, fiber.StatusGone, "Bu bağlantının süresi doldu.")
		return nil
	case !tracking.Trackable(*link.Url, ""):
		result = "unavailable"
		trackingPage(c, fiber.StatusServiceUnavailable, "Bağlantı şu anda açılamıyor.")
		return nil
	}
	c.Set(fiber.HeaderLocation, *link.Url)
	_ = c.Status(fiber.StatusFound).Send(nil)
	result = "redirect"
	// HEAD is a probe, never a click. An invalid or another send's recipient
	// still redirects; SQL only writes if recipient and link belong together.
	if c.Method() == fiber.MethodGet && h.on {
		if rt, valid := tracking.DecodeToken(c.Params("rt"), tracking.RecipientTokenBytes); valid {
			result = h.record(c, rt, link.ID)
		}
	}
	return nil
}

func (h *TrackingPublicHandler) acquire(ctx context.Context) bool {
	select {
	case h.slots <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
func (h *TrackingPublicHandler) record(c fiber.Ctx, token []byte, linkID int64) (result string) {
	defer func() {
		if recover() != nil {
			result = "unavailable"
		}
	}()
	if h.missing('r', token, false) {
		return "ignored"
	}
	if !h.allow() {
		return "throttled"
	}
	ctx, cancel := context.WithTimeout(c.Context(), 500*time.Millisecond)
	defer cancel()
	if !h.acquire(ctx) {
		return "unavailable"
	}
	defer func() { <-h.slots }()
	ua := c.Get(fiber.HeaderUserAgent)
	if len(ua) > 1024 {
		ua = ""
	}
	written, err := h.db.RecordTracking(ctx, token, linkID, ua, c.Method())
	if errors.Is(err, pgx.ErrNoRows) {
		h.missing('r', token, true)
		return "ignored"
	}
	if err != nil {
		return "unavailable"
	}
	if written {
		return "recorded"
	}
	return "ignored"
}
func (h *TrackingPublicHandler) allow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.tokens += now.Sub(h.refilled).Seconds() * 50
	if h.tokens > 50 {
		h.tokens = 50
	}
	h.refilled = now
	if h.tokens < 1 {
		return false
	}
	h.tokens--
	return true
}
func (h *TrackingPublicHandler) missing(kind byte, token []byte, add bool) bool {
	key := sha256.Sum256(append([]byte{kind}, token...))
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	if until, ok := h.negative[key]; ok && until.After(now) {
		return true
	}
	if !add {
		return false
	}
	if len(h.negative) >= 2048 {
		for old, until := range h.negative {
			if !until.After(now) {
				delete(h.negative, old)
			}
		}
		if len(h.negative) >= 2048 {
			return false
		}
	}
	h.negative[key] = now.Add(30 * time.Second)
	return false
}
