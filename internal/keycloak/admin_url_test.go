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

func TestParseAdminURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ raw, want string }{
		{"", ""},
		{"   ", ""},
		{"http://sky-lab-production-keycloak-cfrcp6:8080", "http://sky-lab-production-keycloak-cfrcp6:8080"},
		{"http://keycloak:8080/", "http://keycloak:8080"},
		{"  http://keycloak:8080//  ", "http://keycloak:8080"},
		{"http://keycloak:8080/realms/e-skylab", "http://keycloak:8080"},
		{"http://keycloak:8080/realms/e-skylab/", "http://keycloak:8080"},
		{"https://kc-admin.yildizskylab.com", "https://kc-admin.yildizskylab.com"},
		{"http://keycloak:8080/auth/", "http://keycloak:8080/auth"},
	} {
		got, err := keycloak.ParseAdminURL(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("ParseAdminURL(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{
		"keycloak:8080",
		"sky-lab-production-keycloak-cfrcp6",
		"/keycloak",
		"ftp://keycloak:8080",
		"http://",
		"http://keycloak:8080?x=1",
		"http://keycloak:8080#x",
		"http://operator:secret@keycloak:8080",
		"http://keycloak:8080/admin",
		"http://keycloak:8080/admin/",
		"http://keycloak:8080/admin/realms/e-skylab",
		"http://keycloak:8080/realms/",
		"http://key cloak:8080",
	} {
		got, err := keycloak.ParseAdminURL(raw)
		if err == nil {
			t.Errorf("ParseAdminURL(%q) = %q, want an error", raw, got)
			continue
		}
		// The value may carry credentials; the error never repeats it.
		if strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "KEYCLOAK_ADMIN_URL") {
			t.Errorf("ParseAdminURL(%q) error %q: want it to name the variable and not the value", raw, err)
		}
	}
}

// Without KEYCLOAK_ADMIN_URL SkyMail does what it always did: the service
// account's token and every Admin REST call go to KEYCLOAK_REALM_URL's host.
func TestAdminRESTUsesTheRealmURLWithoutAdminURL(t *testing.T) {
	t.Parallel()
	public := newRecordingKeycloak(t, "")
	internal := newRecordingKeycloak(t, "")

	kc := keycloak.NewClient(public.srv.URL+"/realms/e-skylab", "", "skymail-backend", "secret")
	exerciseAdminREST(t, kc)

	if got := internal.paths(); len(got) != 0 {
		t.Fatalf("the unset admin URL sent requests elsewhere: %v", got)
	}
	public.assertServedEveryFamily(t, "")
}

// With KEYCLOAK_ADMIN_URL every Admin REST call, and the service account's
// token those calls carry, goes to it; KEYCLOAK_REALM_URL (the issuer) gets
// nothing. The realm is still the one KEYCLOAK_REALM_URL names.
func TestAdminRESTAndItsTokenGoToAdminURL(t *testing.T) {
	t.Parallel()
	public := newRecordingKeycloak(t, "")
	internal := newRecordingKeycloak(t, "")

	kc := keycloak.NewClient(public.srv.URL+"/realms/e-skylab", internal.srv.URL+"/", "skymail-backend", "secret")
	exerciseAdminREST(t, kc)

	if got := public.paths(); len(got) != 0 {
		t.Fatalf("KEYCLOAK_REALM_URL got requests although KEYCLOAK_ADMIN_URL is set: %v", got)
	}
	internal.assertServedEveryFamily(t, "")
}

// A Keycloak served under a context path keeps it.
func TestAdminURLKeepsItsContextPath(t *testing.T) {
	t.Parallel()
	public := newRecordingKeycloak(t, "")
	internal := newRecordingKeycloak(t, "/auth")

	kc := keycloak.NewClient(public.srv.URL+"/realms/e-skylab", internal.srv.URL+"/auth//", "skymail-backend", "secret")
	exerciseAdminREST(t, kc)

	if got := public.paths(); len(got) != 0 {
		t.Fatalf("KEYCLOAK_REALM_URL got requests although KEYCLOAK_ADMIN_URL is set: %v", got)
	}
	internal.assertServedEveryFamily(t, "/auth")
}

const (
	recordedGroupID  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	recordedClientID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
)

// exerciseAdminREST calls every method of the client: gocloak's own calls and
// its raw resty requests (children, role groups).
func exerciseAdminREST(t *testing.T, kc keycloak.Client) {
	t.Helper()
	ctx := context.Background()
	if _, err := kc.ListGroups(ctx); err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	if g, err := kc.GetGroup(ctx, recordedGroupID); err != nil || g == nil {
		t.Fatalf("GetGroup: %v, %v", g, err)
	}
	if _, err := kc.GetGroupMembers(ctx, recordedGroupID); err != nil {
		t.Fatalf("GetGroupMembers: %v", err)
	}
	if _, err := kc.ClientRoleMembers(ctx, "skymail", "skymail:mails:approve"); err != nil {
		t.Fatalf("ClientRoleMembers: %v", err)
	}
}

type recordingKeycloak struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
}

// newRecordingKeycloak is a fake Keycloak served under prefix that answers
// the calls exerciseAdminREST makes and records each request as
// "METHOD path".
func newRecordingKeycloak(t *testing.T, prefix string) *recordingKeycloak {
	t.Helper()
	kc := &recordingKeycloak{}
	group := map[string]any{"id": recordedGroupID, "name": "UYELER", "path": "/UYELER"}
	member := map[string]any{"id": "11111111-1111-1111-1111-111111111111", "email": "uye@example.com", "enabled": true}
	kc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kc.mu.Lock()
		kc.requests = append(kc.requests, r.Method+" "+r.URL.Path)
		kc.mu.Unlock()
		path, ok := strings.CutPrefix(r.URL.Path, prefix)
		if !ok {
			http.NotFound(w, r)
			return
		}
		admin := "/admin/realms/e-skylab"
		role := admin + "/clients/" + recordedClientID + "/roles/skymail:mails:approve"
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && path == "/realms/e-skylab/protocol/openid-connect/token":
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.Header.Get("Authorization") != "Bearer tok":
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method != http.MethodGet:
			http.NotFound(w, r)
		case path == admin+"/groups":
			_ = json.NewEncoder(w).Encode([]any{group})
		case path == admin+"/groups/"+recordedGroupID:
			_ = json.NewEncoder(w).Encode(group)
		case path == admin+"/groups/"+recordedGroupID+"/children", path == role+"/users":
			_, _ = w.Write([]byte(`[]`))
		case path == admin+"/groups/"+recordedGroupID+"/members":
			_ = json.NewEncoder(w).Encode([]any{member})
		case path == admin+"/clients":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": recordedClientID, "clientId": "skymail"}})
		case path == role+"/groups":
			_ = json.NewEncoder(w).Encode([]any{group})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kc.srv.Close)
	return kc
}

func (kc *recordingKeycloak) paths() []string {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	return append([]string(nil), kc.requests...)
}

// assertServedEveryFamily checks that the token request and the Admin REST
// calls exerciseAdminREST makes all reached this server under prefix, and
// that nothing else did.
func (kc *recordingKeycloak) assertServedEveryFamily(t *testing.T, prefix string) {
	t.Helper()
	got := kc.paths()
	admin := prefix + "/admin/realms/e-skylab"
	token := prefix + "/realms/e-skylab/protocol/openid-connect/token"
	role := admin + "/clients/" + recordedClientID + "/roles/skymail:mails:approve"
	for _, want := range []string{
		"POST " + token,
		"GET " + admin + "/groups",
		"GET " + admin + "/groups/" + recordedGroupID,
		"GET " + admin + "/groups/" + recordedGroupID + "/children",
		"GET " + admin + "/groups/" + recordedGroupID + "/members",
		"GET " + admin + "/clients",
		"GET " + role + "/users",
		"GET " + role + "/groups",
	} {
		found := false
		for _, request := range got {
			found = found || request == want
		}
		if !found {
			t.Errorf("no %q among %v", want, got)
		}
	}
	for _, request := range got {
		_, path, _ := strings.Cut(request, " ")
		if !strings.HasPrefix(path, admin+"/") && path != token {
			t.Errorf("request outside Admin REST and the token endpoint: %q", request)
		}
	}
}
