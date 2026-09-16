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
