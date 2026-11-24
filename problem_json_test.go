package main

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

// Every error SkyMail answers is an RFC 9457 problem (ADR-0010; hub sector
// audit 2026-11-24, 02): application/problem+json with type about:blank, the
// status's reason phrase as title, status, detail and the path as instance;
// and, as extension members, the body it answered before — code, message,
// params — unchanged, which skymail-frontend and the other clients read.
func TestErrorsAreProblemDetailsThatKeepTheOldBody(t *testing.T) {
	t.Parallel()
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	app.Post("/v1/mail_approvals/:id/approve", func(fiber.Ctx) error {
		return apperrors.New("mail_approval.state_conflict", "The request is not pending.", fiber.StatusConflict).
			WithParams(map[string]interface{}{"state": "approved"})
	})
	app.Post("/v1/busy", func(fiber.Ctx) error {
		return apperrors.New("mail_approval.busy", "Try again.", fiber.StatusServiceUnavailable).WithRetryAfter(5)
	})
	app.Post("/v1/restore", func(fiber.Ctx) error {
		return &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint"}
	})

	for _, c := range []struct {
		method, path string
		status       int
		title, code  string
		params       map[string]any
		retryAfter   string
	}{
		{fiber.MethodPost, "/v1/mail_approvals/7/approve?x=secret@example.com", fiber.StatusConflict, "Conflict", "mail_approval.state_conflict",
			map[string]any{"state": "approved"}, ""},
		{fiber.MethodPost, "/v1/busy", fiber.StatusServiceUnavailable, "Service Unavailable", "mail_approval.busy", nil, "5"},
		{fiber.MethodPost, "/v1/restore", fiber.StatusConflict, "Conflict", apperrors.ErrConflict.Code, nil, ""},
		// Fiber's own: a route that is not there.
		{fiber.MethodGet, "/v1/nowhere", fiber.StatusNotFound, "Not Found", apperrors.ErrStatusNotFound.Code,
			map[string]any{"original_code": float64(404), "original_message": "Not Found"}, ""},
	} {
		t.Run(c.path, func(t *testing.T) {
			response, err := app.Test(httptest.NewRequest(c.method, c.path, nil))
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != c.status {
				t.Fatalf("status %d, want %d", response.StatusCode, c.status)
			}
			if got := response.Header.Get(fiber.HeaderContentType); got != "application/problem+json" {
				t.Errorf("Content-Type %q, want application/problem+json", got)
			}
			if got := response.Header.Get(fiber.HeaderRetryAfter); got != c.retryAfter {
				t.Errorf("Retry-After %q, want %q", got, c.retryAfter)
			}
			raw, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("body %s: %v", raw, err)
			}
			path := c.path
			if i := len("/v1/mail_approvals/7/approve"); c.status == fiber.StatusConflict && len(path) > i && path[i] == '?' {
				path = path[:i]
			}
			want := map[string]any{
				"type": "about:blank", "title": c.title, "status": float64(c.status),
				"detail": body["message"], "instance": path, "code": c.code,
			}
			for key, value := range want {
				if body[key] != value {
					t.Errorf("%s = %#v, want %#v (body %s)", key, body[key], value, raw)
				}
			}
			if message, _ := body["message"].(string); message == "" {
				t.Errorf("message lost: %s", raw)
			}
			if c.params != nil {
				params, _ := body["params"].(map[string]any)
				for key, value := range c.params {
					if params[key] != value {
						t.Errorf("params.%s = %#v, want %#v", key, params[key], value)
					}
				}
			} else if _, ok := body["params"]; ok {
				t.Errorf("params written without any: %s", raw)
			}
		})
	}
}
