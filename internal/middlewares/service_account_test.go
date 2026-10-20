package middlewares

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// A service account's token (client_id = azp, the sub userinfo accepted)
// passes; a person's token, whatever its roles, does not.
func TestRequireServiceAccountRefusesAPersonsToken(t *testing.T) {
	sub := uuid.NewString()
	for _, mutation := range []string{"none", "person", "client id of another client", "userinfo mismatch", "no sub"} {
		t.Run(mutation, func(t *testing.T) {
			claims := jwt.MapClaims{"sub": sub, "azp": "core", "client_id": "core", "aud": []string{"skymail"}}
			switch mutation {
			case "person":
				claims["azp"] = "account-center"
				delete(claims, "client_id")
			case "client id of another client":
				claims["client_id"] = "forms"
			case "userinfo mismatch":
				claims["sub"] = uuid.NewString()
			case "no sub":
				delete(claims, "sub")
			}
			realm := userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON,
				`{"sub":"`+sub+`","resource_access":{"skymail":{"roles":["skymail:notifications:send"]}}}`)
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("fixture-only"))
			if err != nil {
				t.Fatal(err)
			}
			app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
			app.Use(NewAuthMiddleware("skymail", realm, AudienceOff).Authenticate)
			app.Post("/notifications", RequireServiceAccount(), func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusNoContent) })
			req := httptest.NewRequest(http.MethodPost, "/notifications", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if mutation == "none" {
				if resp.StatusCode != fiber.StatusNoContent {
					t.Fatalf("service account: status=%d, want 204", resp.StatusCode)
				}
				return
			}
			var body struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if resp.StatusCode != fiber.StatusForbidden || body.Code != "auth.service_account_required" {
				t.Fatalf("status=%d code=%q, want 403 auth.service_account_required", resp.StatusCode, body.Code)
			}
		})
	}
}
