package middlewares

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
)

const (
	audienceTestSubject = "22222222-2222-4222-8222-222222222222"
	audienceTestEmail   = "ada@yildizskylab.com"
)

// accessToken is a Keycloak-shaped access token with claims. Its signature is
// not real: the userinfo stub stands in for Keycloak, which is what proves a
// token in /v1, so the middleware only reads what Keycloak has accepted.
func accessToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	segment := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return segment(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"}) + "." + segment(claims) + ".c2lnbmF0dXJl"
}

// audienceApp is /v1's authentication in mode, with its log written to logs.
func audienceApp(t *testing.T, mode AudienceMode, userinfoBody string) (*fiber.App, *int, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := zerolog.New(&logs).Level(zerolog.DebugLevel)
	realmURL := userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, userinfoBody)
	reached := 0
	app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
	app.Use(newAuthMiddleware("skymail", realmURL, mode, &logger).Authenticate)
	app.Get("/probe", func(c fiber.Ctx) error {
		reached++
		return c.SendStatus(fiber.StatusNoContent)
	})
	return app, &reached, &logs
}

const skymailUserinfo = `{"sub":"` + audienceTestSubject + `","email":"` + audienceTestEmail + `","email_verified":true,` +
	`"preferred_username":"ada.yilmaz","resource_access":{"skymail":{"roles":["skymail:access"]}}}`

func formsToken(t *testing.T, aud any) string {
	claims := map[string]any{
		"iss": "https://e.yildizskylab.com/realms/e-skylab", "sub": audienceTestSubject, "azp": "forms",
		"email": audienceTestEmail, "resource_access": map[string]any{"skymail": map[string]any{"roles": []string{"skymail:access"}}},
	}
	if aud != nil {
		claims["aud"] = aud
	}
	return accessToken(t, claims)
}

func audienceLines(logs *bytes.Buffer) []map[string]any {
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var line map[string]any
		if json.Unmarshal([]byte(raw), &line) == nil && line["event"] == AudienceMissingEvent {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestAudienceModeFromEnv(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]AudienceMode{
		"":           AudienceOff,
		"  ":         AudienceOff,
		"off":        AudienceOff,
		" log ":      AudienceLog,
		"enforce":    AudienceEnforce,
		"\tenforce ": AudienceEnforce,
	} {
		got, err := AudienceModeFromEnv(func(key string) string {
			if key != "V1_TOKEN_AUDIENCE_MODE" {
				t.Fatalf("read %s", key)
			}
			return raw
		})
		if err != nil || got != want {
			t.Fatalf("%q: mode = %q, err = %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"on", "true", "LOG", "enforced", "log,enforce"} {
		if _, err := AudienceModeFromEnv(func(string) string { return raw }); err == nil ||
			!strings.Contains(err.Error(), "V1_TOKEN_AUDIENCE_MODE") {
			t.Fatalf("%q: err = %v, want one naming V1_TOKEN_AUDIENCE_MODE", raw, err)
		}
	}
}

// off is SkyMail before this check: aud is not read, nothing is written.
func TestAudienceOffAcceptsAndWritesNothing(t *testing.T) {
	t.Parallel()

	for name, token := range map[string]string{
		"no aud":      formsToken(t, nil),
		"other aud":   formsToken(t, []string{"core", "account"}),
		"not a JWT":   "opaque.token.value",
		"skymail aud": formsToken(t, "skymail"),
	} {
		t.Run(name, func(t *testing.T) {
			app, reached, logs := audienceApp(t, AudienceOff, skymailUserinfo)

			if response := requestWithToken(t, app, token); response.StatusCode != fiber.StatusNoContent || *reached != 1 {
				t.Fatalf("status = %d, handler = %d; want 204, 1", response.StatusCode, *reached)
			}
			if logs.Len() != 0 {
				t.Fatalf("off wrote %s", logs.String())
			}
		})
	}
}

// log accepts every token userinfo accepts, and writes one line per accepted
// request whose aud does not name SkyMail: the caller's client and the mode,
// nothing that identifies a person or carries the token.
func TestAudienceLogAcceptsAndCountsTheMissingAudience(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		token  string
		logged bool
		azp    string
	}{
		"no aud":                {formsToken(t, nil), true, "forms"},
		"other aud":             {formsToken(t, []string{"core", "account"}), true, "forms"},
		"aud string, not ours":  {formsToken(t, "core"), true, "forms"},
		"not a JWT":             {"opaque.token.value", true, "none"},
		"no azp":                {accessToken(t, map[string]any{"sub": audienceTestSubject, "aud": "account"}), true, "none"},
		"aud string skymail":    {formsToken(t, "skymail"), false, ""},
		"aud array has skymail": {formsToken(t, []string{"core", "skymail"}), false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			app, reached, logs := audienceApp(t, AudienceLog, skymailUserinfo)

			if response := requestWithToken(t, app, tc.token); response.StatusCode != fiber.StatusNoContent || *reached != 1 {
				t.Fatalf("status = %d, handler = %d; want 204, 1", response.StatusCode, *reached)
			}
			lines := audienceLines(logs)
			if !tc.logged {
				if logs.Len() != 0 {
					t.Fatalf("aud names skymail, yet wrote %s", logs.String())
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("want one %s line, got %s", AudienceMissingEvent, logs.String())
			}
			if lines[0]["azp"] != tc.azp || lines[0]["mode"] != "log" {
				t.Fatalf("line = %v, want azp %q mode log", lines[0], tc.azp)
			}
			for _, secret := range []string{tc.token, audienceTestSubject, audienceTestEmail, "ada.yilmaz", "Bearer"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("log carries %q: %s", secret, logs.String())
				}
			}
			for key := range lines[0] {
				switch key {
				case "level", "service", "event", "azp", "mode", "message":
				default:
					t.Fatalf("log line has field %q: %v", key, lines[0])
				}
			}
		})
	}
}

// A request userinfo or the role check refuses was never accepted, so log
// mode does not count it.
func TestAudienceLogCountsOnlyAcceptedRequests(t *testing.T) {
	t.Parallel()

	app, reached, logs := audienceApp(t, AudienceLog, `{"sub":"`+audienceTestSubject+`","resource_access":{"core":{"roles":["x"]}}}`)

	if response := requestWithToken(t, app, accessToken(t, map[string]any{"azp": "core", "aud": "core"})); response.StatusCode != fiber.StatusForbidden || *reached != 0 {
		t.Fatalf("status = %d, handler = %d; want 403, 0", response.StatusCode, *reached)
	}
	if logs.Len() != 0 {
		t.Fatalf("a refused request was counted: %s", logs.String())
	}
}

// enforce is RFC 9068 §4: a token whose aud does not name SkyMail was not
// issued for SkyMail, and gets 401 before any handler runs.
func TestAudienceEnforceRefusesTheMissingAudience(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		token  string
		status int
		azp    string
	}{
		"no aud":                {formsToken(t, nil), fiber.StatusUnauthorized, "forms"},
		"other aud":             {formsToken(t, []string{"core", "account"}), fiber.StatusUnauthorized, "forms"},
		"not a JWT":             {"opaque.token.value", fiber.StatusUnauthorized, "none"},
		"aud string skymail":    {formsToken(t, "skymail"), fiber.StatusNoContent, ""},
		"aud array has skymail": {formsToken(t, []string{"core", "skymail"}), fiber.StatusNoContent, ""},
	} {
		t.Run(name, func(t *testing.T) {
			app, reached, logs := audienceApp(t, AudienceEnforce, skymailUserinfo)

			response := requestWithToken(t, app, tc.token)
			if response.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, tc.status)
			}
			if tc.status == fiber.StatusNoContent {
				if *reached != 1 || logs.Len() != 0 {
					t.Fatalf("handler = %d, log = %s; want 1 and none", *reached, logs.String())
				}
				return
			}
			if *reached != 0 {
				t.Fatalf("handler ran %d times, want 0", *reached)
			}
			if got := response.Header.Get(fiber.HeaderWWWAuthenticate); got != `Bearer error="invalid_token"` {
				t.Fatalf("WWW-Authenticate = %q", got)
			}
			lines := audienceLines(logs)
			if len(lines) != 1 || lines[0]["azp"] != tc.azp || lines[0]["mode"] != "enforce" {
				t.Fatalf("want one enforce line for %q, got %s", tc.azp, logs.String())
			}
			if strings.Contains(logs.String(), tc.token) || strings.Contains(logs.String(), audienceTestSubject) {
				t.Fatalf("log carries the token or the subject: %s", logs.String())
			}
		})
	}
}

// A token without SkyMail's roles and without its audience is refused as not
// SkyMail's (401), not as lacking a role (403).
func TestAudienceEnforceComesBeforeTheRoleCheck(t *testing.T) {
	t.Parallel()

	app, _, _ := audienceApp(t, AudienceEnforce, `{"sub":"`+audienceTestSubject+`"}`)

	if response := requestWithToken(t, app, accessToken(t, map[string]any{"azp": "core", "aud": "core"})); response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
}

// The audience is the client SkyMail's roles are on (KEYCLOAK_CLIENT_ID), the
// same one the erase route asks for.
func TestAudienceIsTheConfiguredClient(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
	body := `{"sub":"` + audienceTestSubject + `","resource_access":{"skymail-dev":{"roles":["skymail:access"]}}}`
	app.Use(newAuthMiddleware("skymail-dev", userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, body), AudienceEnforce, &logger).Authenticate)
	app.Get("/probe", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })

	if response := requestWithToken(t, app, accessToken(t, map[string]any{"azp": "x", "aud": "skymail-dev"})); response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("aud skymail-dev: status = %d, want 204", response.StatusCode)
	}
	if response := requestWithToken(t, app, accessToken(t, map[string]any{"azp": "x", "aud": "skymail"})); response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("aud skymail: status = %d, want 401", response.StatusCode)
	}
}
