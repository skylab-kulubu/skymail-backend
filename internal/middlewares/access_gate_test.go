package middlewares

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/rs/zerolog"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
)

type gateReader struct {
	decision accessgate.Decision
	subjects []string
}

func (g *gateReader) Check(_ context.Context, subject string) accessgate.Decision {
	g.subjects = append(g.subjects, subject)
	return g.decision
}

func (*gateReader) Ready(context.Context) error { return nil }

func TestAccountAccessGateAllowsOnlyExplicitAllowedDecision(t *testing.T) {
	t.Parallel()

	reader := &gateReader{decision: accessgate.Allowed}
	handled := 0
	app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
	app.Use(func(c fiber.Ctx) error {
		c.Locals("user_id", "human-subject")
		return c.Next()
	})
	app.Use(AccountAccessGate(reader))
	app.Get("/", func(c fiber.Ctx) error {
		handled++
		return c.SendStatus(fiber.StatusNoContent)
	})

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	if err != nil || response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("response=%v err=%v", response, err)
	}
	if handled != 1 || len(reader.subjects) != 1 || reader.subjects[0] != "human-subject" {
		t.Fatalf("handled=%d subjects=%v", handled, reader.subjects)
	}
}

func TestAccountAccessGateStopsBeforePermissionAndHandler(t *testing.T) {
	t.Parallel()

	const rawSubject = "abcdefab-1111-1111-1111-111111111111"
	for _, test := range []struct {
		name       string
		decision   accessgate.Decision
		status     int
		retryAfter string
	}{
		{name: "blocked", decision: accessgate.Blocked, status: fiber.StatusUnauthorized},
		{name: "unavailable", decision: accessgate.Unavailable, status: fiber.StatusServiceUnavailable, retryAfter: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := &gateReader{decision: test.decision}
			permissionCalls := 0
			handlerCalls := 0
			app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
			app.Use(requestid.New())
			app.Use(func(c fiber.Ctx) error {
				c.Locals("user_id", rawSubject)
				return c.Next()
			})
			var logs bytes.Buffer
			logger := zerolog.New(&logs).Level(zerolog.DebugLevel)
			app.Use(accountAccessGate(reader, &logger))
			app.Use(func(c fiber.Ctx) error {
				permissionCalls++
				return c.Next()
			})
			app.Get("/", func(c fiber.Ctx) error {
				handlerCalls++
				return c.SendStatus(fiber.StatusNoContent)
			})

			response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != test.status || permissionCalls != 0 || handlerCalls != 0 {
				t.Fatalf("status=%d permission=%d handler=%d body=%s", response.StatusCode, permissionCalls, handlerCalls, body)
			}
			if response.Header.Get(fiber.HeaderCacheControl) != "no-store" || response.Header.Get(fiber.HeaderRetryAfter) != test.retryAfter {
				t.Fatalf("headers = %v", response.Header)
			}
			if test.decision == accessgate.Blocked && response.Header.Get(fiber.HeaderWWWAuthenticate) != `Bearer error="invalid_token"` {
				t.Fatalf("WWW-Authenticate = %q", response.Header.Get(fiber.HeaderWWWAuthenticate))
			}
			serialized := string(body) + logs.String()
			if strings.Contains(serialized, rawSubject) || strings.Contains(serialized, accessgate.MarkerKey(rawSubject)) {
				t.Fatal("response or log exposed the raw subject or digest")
			}
			if !strings.Contains(logs.String(), `"decision":"`+string(test.decision)+`"`) || !strings.Contains(logs.String(), `"correlation_id":"`) {
				t.Fatalf("missing structured access decision log: %s", logs.String())
			}
		})
	}
}

func TestAccountAccessGateFailsClosedWhenAuthenticatedSubjectIsMissing(t *testing.T) {
	t.Parallel()

	app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
	app.Use(AccountAccessGate(&gateReader{decision: accessgate.Allowed}))
	app.Get("/", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/", nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.StatusCode)
	}
}

func testErrorHandler(c fiber.Ctx, err error) error {
	appError, ok := err.(*apperrors.AppError)
	if !ok {
		return err
	}
	return c.Status(appError.Status).JSON(appError)
}
