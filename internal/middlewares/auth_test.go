package middlewares

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// userinfoStub stands in for Keycloak's userinfo endpoint. The realm URL the
// middleware is built with is the stub's address, so the middleware reaches the
// stub by the same path it would reach Keycloak.
func userinfoStub(t *testing.T, status int, contentType, body string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/protocol/openid-connect/userinfo" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func authApp(realmURL string) (*fiber.App, *int) {
	reached := 0
	app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
	app.Use(NewAuthMiddleware("skymail", realmURL).Authenticate)
	app.Get("/probe", func(c fiber.Ctx) error {
		reached++
		return c.SendStatus(fiber.StatusNoContent)
	})
	return app, &reached
}

func requestWithToken(t *testing.T, app *fiber.App, token string) *http.Response {
	t.Helper()
	request := httptest.NewRequest(fiber.MethodGet, "/probe", nil)
	if token != "" {
		request.Header.Set(fiber.HeaderAuthorization, "Bearer "+token)
	}
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// The bug this guards: Keycloak answers an expired token with 401 and an empty
// body, and reading the body before the status turned that into a 500 the
// operator reads as a broken server rather than as an ended session.
func TestExpiredTokenIsUnauthorizedNotInternalError(t *testing.T) {
	t.Parallel()

	app, reached := authApp(userinfoStub(t, http.StatusUnauthorized, "text/plain;charset=utf-8", ""))

	response := requestWithToken(t, app, "expired.token.value")

	if response.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
	if *reached != 0 {
		t.Fatalf("handler ran %d times, want 0", *reached)
	}
}

func TestRejectedTokenIsUnauthorizedWhateverTheBodyCarries(t *testing.T) {
	t.Parallel()

	for name, stub := range map[string]string{
		"empty body":    userinfoStub(t, http.StatusUnauthorized, "", ""),
		"json body":     userinfoStub(t, http.StatusUnauthorized, fiber.MIMEApplicationJSON, `{"error":"invalid_token"}`),
		"html body":     userinfoStub(t, http.StatusUnauthorized, fiber.MIMETextHTML, "<html>no</html>"),
		"forbidden 403": userinfoStub(t, http.StatusForbidden, "", ""),
	} {
		t.Run(name, func(t *testing.T) {
			app, _ := authApp(stub)

			if response := requestWithToken(t, app, "rejected.token.value"); response.StatusCode != fiber.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", response.StatusCode)
			}
		})
	}
}

// A broken identity provider is not the caller's fault, so it does not get told
// its token is bad.
func TestIdentityProviderFailureIsServiceUnavailable(t *testing.T) {
	t.Parallel()

	for name, realmURL := range map[string]string{
		"keycloak 500": userinfoStub(t, http.StatusInternalServerError, "", "boom"),
		"keycloak 502": userinfoStub(t, http.StatusBadGateway, "", ""),
		"unreachable":  "http://127.0.0.1:1",
	} {
		t.Run(name, func(t *testing.T) {
			app, reached := authApp(realmURL)

			response := requestWithToken(t, app, "good.token.value")

			if response.StatusCode != fiber.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", response.StatusCode)
			}
			if *reached != 0 {
				t.Fatalf("handler ran %d times, want 0", *reached)
			}
		})
	}
}

// A 2xx that does not carry the claims is genuinely our problem, and stays a 500.
func TestUnreadableUserinfoBodyStaysInternalError(t *testing.T) {
	t.Parallel()

	app, _ := authApp(userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, "not json at all"))

	if response := requestWithToken(t, app, "good.token.value"); response.StatusCode != fiber.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.StatusCode)
	}
}

func TestAcceptedTokenReachesTheHandler(t *testing.T) {
	t.Parallel()

	body := `{"sub":"11111111-1111-4111-8111-111111111111","resource_access":{"skymail":{"roles":["skymail:access"]}}}`
	app, reached := authApp(userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, body))

	response := requestWithToken(t, app, "good.token.value")

	if response.StatusCode != fiber.StatusNoContent {
		t.Fatalf("status = %d, want 204", response.StatusCode)
	}
	if *reached != 1 {
		t.Fatalf("handler ran %d times, want 1", *reached)
	}
}

func TestMissingOrMalformedAuthorizationHeaderIsForbidden(t *testing.T) {
	t.Parallel()

	realmURL := userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, `{"sub":"x"}`)

	app, _ := authApp(realmURL)
	if response := requestWithToken(t, app, ""); response.StatusCode != fiber.StatusForbidden {
		t.Fatalf("missing header: status = %d, want 403", response.StatusCode)
	}

	app, _ = authApp(realmURL)
	request := httptest.NewRequest(fiber.MethodGet, "/probe", nil)
	request.Header.Set(fiber.HeaderAuthorization, "Basic aGVsbG8=")
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusForbidden {
		t.Fatalf("basic auth: status = %d, want 403", response.StatusCode)
	}
}

// A Mail template version records who wrote it by name, and there is no user
// directory to look a subject up in later, so the name is taken from the token
// at the time: the person's name, or the username when the token carries no
// name — a service account, the Template seed's client, has only that.
func TestAuthenticatedNameIsTheTokensNameOrUsername(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		claims string
		want   any
	}{
		"person":          {`"name":"Ada Yılmaz","preferred_username":"ada.yilmaz"`, "Ada Yılmaz"},
		"service account": {`"preferred_username":"service-account-skymail-seed"`, "service-account-skymail-seed"},
		"blank name":      {`"name":"  ","preferred_username":"ada.yilmaz"`, "ada.yilmaz"},
		"neither":         {`"email":"ada@yildizskylab.com"`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"sub":"11111111-1111-4111-8111-111111111111",` + tc.claims +
				`,"resource_access":{"skymail":{"roles":["skymail:access"]}}}`
			var got any
			app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
			app.Use(NewAuthMiddleware("skymail", userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, body)).Authenticate)
			app.Get("/probe", func(c fiber.Ctx) error {
				got = c.Locals("user_name")
				return c.SendStatus(fiber.StatusNoContent)
			})

			if response := requestWithToken(t, app, "good.token.value"); response.StatusCode != fiber.StatusNoContent {
				t.Fatalf("status = %d, want 204", response.StatusCode)
			}
			if got != tc.want {
				t.Fatalf("user_name = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A Mail onayı decision is mailed to whoever submitted the request, also when
// SkyMail itself expires it days later with no token to ask, so the address
// the token carried is kept at submission — only one Keycloak has verified:
// an unverified one is taken as none, and "user_email_unverified" says why.
func TestAuthenticatedEmailIsTheTokensVerifiedEmail(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		claims     string
		want       any
		unverified any
	}{
		"verified":   {`"name":"Ada Yılmaz","email":"ada@yildizskylab.com","email_verified":true`, "ada@yildizskylab.com", nil},
		"unverified": {`"name":"Ada Yılmaz","email":"ada@yildizskylab.com","email_verified":false`, nil, true},
		"unsaid":     {`"name":"Ada Yılmaz","email":"ada@yildizskylab.com"`, nil, true},
		"blank":      {`"name":"Ada Yılmaz","email":" ","email_verified":true`, nil, nil},
		"missing":    {`"preferred_username":"service-account-skymail-seed"`, nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"sub":"11111111-1111-4111-8111-111111111111",` + tc.claims +
				`,"resource_access":{"skymail":{"roles":["skymail:access"]}}}`
			var got, unverified any
			app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
			app.Use(NewAuthMiddleware("skymail", userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, body)).Authenticate)
			app.Get("/probe", func(c fiber.Ctx) error {
				got = c.Locals("user_email")
				unverified = c.Locals("user_email_unverified")
				return c.SendStatus(fiber.StatusNoContent)
			})

			if response := requestWithToken(t, app, "good.token.value"); response.StatusCode != fiber.StatusNoContent {
				t.Fatalf("status = %d, want 204", response.StatusCode)
			}
			if got != tc.want || unverified != tc.unverified {
				t.Fatalf("user_email = %#v, user_email_unverified = %#v; want %#v, %#v", got, unverified, tc.want, tc.unverified)
			}
		})
	}
}
