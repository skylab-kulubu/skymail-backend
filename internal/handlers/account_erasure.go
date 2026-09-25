package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/erasuretoken"
)

// AccountErasureStore is Postgres for the erase route.
type AccountErasureStore interface {
	FindAccountErasureReceipt(ctx context.Context, requestID uuid.UUID) (*database.AccountErasureReceipt, error)
	EraseAccount(ctx context.Context, erasure database.AccountErasure) (*database.AccountErasureReceipt, error)
}

// ErasureTokenVerifier proves the route's bearer token is core-erasure's,
// for SkyMail, with the erase role.
type ErasureTokenVerifier interface {
	Verify(ctx context.Context, authorization string) (erasuretoken.Caller, error)
}

// AccountErasureHandler serves PUT /internal/v1/account-erasures/{request_id}:
// core's Erasure command for SkyMail (ADR-0051; account erasure spec §2).
// It is idempotent on the request id: a repeat answers the stored 200.
type AccountErasureHandler struct {
	store  AccountErasureStore
	tokens ErasureTokenVerifier
	// The account access gate; nil when ACCOUNT_ACCESS_GATE_MODE is off.
	gate accessgate.Reader
}

func NewAccountErasureHandler(store AccountErasureStore, tokens ErasureTokenVerifier, gate accessgate.Reader) AccountErasureHandler {
	return AccountErasureHandler{store: store, tokens: tokens, gate: gate}
}

// The fixed codes the route answers with (RFC 7807, ADR-0010). None carries a
// value from the request.
const (
	erasureInvalidCommand         = "invalid_erasure_command"
	erasureUnauthorized           = "erasure_unauthorized"
	erasureForbidden              = "erasure_forbidden"
	erasureSubjectNotBlocked      = "subject_not_blocked"
	erasureSubjectBlockUnverified = "subject_block_unverifiable"
	erasureAccessGateUnavailable  = "access_gate_unavailable"
	erasureTokenKeysUnavailable   = "token_keys_unavailable"
	erasureStoreUnavailable       = "erasure_store_unavailable"
)

const (
	erasureMaxBodyBytes   = 4096
	erasureMaxEmails      = 3
	erasureMaxEmailLength = 254
	// Core clamps Retry-After to 30 s .. 15 min (spec §2.4).
	erasureRetryAfter        = 30
	erasureGateOffRetryAfter = 300
)

// Erase runs the command. Logs carry the request id and a fixed code only:
// never the body, the subject, an address or a name (spec §2.7).
func (h AccountErasureHandler) Erase(c fiber.Ctx) error {
	c.Set(fiber.HeaderCacheControl, "no-store")
	requestID, validPath := parseCanonicalishUUID(c.Params("request_id"))
	logID := ""
	if validPath {
		logID = requestID.String()
	}

	caller, err := h.tokens.Verify(c.Context(), c.Get(fiber.HeaderAuthorization))
	switch {
	case errors.Is(err, erasuretoken.ErrKeysUnavailable):
		return erasureUnavailable(c, logID, erasureTokenKeysUnavailable, erasureRetryAfter)
	case errors.Is(err, erasuretoken.ErrForbidden):
		return erasureProblem(c, logID, fiber.StatusForbidden, erasureForbidden, "Erasure forbidden")
	case err != nil:
		c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
		return erasureProblem(c, logID, fiber.StatusUnauthorized, erasureUnauthorized, "Erasure token rejected")
	}
	// The caller's own marker, as on every route: core-erasure's service
	// account is never blocked, a blocked one is refused.
	if h.gate != nil {
		switch h.gate.Check(c.Context(), caller.Subject) {
		case accessgate.Allowed:
		case accessgate.Blocked:
			c.Set(fiber.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
			return erasureProblem(c, logID, fiber.StatusUnauthorized, erasureUnauthorized, "Erasure token rejected")
		default:
			return erasureUnavailable(c, logID, erasureAccessGateUnavailable, erasureRetryAfter)
		}
	}

	command, ok := parseErasureCommand(c.Get(fiber.HeaderContentType), c.Body())
	if !validPath || !ok || command.requestID != requestID {
		return erasureProblem(c, logID, fiber.StatusBadRequest, erasureInvalidCommand, "Invalid erasure command")
	}

	receipt, err := h.store.FindAccountErasureReceipt(c.Context(), requestID)
	if err != nil {
		return erasureStoreFailed(c, logID, err)
	}
	if receipt != nil {
		return erasureCompleted(c, receipt)
	}

	// A compromised caller must not erase someone who never asked: core has
	// blocked the person before it sends the command, and SkyMail reads that
	// block itself. With the gate off, as in sandbox, it cannot.
	if h.gate == nil {
		return erasureUnavailable(c, logID, erasureSubjectBlockUnverified, erasureGateOffRetryAfter)
	}
	switch h.gate.Check(c.Context(), command.subjectID) {
	case accessgate.Blocked:
	case accessgate.Allowed:
		return erasureProblem(c, logID, fiber.StatusConflict, erasureSubjectNotBlocked, "Subject is not blocked")
	default:
		return erasureUnavailable(c, logID, erasureSubjectBlockUnverified, erasureRetryAfter)
	}

	receipt, err = h.store.EraseAccount(c.Context(), database.AccountErasure{
		RequestID: requestID,
		Subject:   command.subjectID,
		Emails:    command.emails,
	})
	if errors.Is(err, database.ErrAccountErasureInProgress) {
		log.Info().Str("request_id", logID).Msg("account erasure waits for mail in flight")
		c.Set(fiber.HeaderRetryAfter, strconv.Itoa(erasureRetryAfter))
		return erasureJSON(c, fiber.StatusAccepted, struct {
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
		}{requestID.String(), "in_progress"})
	}
	if err != nil {
		return erasureStoreFailed(c, logID, err)
	}
	log.Info().Str("request_id", logID).Interface("counts", receipt.Counts).Msg("account erasure completed")
	return erasureCompleted(c, receipt)
}

type erasureCommand struct {
	requestID uuid.UUID
	subjectID string
	emails    []string
}

// parseErasureCommand reads the command body (spec §2.2): exactly
// request_id, subject_id and emails, each once, in a JSON object of at most
// 4 KB. subject_id is a canonical UUID and never Silinmiş kullanıcı; emails
// holds 0..3 addresses, each whole — they are searched for in mail text, so
// anything that is not plainly an address is refused rather than matched.
func parseErasureCommand(contentType string, body []byte) (erasureCommand, bool) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != fiber.MIMEApplicationJSON || len(body) > erasureMaxBodyBytes {
		return erasureCommand{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return erasureCommand{}, false
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		name, isName := token.(string)
		if err != nil || !isName {
			return erasureCommand{}, false
		}
		if _, seen := fields[name]; seen {
			return erasureCommand{}, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return erasureCommand{}, false
		}
		fields[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return erasureCommand{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return erasureCommand{}, false
	}
	if len(fields) != 3 {
		return erasureCommand{}, false
	}

	var command erasureCommand
	requestID, ok := jsonString(fields["request_id"])
	if !ok {
		return erasureCommand{}, false
	}
	if command.requestID, ok = parseCanonicalishUUID(requestID); !ok {
		return erasureCommand{}, false
	}
	// Keycloak subjects are lower-case hyphenated UUIDs, and SkyMail stores
	// them as they come: another spelling would match no row.
	subject, ok := jsonString(fields["subject_id"])
	parsed, err := uuid.Parse(subject)
	if !ok || err != nil || parsed.String() != subject || subject == database.DeletedUserSubject {
		return erasureCommand{}, false
	}
	command.subjectID = subject

	raw := bytes.TrimSpace(fields["emails"])
	if len(raw) == 0 || raw[0] != '[' {
		return erasureCommand{}, false
	}
	var emails []json.RawMessage
	if err := json.Unmarshal(raw, &emails); err != nil || len(emails) > erasureMaxEmails {
		return erasureCommand{}, false
	}
	command.emails = make([]string, 0, len(emails))
	for _, item := range emails {
		email, ok := jsonString(item)
		if !ok || !plainAddress(email) {
			return erasureCommand{}, false
		}
		command.emails = append(command.emails, email)
	}
	return command, true
}

// jsonString is a JSON string value; null and every other type are not.
func jsonString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// plainAddress is an address as core sends one: no spaces or control
// characters, a local part and a domain around the last @, at most 254
// characters.
func plainAddress(email string) bool {
	if email == "" || utf8.RuneCountInString(email) > erasureMaxEmailLength {
		return false
	}
	for _, r := range email {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	at := strings.LastIndex(email, "@")
	return at > 0 && at < len(email)-1
}

// parseCanonicalishUUID takes a UUID in its 36-character hyphenated form, in
// either case — not the braced, URN or bare forms uuid.Parse also accepts.
func parseCanonicalishUUID(s string) (uuid.UUID, bool) {
	if len(s) != 36 {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(s)
	return id, err == nil
}

func erasureCompleted(c fiber.Ctx, receipt *database.AccountErasureReceipt) error {
	counts := receipt.Counts
	if counts == nil {
		counts = map[string]int64{}
	}
	return erasureJSON(c, fiber.StatusOK, struct {
		RequestID   string           `json:"request_id"`
		Status      string           `json:"status"`
		CompletedAt time.Time        `json:"completed_at"`
		Counts      map[string]int64 `json:"counts"`
	}{receipt.RequestID.String(), "completed", receipt.CompletedAt.UTC(), counts})
}

// erasureJSON writes with encoding/json, which orders map keys: the first
// answer and every repeat are the same bytes.
func erasureJSON(c fiber.Ctx, status int, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Status(status).Send(raw)
}

func erasureProblem(c fiber.Ctx, requestID string, status int, code, title string) error {
	log.Warn().Str("request_id", requestID).Str("code", code).Msg("account erasure not done")
	raw, err := json.Marshal(struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Code   string `json:"code"`
	}{"about:blank", title, status, code})
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, "application/problem+json")
	return c.Status(status).Send(raw)
}

func erasureUnavailable(c fiber.Ctx, requestID, code string, retryAfter int) error {
	c.Set(fiber.HeaderRetryAfter, strconv.Itoa(retryAfter))
	return erasureProblem(c, requestID, fiber.StatusServiceUnavailable, code, "Erasure temporarily unavailable")
}

// erasureStoreFailed logs a Postgres failure by its SQLSTATE alone: a
// message can quote a value.
func erasureStoreFailed(c fiber.Ctx, requestID string, err error) error {
	state := "none"
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		state = pgErr.Code
	}
	log.Error().Str("request_id", requestID).Str("sqlstate", state).Msg("account erasure store failed")
	return erasureUnavailable(c, requestID, erasureStoreUnavailable, erasureRetryAfter)
}
