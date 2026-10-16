package middlewares

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

func TestFormsSourceConfigIsDisabledUntilIdentityAndLimitAreConfigured(t *testing.T) {
	for _, values := range []map[string]string{{}, {"SKYMAIL_FORMS_SOURCE_SERVICE_SUB": uuid.NewString()}, {"SKYMAIL_FORMS_SOURCE_MAX_RECIPIENTS": "101"}} {
		cfg, err := FormsSourceConfigFromEnv(func(k string) string { return values[k] }, "realm", "skymail")
		if err != nil || cfg.Enabled() {
			t.Fatalf("incomplete config enabled source creation: enabled=%v error=%v", cfg.Enabled(), err)
		}
	}
	for _, value := range []string{"0", "-1", "many"} {
		_, err := FormsSourceConfigFromEnv(func(k string) string {
			if k == "SKYMAIL_FORMS_SOURCE_MAX_RECIPIENTS" {
				return value
			}
			return ""
		}, "realm", "skymail")
		if err == nil {
			t.Fatal("invalid configured limit accepted")
		}
	}
}

func TestFormsSourceIdentityRequiresTheAcceptedServiceSubjectAndAllClaims(t *testing.T) {
	service := uuid.NewString()
	for _, mutation := range []string{"none", "public client", "human subject", "userinfo mismatch", "wrong issuer", "wrong audience", "missing client id", "disabled"} {
		t.Run(mutation, func(t *testing.T) {
			realm := userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, `{"sub":"`+service+`","resource_access":{"skymail":{"roles":["skymail:access","skymail:mails:send"]}}}`)
			claims := jwt.MapClaims{"sub": service, "azp": "forms", "client_id": "forms", "iss": realm, "aud": []string{"skymail"}}
			cfg := FormsSourceConfig{ServiceSub: service, ClientID: "forms", Issuer: realm, Audience: "skymail", MaxRecipients: 101}
			switch mutation {
			case "public client":
				claims["azp"] = "skyforms"
				claims["client_id"] = "skyforms"
			case "human subject":
				cfg.ServiceSub = uuid.NewString()
			case "userinfo mismatch":
				claims["sub"] = uuid.NewString()
			case "wrong issuer":
				claims["iss"] = "other realm"
			case "wrong audience":
				claims["aud"] = []string{"forms"}
			case "missing client id":
				delete(claims, "client_id")
			case "disabled":
				cfg.ServiceSub = ""
			}
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("fixture-only"))
			if err != nil {
				t.Fatal(err)
			}
			app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
			app.Use(NewAuthMiddleware("skymail", realm, AudienceOff).Authenticate)
			app.Post("/source", RequireFormsSourceService(cfg), func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
			req := httptest.NewRequest(http.MethodPost, "/source", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			want := fiber.StatusForbidden
			if mutation == "none" {
				want = fiber.StatusNoContent
			}
			if mutation == "disabled" {
				want = fiber.StatusServiceUnavailable
			}
			if resp.StatusCode != want {
				t.Fatalf("status=%d, want %d", resp.StatusCode, want)
			}
		})
	}
}
