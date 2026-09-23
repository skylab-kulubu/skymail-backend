package keycloak_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
)

func TestListGroupsIncludesNestedChildren(t *testing.T) {
	t.Parallel()
	kc, _ := newNestedGroupServer(t)
	groups, err := kc.ListGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 {
		t.Fatalf("got %d groups", len(groups))
	}
	names := map[string]string{}
	for _, g := range groups {
		names[*g.Name] = *g.Path
	}
	if names["AGC"] != "/UYELER/ARGE/ALGOLAB/AGC" {
		t.Fatalf("names %+v", names)
	}
}

func TestGetGroupMembersIncludesDescendants(t *testing.T) {
	t.Parallel()
	kc, ids := newNestedGroupServer(t)
	members, err := kc.GetGroupMembers(context.Background(), ids.parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members", len(members))
	}
}

type nestedIDs struct {
	parent, child, grandchild string
}

func newNestedGroupServer(t *testing.T) (keycloak.Client, nestedIDs) {
	t.Helper()
	ids := nestedIDs{
		parent:     "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		child:      "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		grandchild: "cccccccc-cccc-cccc-cccc-cccccccccccc",
	}
	parent := map[string]any{"id": ids.parent, "name": "UYELER", "path": "/UYELER"}
	child := map[string]any{"id": ids.child, "name": "ALGOLAB", "path": "/UYELER/ARGE/ALGOLAB"}
	grandchild := map[string]any{"id": ids.grandchild, "name": "AGC", "path": "/UYELER/ARGE/ALGOLAB/AGC"}
	parentMember := map[string]any{"id": "11111111-1111-1111-1111-111111111111", "email": "uyeler@example.com"}
	agcMember := map[string]any{"id": "22222222-2222-2222-2222-222222222222", "email": "agc@example.com"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups":
			_ = json.NewEncoder(w).Encode([]any{parent})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/"+ids.parent+"/children":
			_ = json.NewEncoder(w).Encode([]any{child})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/"+ids.child+"/children":
			_ = json.NewEncoder(w).Encode([]any{grandchild})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/"+ids.grandchild+"/children":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/"+ids.parent:
			_ = json.NewEncoder(w).Encode(parent)
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/"+ids.parent+"/members":
			_ = json.NewEncoder(w).Encode([]any{parentMember})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/"+ids.child+"/members":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/admin/realms/e-skylab/groups/"+ids.grandchild+"/members":
			_ = json.NewEncoder(w).Encode([]any{agcMember})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return keycloak.NewClient(srv.URL+"/realms/e-skylab", "skymail", "secret"), ids
}

// An approver holds the role directly or through a group, a subgroup of one
// included; each is listed once, and a disabled account is not.
func TestClientRoleMembersIncludesGroupsAndSkipsDisabled(t *testing.T) {
	t.Parallel()
	const clientUUID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	const roleGroup = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	const subGroup = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	direct := map[string]any{"id": "11111111-1111-1111-1111-111111111111", "email": "fatih@example.com", "enabled": true}
	both := map[string]any{"id": "22222222-2222-2222-2222-222222222222", "email": "yusuf@example.com", "enabled": true}
	disabled := map[string]any{"id": "33333333-3333-3333-3333-333333333333", "email": "eski@example.com", "enabled": false}
	nested := map[string]any{"id": "44444444-4444-4444-4444-444444444444", "email": "yk@example.com", "enabled": true}

	var clientQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		admin := "/admin/realms/e-skylab"
		role := admin + "/clients/" + clientUUID + "/roles/skymail:mails:approve"
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.URL.Path == admin+"/clients":
			clientQuery = r.URL.Query().Get("clientId")
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": clientUUID, "clientId": "skymail"}})
		case r.URL.Path == role+"/users":
			if r.URL.Query().Get("first") != "0" {
				_ = json.NewEncoder(w).Encode([]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]any{direct, both, disabled})
		case r.URL.Path == role+"/groups":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": roleGroup, "name": "YK", "path": "/YK"}})
		case r.URL.Path == admin+"/groups/"+roleGroup:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": roleGroup, "name": "YK", "path": "/YK"})
		case r.URL.Path == admin+"/groups/"+roleGroup+"/children":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": subGroup, "name": "BASKAN", "path": "/YK/BASKAN"}})
		case r.URL.Path == admin+"/groups/"+subGroup+"/children":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.URL.Path == admin+"/groups/"+roleGroup+"/members":
			_ = json.NewEncoder(w).Encode([]any{both})
		case r.URL.Path == admin+"/groups/"+subGroup+"/members":
			_ = json.NewEncoder(w).Encode([]any{nested})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	kc := keycloak.NewClient(srv.URL+"/realms/e-skylab", "skymail-backend", "secret")

	members, err := kc.ClientRoleMembers(context.Background(), "skymail", "skymail:mails:approve")
	if err != nil {
		t.Fatal(err)
	}
	if clientQuery != "skymail" {
		t.Errorf("client looked up by clientId %q, want skymail", clientQuery)
	}
	var emails []string
	for _, m := range members {
		emails = append(emails, *m.Email)
	}
	if strings.Join(emails, ",") != "fatih@example.com,yusuf@example.com,yk@example.com" {
		t.Fatalf("role members = %v", emails)
	}
}

// A client that does not exist has no role members to give; saying so beats
// an empty list that reads as "nobody holds the role".
func TestClientRoleMembersRefusesAnUnknownClient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/protocol/openid-connect/token"):
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300,"token_type":"Bearer"}`))
		case r.URL.Path == "/admin/realms/e-skylab/clients":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	kc := keycloak.NewClient(srv.URL+"/realms/e-skylab", "skymail-backend", "secret")

	if _, err := kc.ClientRoleMembers(context.Background(), "skymail", "skymail:mails:approve"); err == nil {
		t.Fatal("an unknown client gave role members")
	}
}
