// Package accessgate implements the shared, versioned account-access denylist.
// Subjects are hashed with the exact issuer and are never stored directly.
package accessgate

import (
	"crypto/sha256"
	"encoding/hex"
)

const (
	Issuer          = "https://e.yildizskylab.com/realms/e-skylab"
	ContractKey     = "skylab:account-access:v1:contract"
	ContractValue   = `sha256(iss\0sub);marker=1;ttl=none`
	MarkerKeyPrefix = "skylab:account-access:v1:blocked:"
	MarkerValue     = "1"
)

func MarkerKey(subject string) string {
	return MarkerKeyForIssuer(Issuer, subject)
}

func MarkerKeyForIssuer(issuer, subject string) string {
	digest := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return MarkerKeyPrefix + hex.EncodeToString(digest[:])
}
