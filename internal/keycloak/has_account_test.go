package keycloak_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
)

// Whether an address is someone's account (the recipient retention run): any
// user, enabled or not, verified or not, whose primary e-mail is the address,
// or whose school or personal address is. In doubt a row is kept.
func TestHasAccountFindsAnyUserWithTheAddress(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var searches []string
	failing := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/users":
			mu.Lock()
			fail := failing
			query := r.URL.Query()
			searches = append(searches, query.Get("email")+"|"+query.Get("q"))
			mu.Unlock()
			if fail {
				http.Error(w, "down", http.StatusServiceUnavailable)
				return
			}
			if query.Get("exact") != "true" || query.Get("max") != "1" {
				t.Errorf("search %v is not exact and bounded", query)
			}
			found := []any{}
			switch {
			case query.Get("email") == "uye@skylab.example",
				query.Get("q") == "schoolEmail:okul@std.example",
				query.Get("q") == "personalEmail:kisisel@example.org":
				found = append(found, map[string]any{"id": ayseID, "enabled": false})
			}
			_ = json.NewEncoder(w).Encode(found)
		default:
			http.Error(w, "down", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(srv.Close)
	finder, ok := keycloak.NewClient(srv.URL+"/realms/e-skylab", "", "skymail", "secret").(keycloak.AccountFinder)
	if !ok {
		t.Fatal("the Keycloak client is not an AccountFinder")
	}
	ctx := context.Background()
	for address, want := range map[string]bool{
		"uye@skylab.example":  true,
		"okul@std.example":    true,
		"OKUL@std.example":    true, // asked as given and lower-cased
		"kisisel@example.org": true,
		"konuk@guest.example": false,
		"   ":                 false,
	} {
		got, err := finder.HasAccount(ctx, address)
		if err != nil || got != want {
			t.Errorf("HasAccount(%q) = %v, %v; want %v", address, got, err, want)
		}
	}
	mu.Lock()
	searches = nil
	failing = true
	mu.Unlock()
	if _, err := finder.HasAccount(ctx, "konuk@guest.example"); err == nil {
		t.Error("a Keycloak that does not answer gave an answer")
	}
}
