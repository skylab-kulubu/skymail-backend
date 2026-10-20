package keycloak_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
)

// The account of a person a send names by address (Yusuf, 2026-10-20): only
// Keycloak's exact search on the primary e-mail, one enabled user whose
// address is verified and equals the one asked about case-insensitively.
// Anything else is no account; an address is never guessed.

const (
	ayseID  = "1a000000-0000-4000-8000-000000000001"
	otherID = "1a000000-0000-4000-8000-000000000002"
)

func user(id, email string, enabled, verified bool) map[string]any {
	return map[string]any{"id": id, "email": email, "enabled": enabled, "emailVerified": verified}
}

// accountServer answers the exact e-mail search with users[address]; queries
// records each search's query string.
func accountServer(t *testing.T, users map[string][]any) (keycloak.Client, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var queries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/users":
			mu.Lock()
			queries = append(queries, r.URL.Query())
			mu.Unlock()
			found, ok := users[strings.ToLower(r.URL.Query().Get("email"))]
			if !ok {
				found = []any{}
			}
			_ = json.NewEncoder(w).Encode(found)
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	return keycloak.NewClient(srv.URL+"/realms/e-skylab", "", "skymail", "secret"), &queries
}

func TestTheAccountOfAnAddressIsTheOneVerifiedEnabledExactMatch(t *testing.T) {
	t.Parallel()
	kc, queries := accountServer(t, map[string][]any{
		"ayse@example.com":       {user(ayseID, "ayse@example.com", true, true)},
		"unverified@example.com": {user(ayseID, "unverified@example.com", true, false)},
		"disabled@example.com":   {user(ayseID, "disabled@example.com", false, true)},
		// A search that also gave another address: only the exact one counts.
		"near@example.com": {user(otherID, "near@example.com.tr", true, true)},
		// A realm that allowed duplicate e-mails: never one of them.
		"twice@example.com": {user(ayseID, "twice@example.com", true, true), user(otherID, "TWICE@example.com", true, true)},
	})
	ctx := context.Background()
	for _, c := range []struct {
		address string
		want    string
	}{
		{"ayse@example.com", ayseID},
		{"  AYSE@Example.com ", ayseID},
		{"unverified@example.com", ""},
		{"disabled@example.com", ""},
		{"near@example.com", ""},
		{"twice@example.com", ""},
		{"nobody@example.com", ""},
	} {
		sub, err := kc.VerifiedAccountByEmail(ctx, c.address)
		if err != nil {
			t.Fatalf("%q: %v", c.address, err)
		}
		got := ""
		if sub != nil {
			got = sub.String()
		}
		if got != c.want {
			t.Errorf("%q: account %q, want %q", c.address, got, c.want)
		}
	}
	for _, q := range *queries {
		if q.Get("exact") != "true" || q.Get("max") != "2" || q.Get("email") == "" || q.Get("username") != "" || q.Get("search") != "" || q.Get("q") != "" {
			t.Errorf("the search %v is not the exact primary e-mail search", q)
		}
	}
	if sub, err := kc.VerifiedAccountByEmail(ctx, "   "); sub != nil || err != nil {
		t.Errorf("an empty address = %v, %v; want no account and no search", sub, err)
	}
}

// Keycloak failing is an error, never "no account": the send then tracks no
// one it could not ask about.
func TestAnAccountSearchKeycloakFailsIsAnError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	kc := keycloak.NewClient(srv.URL+"/realms/e-skylab", "", "skymail", "secret")
	if sub, err := kc.VerifiedAccountByEmail(context.Background(), "ayse@example.com"); err == nil || sub != nil {
		t.Fatalf("= %v, %v; want an error", sub, err)
	}
}
