package mailer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

// The recipient name cases are one file kept in two repos: this copy, and
// skymail-frontend's src/lib/mail-render/testdata/recipient-name.json, whose
// test pins the same hash. The panel's preview fills the tags as the mailer
// does, so the two must not disagree.
const recipientNameCasesSHA256 = "46ad04b6204b6ffb29353cd715ea4001f7cdfd2b485ab2d74b8b62058d774a54"

type recipientNameCase struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	FullName string `json:"fullName"`
	Expect   string `json:"expect"`
}

func recipientNameCases(t *testing.T) (text, markup []recipientNameCase) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "recipient-name.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256(raw)); sum != recipientNameCasesSHA256 {
		t.Fatalf("testdata/recipient-name.json hashes to %s, not the %s both repos pin: change skymail-frontend's copy (src/lib/mail-render/testdata/recipient-name.json) to match, then both pins", sum, recipientNameCasesSHA256)
	}
	var fixture struct {
		Text   []recipientNameCase `json:"text"`
		Markup []recipientNameCase `json:"markup"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Text) == 0 || len(fixture.Markup) == 0 {
		t.Fatal("the shared fixture holds no cases")
	}
	return fixture.Text, fixture.Markup
}

func TestRecipientNameTagsInText(t *testing.T) {
	text, _ := recipientNameCases(t)
	for _, tc := range text {
		t.Run(tc.Name, func(t *testing.T) {
			if got := fillRecipientName(tc.Value, recipientName(tc.FullName)); got != tc.Expect {
				t.Errorf("fill(%q, %q) = %q, want %q", tc.Value, tc.FullName, got, tc.Expect)
			}
		})
	}
}

func TestRecipientNameTagsInMarkup(t *testing.T) {
	_, markup := recipientNameCases(t)
	for _, tc := range markup {
		t.Run(tc.Name, func(t *testing.T) {
			// The fixture is written as the allow-list writes it, or the panel
			// would be held to markup the server never sees.
			if kept := sanitizeEmailHTML(tc.Value); kept != tc.Value {
				t.Fatalf("the case is not as the allow-list writes it:\n got %s\nwant %s", kept, tc.Value)
			}
			got := fillRecipientNameMarkup(tc.Value, recipientName(tc.FullName))
			if got != tc.Expect {
				t.Errorf("fill(%q, %q) =\n %s\nwant\n %s", tc.Value, tc.FullName, got, tc.Expect)
			}
			if kept := sanitizeEmailHTML(got); kept != got {
				t.Errorf("the filled markup is not what safeHTML keeps:\n%s\n%s", got, kept)
			}
		})
	}
}

// free.basic as the seed writes it, in short: the subject, the preheader and
// the heading print variables, the body is markup.
var freeBasic = TemplateParts{
	Subject:   `{{.Subject}}`,
	PlainText: `{{if .Heading}}{{.Heading}}{{end}} {{safeHTML .BodyHtml}}`,
	HTML:      `<span class="preheader">{{.Subject}}</span>{{if .Heading}}<h1>{{.Heading}}</h1>{{end}}<div>{{safeHTML .BodyHtml}}</div>{{if .CtaUrl}}<a href="{{.CtaUrl}}">{{.CtaLabel}}</a>{{end}}`,
}

func renderFree(t *testing.T, vars map[string]any, recipient RecipientInfo) Rendered {
	t.Helper()
	raw, err := json.Marshal(vars)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(freeBasic.Subject, freeBasic.PlainText, freeBasic.HTML, raw, recipient)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAFreeAnnouncementGreetsEachRecipientByName(t *testing.T) {
	vars := map[string]any{
		"Subject":  "{{.FullName}}, kayıt onayın",
		"Heading":  "{{.FullName}}",
		"BodyHtml": `<p>Sayın {{.FullName}},</p><p>{{.Email}} {{index . "Email"}}</p>`,
		"CtaUrl":   "https://example.com/{{.FullName}}",
		"CtaLabel": "{{.FullName}}",
	}
	out := renderFree(t, vars, RecipientInfo{FullName: "Ayşe <Yılmaz>", Email: "ayse@example.com"})

	if out.Subject != "Ayşe <Yılmaz>, kayıt onayın" {
		t.Errorf("subject = %q", out.Subject)
	}
	for _, want := range []string{
		`<span class="preheader">Ayşe &lt;Yılmaz&gt;, kayıt onayın</span>`,
		`<p>Sayın Ayşe &lt;Yılmaz&gt;,</p>`,
		// Every other tag, and the variables the subject and the body do not
		// print, are as before: text.
		`<p>{{.Email}} {{index . &#34;Email&#34;}}</p>`,
		`<h1>{{.FullName}}</h1>`,
		`>{{.FullName}}</a>`,
	} {
		if !strings.Contains(out.HTML, want) {
			t.Errorf("html lacks %s:\n%s", want, out.HTML)
		}
	}
	if strings.Contains(out.HTML, "<Yılmaz>") || strings.Contains(out.HTML, "ayse@example.com") {
		t.Errorf("html holds the raw name or the address:\n%s", out.HTML)
	}
	if !strings.Contains(out.PlainText, "Sayın Ayşe <Yılmaz>,") || !strings.Contains(out.PlainText, "{{.FullName}} ") {
		t.Errorf("plain text = %q", out.PlainText)
	}
}

func TestAFreeAnnouncementWithoutANameUsesTheFallback(t *testing.T) {
	vars := map[string]any{
		"Subject":  `Sayın {{or .FullName "katılımcı"}}`,
		"BodyHtml": `<p>Sayın {{or .FullName &#34;katılımcı&#34;}}, merhaba {{.FullName}}.</p>`,
	}
	out := renderFree(t, vars, RecipientInfo{FullName: "  ", Email: "a@example.com"})
	if out.Subject != "Sayın katılımcı" {
		t.Errorf("subject = %q", out.Subject)
	}
	if !strings.Contains(out.HTML, "<p>Sayın katılımcı, merhaba .</p>") {
		t.Errorf("html:\n%s", out.HTML)
	}
}

// The name goes in text only: a link's address stays what the operator
// wrote, the same for everyone, and so the same link tracking counts.
func TestTheNameNeverReachesALinkAddress(t *testing.T) {
	vars := map[string]any{
		"Subject":  "Duyuru",
		"BodyHtml": `<p><a href="https://example.com/kayit?ad={{.FullName}}">{{.FullName}}, kaydol</a></p>`,
	}
	ali := renderFree(t, vars, RecipientInfo{FullName: "Ali", Email: "ali@example.com"})
	if !strings.Contains(ali.HTML, ">Ali, kaydol</a>") {
		t.Fatalf("the link's text is not filled:\n%s", ali.HTML)
	}
	links := tracking.Links(ali.HTML, "https://mail.example")
	if len(links) != 1 || strings.Contains(links[0], "Ali") {
		t.Fatalf("links = %v", links)
	}

	parsed, err := parseMailTemplates(freeBasic.Subject, freeBasic.PlainText, freeBasic.HTML)
	if err != nil {
		t.Fatal(err)
	}
	probes := parsed.operatorProbes(vars)
	if len(probes) != len(probeRecipients) {
		t.Fatalf("probes = %d", len(probes))
	}
	if common := tracking.CommonLinks(append([]string{ali.HTML}, probes...), "https://mail.example"); len(common) != 1 || common[0] != links[0] {
		t.Errorf("the link is not common to every recipient: %v", common)
	}
}

// A recipient's own variables are core's words, never an operator's: a tag
// in one stays text.
func TestARecipientsOwnVariableIsNotFilled(t *testing.T) {
	out, err := Render(`{{.CategoryLabel}}`, `x`, `<p>{{.CategoryLabel}}</p>`, []byte(`{}`),
		RecipientInfo{FullName: "Ali", Email: "ali@example.com", Variables: map[string]string{"CategoryLabel": "{{.FullName}}"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != "{{.FullName}}" {
		t.Errorf("subject = %q", out.Subject)
	}
}

// A template's other variables, and a template's own text, are as before.
func TestOnlyTheSubjectsAndTheMarkupsVariablesAreFilled(t *testing.T) {
	fields := nameFieldsOf(`{{.Subject}} {{if .Title}}{{.Title}}{{end}}`,
		`{{safeHTML .BodyHtml}}{{range .Items}}{{safeHTML .Inner}}{{end}}{{define "x"}}{{safeHTML .Defined}}{{end}}{{if .A}}{{safeHTML $.Dollar}}{{end}}`)
	for name, want := range map[string]string{"BodyHtml": "markup", "Dollar": "markup", "Subject": "plain", "Title": "plain",
		"Inner": "", "Defined": "", "Heading": "", "A": ""} {
		got := ""
		if fields.markup[name] {
			got = "markup"
		} else if fields.plain[name] {
			got = "plain"
		}
		if got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// A service mail prints no markup: its subject's variables (a form's title,
// what a respondent wrote) are as before, a tag in them text. The tags are a
// free announcement's (and a news send's), whose body is markup.
func TestATemplateWithoutMarkupFillsNothing(t *testing.T) {
	fields := nameFieldsOf(`{{.FormTitle}} yanıtın`, `<p>{{.FormTitle}}</p><p>{{.Answer}}</p>`)
	if len(fields.markup) != 0 || len(fields.plain) != 0 {
		t.Errorf("fields = %+v, want none", fields)
	}
	out, err := Render(`{{.FormTitle}} yanıtın`, `{{.FormTitle}}`, `<p>{{.FormTitle}}</p>`,
		[]byte(`{"FormTitle":"{{.FullName}} anketi"}`), RecipientInfo{FullName: "Ali", Email: "ali@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Subject != "{{.FullName}} anketi yanıtın" {
		t.Errorf("subject = %q", out.Subject)
	}
}

// Masking renders a sent mail again and trusts it only when it gives back
// exactly the mail that went: the name filled at sending is filled again.
func TestAMaskedFreeAnnouncementRendersAgainWithTheName(t *testing.T) {
	vars := map[string]any{"Subject": "{{.FullName}}", "BodyHtml": `<p>Sayın {{.FullName}}, kodun 482913.</p>`}
	sent, raw := sendOf(t, freeBasic, vars)
	if sent.Subject != ayse.FullName {
		t.Fatalf("subject = %q", sent.Subject)
	}
	masked, err := MaskSent(sent, raw, ayse, []TemplateParts{freeBasic})
	if err != nil {
		t.Fatal(err)
	}
	if masked.Method != MaskedByVariables {
		t.Errorf("method = %s, want %s", masked.Method, MaskedByVariables)
	}
	if !strings.Contains(masked.HTML, "Sayın Ayşe Kaya,") {
		t.Errorf("masked html lost the name:\n%s", masked.HTML)
	}
}
