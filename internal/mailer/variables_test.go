package mailer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
