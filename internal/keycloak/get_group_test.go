package keycloak_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
)

// A group Keycloak does not have (404) is no group, nil and no error: gocloak
// returns its *APIError, and the callers answer "unknown group" (404,
// mail_approval.audience_unavailable) instead of a 500. Any other failure is
// still an error.
func TestGetGroupAnswersAGroupKeycloakDoesNotHaveAsNil(t *testing.T) {
	t.Parallel()
	const missing = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const broken = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.URL.Path == "/admin/realms/e-skylab/groups/"+missing:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Could not find group by id"}`))
		case r.URL.Path == "/admin/realms/e-skylab/groups/"+broken:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	kc := keycloak.NewClient(srv.URL+"/realms/e-skylab", "", "skymail", "secret")

	group, err := kc.GetGroup(context.Background(), missing)
	if err != nil || group != nil {
		t.Fatalf("GetGroup(missing) = %v, %v; want nil, nil", group, err)
	}
	if _, err := kc.GetGroup(context.Background(), broken); err == nil {
		t.Fatal("GetGroup on a Keycloak 500 answered no error")
	}
}
