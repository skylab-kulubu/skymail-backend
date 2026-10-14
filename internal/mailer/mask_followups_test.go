package mailer

import (
	"reflect"
	"strings"
	"testing"
)

// maskListing masks sent as the sweep does for a template that lists
// sensitive variables.
func maskListing(t *testing.T, sent Rendered, raw []byte, listed []string, candidates []TemplateParts) MaskedMail {
	t.Helper()
	m := NewMaskerListing(raw, listed)
	m.UseTemplates(candidates)
	masked, err := m.Mail(sent, ayse)
	if err != nil {
		t.Fatal(err)
	}
	return masked
}

// Values no name or shape gives away — a code of letters alone, a pass
// phrase, a code of small letters and one digit, three digits — are masked
// when the template lists their variables, whatever their names (#63 review,
// R3). Both ways: rendered again, and the patterns with the values known.
func TestATemplatesSensitiveVariablesAreMaskedWhateverTheirNames(t *testing.T) {
	tmpl := TemplateParts{
		Subject:   "Girişin {{.numara}}",
		PlainText: "Kodun {{.harfler}}, cümlen {{.ilkAdim}}, anahtarın {{.x}}, numaran {{.numara}}. {{.dakikaCount}} dk.",
		HTML: `<p title="{{.x}}">Kodun <b>{{.harfler}}</b>, cümlen {{.ilkAdim}}, anahtarın {{.x}}, numaran {{.numara}}.</p>` +
			`<p>{{.dakikaCount}} dk.</p>`,
	}
	vars := map[string]any{
		"harfler": "kxqpzmwt", "ilkAdim": "elma-armut-kiraz-nar", "x": "kxq7zmw", "numara": "482",
		"dakikaCount": "10", "firstName": "Ayşe",
	}
	listed := []string{"harfler", "ilkAdim", "x", "numara"}
	sent, raw := sendOf(t, tmpl, vars)

	for mode, candidates := range map[string][]TemplateParts{"variables": {tmpl}, "patterns": nil} {
		masked := maskListing(t, sent, raw, listed, candidates)
		assertNoneOf(t, masked.Rendered, "kxqpzmwt", "elma-armut-kiraz-nar", "kxq7zmw")
		if strings.Contains(masked.PlainText, "482") || strings.Contains(masked.Subject, "482") {
			t.Errorf("%s: the three-digit code is still there: %+v", mode, masked.Rendered)
		}
		if !strings.Contains(masked.PlainText, "10 dk.") {
			t.Errorf("%s: what is not listed changed: %q", mode, masked.PlainText)
		}
	}
	if got := NewMaskerListing(raw, listed).Variables(); !reflect.DeepEqual(got, []string{"harfler", "ilkAdim", "numara", "x"}) {
		t.Errorf("variables = %v", got)
	}
}

// Everything under a listed variable is masked, a number included; a variable
// not listed is still masked by its name and shape, as without a list.
func TestAListedVariableIsMaskedWholeAndTheNamesStillCount(t *testing.T) {
	tmpl := TemplateParts{
		Subject:   "Bilet",
		PlainText: "{{.bilet.no}} {{.bilet.sira}} {{.link}} {{.Etkinlik}}",
		HTML:      `<p>{{.bilet.no}} {{.bilet.sira}}</p><a href="{{.link}}">aç</a> {{.Etkinlik}}`,
	}
	vars := map[string]any{
		"bilet":    map[string]any{"no": "kxqpzmwt", "sira": 482},
		"link":     "https://x.example/y?k=abcdefgh1",
		"Etkinlik": "Yaz Kampı",
	}
	sent, raw := sendOf(t, tmpl, vars)
	masked := maskListing(t, sent, raw, []string{"bilet"}, []TemplateParts{tmpl})
	if masked.Method != MaskedByVariables {
		t.Errorf("method = %s", masked.Method)
	}
	assertNoneOf(t, masked.Rendered, "kxqpzmwt", "abcdefgh1")
	if strings.Contains(masked.PlainText, "482") {
		t.Errorf("the listed number is still there: %q", masked.PlainText)
	}
	if !strings.Contains(masked.PlainText, "Yaz Kampı") {
		t.Errorf("what is not listed changed: %q", masked.PlainText)
	}
	want := []string{"bilet.no", "bilet.sira", "link"}
	if got := NewMaskerListing(raw, []string{"bilet"}).Variables(); !reflect.DeepEqual(got, want) {
		t.Errorf("variables = %v, want %v", got, want)
	}
}

// A template that lists nothing masks as before: by name and shape.
func TestNoListMasksAsBefore(t *testing.T) {
	raw := []byte(`{"code":"482910","Etkinlik":"Yaz Kampı","sira":482}`)
	for _, listed := range [][]string{nil, {}} {
		if got := NewMaskerListing(raw, listed).Variables(); !reflect.DeepEqual(got, []string{"code"}) {
			t.Errorf("listed %v: variables = %v, want [code]", listed, got)
		}
	}
}

// A verification code under a name that ends in verification or doğrulama is
// a credential by its name (#63 review, R3).
func TestVerificationNamesAreSecretNames(t *testing.T) {
	for _, name := range []string{"verification", "emailVerification", "dogrulama", "doğrulama", "passphrase", "loginPassphrase"} {
		if !secretName(name) {
			t.Errorf("secretName(%q) = false", name)
		}
	}
	tmpl := TemplateParts{Subject: "Doğrula", PlainText: "{{.verification}}", HTML: `<p>{{.verification}}</p>`}
	sent, raw := sendOf(t, tmpl, map[string]any{"verification": "kxqpzmwt"})
	for mode, candidates := range map[string][]TemplateParts{"variables": {tmpl}, "patterns": nil} {
		masked, err := MaskSent(sent, raw, ayse, candidates)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		assertNoneOf(t, masked.Rendered, "kxqpzmwt")
	}
}

// In the patterns, an id, a class or a name is masked as text and looked at
// by the last check like any attribute a reader may see (#63 review, R4): a
// template that writes a code into one leaves no code behind, the variables
// gone or not.
func TestThePatternsMaskACodeInAnIdClassOrName(t *testing.T) {
	sent := Rendered{
		Subject:   "Kod",
		PlainText: "Kod",
		HTML: `<html><body><p id="c-483920" class="t-muted k-77a1b2" title="Kod">Kod</p>` +
			`<input name="n-483920" type="hidden"><td class="foot-cell" align="right">SKY LAB</td></body></html>`,
	}
	for name, vars := range map[string][]byte{"no variables": []byte(`{}`), "variables gone": nil} {
		masked, err := MaskSent(sent, vars, ayse, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if masked.Method != MaskedByPatterns {
			t.Errorf("%s: method = %s", name, masked.Method)
		}
		assertNoneOf(t, masked.Rendered, "483920", "k-77a1b2")
		// What is no code stays: the classes the look needs.
		for _, kept := range []string{`t-muted`, `class="foot-cell"`, `align="right"`, `type="hidden"`} {
			if !strings.Contains(masked.HTML, kept) {
				t.Errorf("%s: %s changed: %s", name, kept, masked.HTML)
			}
		}
	}
}

// The last check sees a code in an id, a class or a name.
func TestTheLastCheckSeesACodeInAnIdClassOrName(t *testing.T) {
	for _, html := range []string{`<p id="c-483920">x</p>`, `<p class="a k-77a1b2">x</p>`, `<input name="n-483920">`} {
		if !unexplained(Rendered{HTML: html}, nil) {
			t.Errorf("unexplained(%s) = false", html)
		}
	}
	// Colours and sizes stay out of it.
	if unexplained(Rendered{HTML: `<td bgcolor="#FF0000" width="600" style="color:#123456">x</td>`}, nil) {
		t.Error("a colour or a size is taken for a code")
	}
}
