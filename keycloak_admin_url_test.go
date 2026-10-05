package main

import (
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/config"
)

// KEYCLOAK_ADMIN_URL moves Admin REST only. The Erasure command's token is
// still checked against KEYCLOAK_REALM_URL's issuer and keys: Keycloak names
// its public address in iss whichever address issued a token.
func TestKeycloakAdminURLLeavesTheErasureTokenCheckOnTheRealmURL(t *testing.T) {
	t.Parallel()
	const realm = "https://e.yildizskylab.com/realms/e-skylab"
	for _, admin := range []string{"", "http://sky-lab-production-keycloak-cfrcp6:8080"} {
		got := erasureTokenConfig(config.Config{KeycloakRealmURL: realm, KeycloakAdminURL: admin, KeycloakClientID: "skymail"})
		if got.Issuer != realm || got.JWKSURL != realm+"/protocol/openid-connect/certs" || got.ResourceClient != "skymail" {
			t.Fatalf("admin %q: erasure token config = %+v", admin, got)
		}
	}
}
