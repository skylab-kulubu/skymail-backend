package middlewares

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// A person's own routes know the person's own addresses (GET
// /v1/me/email-delivery; hub ticket rabbitmq-olaylar 10): the address
// userinfo gives as email, unless userinfo says Keycloak has not verified it,
// and the school address (school_email) userinfo or, failing that, the
// accepted token carries. Trimmed and lower-cased, each once; nothing else.
func TestAuthenticatePersonKeepsThePersonsOwnAddresses(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		userinfo    string
		tokenClaims map[string]any
		want        []string
	}{
		"verified email":         {`"email":" Ada@YildizSkyLab.com ","email_verified":true`, nil, []string{"ada@yildizskylab.com"}},
		"email, verified unsaid": {`"email":"ada@yildizskylab.com"`, nil, []string{"ada@yildizskylab.com"}},
		"unverified email":       {`"email":"ada@yildizskylab.com","email_verified":false`, nil, nil},
		"blank email":            {`"email":"  ","email_verified":true`, nil, nil},
		"no address":             {``, nil, nil},
		"school from userinfo": {`"email":"ada@example.com","email_verified":true,"school_email":"Ada.Yilmaz@std.yildiz.edu.tr"`, nil,
			[]string{"ada@example.com", "ada.yilmaz@std.yildiz.edu.tr"}},
		"school from the token": {`"email":"ada@example.com","email_verified":true`, map[string]any{"school_email": " ada.yilmaz@std.yildiz.edu.tr"},
			[]string{"ada@example.com", "ada.yilmaz@std.yildiz.edu.tr"}},
		"userinfo's school first": {`"school_email":"ada.yilmaz@std.yildiz.edu.tr"`, map[string]any{"school_email": "other@std.yildiz.edu.tr"},
			[]string{"ada.yilmaz@std.yildiz.edu.tr"}},
		"school kept when email is unverified": {`"email":"ada@example.com","email_verified":false,"school_email":"ada.yilmaz@std.yildiz.edu.tr"`, nil,
			[]string{"ada.yilmaz@std.yildiz.edu.tr"}},
		"the same address once": {`"email":"ada.yilmaz@std.yildiz.edu.tr","email_verified":true,"school_email":"ADA.YILMAZ@std.yildiz.edu.tr"`, nil,
			[]string{"ada.yilmaz@std.yildiz.edu.tr"}},
		"a school claim that is not a string": {`"email":"ada@example.com"`, map[string]any{"school_email": []string{"x@std.yildiz.edu.tr"}},
			[]string{"ada@example.com"}},
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"sub":"` + audienceTestSubject + `"`
			if tc.userinfo != "" {
				body += `,` + tc.userinfo
			}
			body += `}`
			claims := map[string]any{"sub": audienceTestSubject, "azp": "account-center", "aud": []string{"skymail"}}
			for k, v := range tc.tokenClaims {
				claims[k] = v
			}
			var got []string
			reached := false
			app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
			app.Use(NewAuthMiddleware("skymail", userinfoStub(t, http.StatusOK, fiber.MIMEApplicationJSON, body), AudienceOff).AuthenticatePerson)
			app.Get("/probe", func(c fiber.Ctx) error {
				reached = true
				got = PersonAddresses(c)
				return c.SendStatus(fiber.StatusNoContent)
			})

			if response := requestWithToken(t, app, accessToken(t, claims)); response.StatusCode != fiber.StatusNoContent || !reached {
				t.Fatalf("status = %d, want 204", response.StatusCode)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("addresses = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A route that did not run AuthenticatePerson has no addresses.
func TestPersonAddressesWithoutThePersonCheckAreNone(t *testing.T) {
	t.Parallel()
	app := fiber.New(fiber.Config{ErrorHandler: testErrorHandler})
	var got []string
	app.Get("/probe", func(c fiber.Ctx) error {
		c.Locals(personAddressesKey, "not a list")
		got = PersonAddresses(c)
		return c.SendStatus(fiber.StatusNoContent)
	})
	if response := requestWithToken(t, app, ""); response.StatusCode != fiber.StatusNoContent || got != nil {
		t.Fatalf("status %d addresses %#v", response.StatusCode, got)
	}
}
