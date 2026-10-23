package mailer

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// The two probe recipients differ in each field's length, first and last
// character, and an e-mail in its local part and domain: what a template
// derives from a field differs between them too (hub review T8). Every
// unsubscribe address starts "https://", a real one too: past it they differ.
func TestProbeRecipientsDifferInEveryDerivedShape(t *testing.T) {
	a, b := probeRecipients[0], probeRecipients[1]
	fields := []struct{ name, a, b string }{
		{"FullName", a.FullName, b.FullName},
		{"Email", a.Email, b.Email},
		{"UnsubscribeURL", strings.TrimPrefix(a.UnsubscribeURL, "https://"), strings.TrimPrefix(b.UnsubscribeURL, "https://")},
	}
	for _, f := range fields {
		if len(f.a) == len(f.b) || utf8.RuneCountInString(f.a) == utf8.RuneCountInString(f.b) {
			t.Errorf("%s: %q and %q are as long", f.name, f.a, f.b)
		}
		if f.a[:1] == f.b[:1] || f.a[len(f.a)-1:] == f.b[len(f.b)-1:] {
			t.Errorf("%s: %q and %q start or end alike", f.name, f.a, f.b)
		}
	}
	localA, domainA, _ := strings.Cut(a.Email, "@")
	localB, domainB, _ := strings.Cut(b.Email, "@")
	if len(localA) == len(localB) || len(domainA) == len(domainB) || domainA == domainB {
		t.Errorf("Email: %q and %q share a local part length or a domain", a.Email, b.Email)
	}
	for _, r := range probeRecipients {
		for _, v := range []string{r.Email, r.UnsubscribeURL} {
			if !strings.Contains(v, ".invalid") && !strings.Contains(v, ".example") {
				t.Errorf("%q is not on a reserved name (RFC 2606)", v)
			}
		}
	}
}

// A link made of a recipient's field, whole or derived, is never common to
// the two probes, so it is never registered for a person's send.
func TestALinkDerivedFromTheRecipientIsNotCommonToTheProbes(t *testing.T) {
	const base = "https://mail.example.test"
	html := `<a href="https://skylab.example/sabit">s</a>` +
		`<a href="https://skylab.example/l?n={{len .Email}}">l</a>` +
		`<a href="https://skylab.example/n?n={{len .FullName}}">n</a>` +
		`<a href="https://skylab.example/f?c={{slice .Email 0 1}}">f</a>` +
		`<a href="https://skylab.example/p?c={{printf "%.3s" .FullName}}">p</a>` +
		`<a href="https://skylab.example/e?c={{eq (len .Email) 15}}">e</a>` +
		`<a href="https://skylab.example/u?n={{len .UnsubscribeUrl}}">u</a>`
	templates, err := parseMailTemplates("s", "t", html)
	if err != nil {
		t.Fatal(err)
	}
	probes := templates.operatorProbes(map[string]interface{}{})
	if len(probes) != len(probeRecipients) {
		t.Fatalf("probes = %d, want %d", len(probes), len(probeRecipients))
	}
	got := tracking.CommonLinks(probes, base)
	if len(got) != 1 || got[0] != "https://skylab.example/sabit" {
		t.Fatalf("common links = %v, want only the link written out", got)
	}
}
