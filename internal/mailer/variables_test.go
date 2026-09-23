package mailer

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The shared cases are one file kept in two repos: this copy, and
// skymail-frontend's src/lib/mail-render/testdata/referenced-variables.json,
// whose test pins the same hash. Changing either copy fails its own repo's
// test until both copies, and both hashes, change together.
const sharedCasesSHA256 = "b54be25614fedd3054e85589e27ea596b97d2023dc37817058a32d4c08d2bbab"

func TestTheSharedCasesAreTheOnesTheFrontendHolds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "referenced-variables.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256(raw)); sum != sharedCasesSHA256 {
		t.Fatalf("testdata/referenced-variables.json hashes to %s, not the %s both repos pin: change skymail-frontend's copy (src/lib/mail-render/testdata/referenced-variables.json) to match, then both pins", sum, sharedCasesSHA256)
	}
}

type referencedVariablesCase struct {
	Name   string   `json:"name"`
	Source string   `json:"source"`
	Expect []string `json:"expect"`
}

// The cases skymail-frontend's render module is held to as well, from the same
// file: the panel shows a template's variables with one reading of a body and
// the server refuses a save with another, so the two must not disagree.
func TestReferencedVariablesFollowTheSharedRule(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "referenced-variables.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []referencedVariablesCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("the shared fixture holds no cases")
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			got, err := ReferencedVariables(tc.Source)
			if err != nil {
				t.Fatalf("ReferencedVariables(%q): %v", tc.Source, err)
			}
			if !reflect.DeepEqual(got, tc.Expect) {
				t.Errorf("ReferencedVariables(%q) = %q, want %q", tc.Source, got, tc.Expect)
			}
		})
	}
}

// A body is read the way the mailer parses it, so a body the mailer could
// not parse — and would fail every send with — is an error here too.
func TestReferencedVariablesOfABodyTheMailerCannotParse(t *testing.T) {
	for name, body := range map[string]string{
		"an unclosed action":   `<a href="{{.link">Git</a>`,
		"an if without an end": `{{if .link}}<a href="{{.link}}">Git</a>`,
		"an unknown function":  `{{shout .link}}`,
		"an end with no block": `{{.link}}{{end}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReferencedVariables(body)
			if err == nil {
				t.Fatalf("ReferencedVariables(%q) = %q, want a parse error", body, got)
			}
			if _, parseErr := parseMailTemplates("", "", body); parseErr == nil || !strings.HasSuffix(parseErr.Error(), err.Error()) {
				t.Errorf("error %q is not the one the mailer meets (%v)", err, parseErr)
			}
		})
	}
}

// The mailer's own functions parse: a body using them is a body it sends.
func TestReferencedVariablesKnowsTheMailersFunctions(t *testing.T) {
	got, err := ReferencedVariables(`{{safeHTML .BodyHtml}} {{add1 .Count}}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"BodyHtml", "Count"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// An empty body references nothing, and is not an error: whether a body may
// be empty is not this function's to say.
func TestReferencedVariablesOfAnEmptyBody(t *testing.T) {
	got, err := ReferencedVariables("")
	if err != nil || len(got) != 0 || got == nil {
		t.Errorf("ReferencedVariables(\"\") = %#v, %v; want an empty list", got, err)
	}
}

// The mailer executes a body with html/template, which leaves an HTML comment —
// and every action inside it — out of the mail it sends. A link kept only in a
// comment reaches no one, which is why a reference there does not count.
func TestTheMailerLeavesHTMLCommentsOutOfTheMail(t *testing.T) {
	parsed, err := parseMailTemplates("Konu", "Metin", `<p>Parolanı sıfırla.</p><!-- <a href="{{.link}}">Sıfırla</a> --><p>{{.firstName}}</p>`)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := parsed.html.Execute(&out, map[string]any{"link": "https://e.example/reset", "firstName": "Ada"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "e.example") || strings.Contains(got, "<!--") || !strings.Contains(got, "Ada") {
		t.Fatalf("executed body = %q, want the comment and the link in it left out, the rest kept", got)
	}
}
