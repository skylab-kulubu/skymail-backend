package tracking_test

import (
	"bytes"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// A recipient's token is 16 random bytes, 22 characters in a URL; a link's 8,
// 11 characters. Two are never the same.
func TestTokensAreRandomAndTheirSize(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		rt, err := tracking.NewRecipientToken()
		if err != nil || len(rt) != tracking.RecipientTokenBytes {
			t.Fatalf("recipient token %x, %v", rt, err)
		}
		lt, err := tracking.NewLinkToken()
		if err != nil || len(lt) != tracking.LinkTokenBytes {
			t.Fatalf("link token %x, %v", lt, err)
		}
		for _, token := range [][]byte{rt, lt} {
			encoded := tracking.EncodeToken(token)
			if seen[encoded] {
				t.Fatalf("token %s twice", encoded)
			}
			seen[encoded] = true
		}
		if got := len(tracking.EncodeToken(rt)); got != 22 {
			t.Fatalf("a recipient token is %d characters, want 22", got)
		}
		if got := len(tracking.EncodeToken(lt)); got != 11 {
			t.Fatalf("a link token is %d characters, want 11", got)
		}
	}
}

// A token read from a URL is exactly what was encoded: the right length, the
// URL alphabet, no padding and one spelling per token.
func TestDecodeTokenTakesOnlyItsOwnSpelling(t *testing.T) {
	rt, _ := tracking.NewRecipientToken()
	encoded := tracking.EncodeToken(rt)
	if got, ok := tracking.DecodeToken(encoded, tracking.RecipientTokenBytes); !ok || !bytes.Equal(got, rt) {
		t.Fatalf("decode(%s) = %x, %v; want %x", encoded, got, ok, rt)
	}
	lt, _ := tracking.NewLinkToken()
	if _, ok := tracking.DecodeToken(tracking.EncodeToken(lt), tracking.RecipientTokenBytes); ok {
		t.Error("a link token was read as a recipient's")
	}
	for _, bad := range []string{
		"",
		encoded + "=",
		encoded[:21],
		encoded + "A",
		"++++++++++++++++++++++", // standard alphabet
		"//////////////////////",
		"AAAAAAAAAAAAAAAAAAAAA.",
		"AAAAAAAAAAAAAAAAAAAAAB", // the last character's spare bits set: a second spelling of …AA
		"../../../../etc/passwd",
	} {
		if got, ok := tracking.DecodeToken(bad, tracking.RecipientTokenBytes); ok {
			t.Errorf("decode(%q) = %x, accepted", bad, got)
		}
	}
}
