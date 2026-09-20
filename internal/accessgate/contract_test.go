package accessgate_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
)

func TestMarkerKeyMatchesSharedGoldenVector(t *testing.T) {
	t.Parallel()

	const subject = "11111111-1111-1111-1111-111111111111"
	const wantDigest = "0be50f44b14aa5ca5ae10cfedf7e4986d4d56ddf17b9055e99f3fedcbdb880ff"

	got := accessgate.MarkerKey(subject)
	want := accessgate.MarkerKeyPrefix + wantDigest
	if got != want {
		t.Fatalf("marker key = %q, want %q", got, want)
	}
	if strings.Contains(got, subject) {
		t.Fatal("marker key contains the raw subject")
	}
}

func TestMarkerKeyUsesExactIssuerAndSubjectBytes(t *testing.T) {
	t.Parallel()

	const subject = "abcdefab-1111-1111-1111-111111111111"
	canonical := accessgate.MarkerKey(subject)
	if got := accessgate.MarkerKeyForIssuer(accessgate.Issuer+"/", subject); got == canonical {
		t.Fatal("trailing-slash issuer unexpectedly matched the exact issuer contract")
	}
	if got := accessgate.MarkerKeyForIssuer(accessgate.Issuer, strings.ToUpper(subject)); got == canonical {
		t.Fatal("subject normalization unexpectedly matched the exact-subject contract")
	}
}
