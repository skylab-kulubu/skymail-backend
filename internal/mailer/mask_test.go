package mailer

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A Keycloak mail as the seed writes one: the link in an href and in the text,
// the plain text with the link, the person's name.
var keycloakVerify = TemplateParts{
	Subject:   "E-posta adresini doğrula",
	PlainText: "Merhaba {{.firstName}},\n\nAdresini doğrulamak için: {{.link}}\nBağlantı {{.linkExpirationMinutes}} dakika geçerli.",
	HTML: `<!doctype html><html><head><title>Doğrula</title></head><body>` +
		`<p>Merhaba {{.firstName}},</p>` +
		`<p><a href="{{.link}}" title="{{.link}}">Adresimi doğrula</a></p>` +
		`<p>Çalışmazsa bu adresi aç: {{.link}}</p>` +
		`<p>Bağlantı {{.linkExpirationMinutes}} dakika geçerli. <a href="https://yildizskylab.com">SKY LAB</a></p>` +
		`</body></html>`,
}

const actionLink = "https://e.yildizskylab.com/realms/e-skylab/login-actions/action-token?key=eyJhbGciOiJIUzI1NiJ9.eyJleHAiOjE3NjA0MDAwMDB9.c2lnbmF0dXJlMTIz&client_id=account&tab_id=AbC123xYz"

var keycloakVars = map[string]any{
	"link": actionLink, "linkExpirationMinutes": "60", "code": "", "codeExpirationMinutes": "",
	"firstName": "Ayşe", "username": "ayse", "realmDisplayName": "SKY LAB", "subjectKey": "emailVerificationSubject",
}

var ayse = RecipientInfo{FullName: "Ayşe Kaya", Email: "ayse@example.com"}

// sendOf renders a send exactly as the queue stores it.
func sendOf(t *testing.T, tmpl TemplateParts, vars map[string]any) (Rendered, []byte) {
	t.Helper()
	raw, err := json.Marshal(vars)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := Render(tmpl.Subject, tmpl.PlainText, tmpl.HTML, raw, ayse)
	if err != nil {
		t.Fatal(err)
	}
	return sent, raw
}

// assertNoneOf fails for every secret, raw or escaped as a renderer prints
// it, found in any part of the masked mail.
func assertNoneOf(t *testing.T, mail Rendered, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		for _, form := range printedForms(secret) {
			for name, part := range map[string]string{"subject": mail.Subject, "plain text": mail.PlainText, "html": mail.HTML} {
				if strings.Contains(strings.ToLower(part), strings.ToLower(form)) {
					t.Errorf("%s holds %q:\n%s", name, form, part)
				}
			}
		}
	}
}

func TestMaskSentRendersAgainWithTheLinkMasked(t *testing.T) {
	sent, raw := sendOf(t, keycloakVerify, keycloakVars)
	if !strings.Contains(sent.HTML, "action-token") {
		t.Fatalf("the test's mail does not hold the link: %s", sent.HTML)
	}

	masked, err := MaskSent(sent, raw, ayse, []TemplateParts{keycloakVerify})
	if err != nil {
		t.Fatal(err)
	}
	if masked.Method != MaskedByVariables {
		t.Errorf("method = %s, want %s", masked.Method, MaskedByVariables)
	}
	if !reflect.DeepEqual(masked.Variables, []string{"link"}) {
		t.Errorf("variables = %v, want [link]", masked.Variables)
	}
	assertNoneOf(t, masked.Rendered, actionLink, "action-token", "eyJhbGciOiJIUzI1NiJ9", "AbC123xYz", "c2lnbmF0dXJlMTIz")
	for name, part := range map[string]string{"plain text": masked.PlainText, "html": masked.HTML} {
		if !strings.Contains(part, Mask) {
			t.Errorf("%s does not show the mask:\n%s", name, part)
		}
		// The rest is open: the greeting, the static text, the expiry.
		for _, open := range []string{"Merhaba Ayşe", "60 dakika geçerli"} {
			if !strings.Contains(part, open) {
				t.Errorf("%s lost %q:\n%s", name, open, part)
			}
		}
	}
	if !strings.Contains(masked.HTML, maskMark) {
		t.Errorf("html does not mark the masked link:\n%s", masked.HTML)
	}
	// The template's own address is not a credential and stays.
	if !strings.Contains(masked.HTML, `href="https://yildizskylab.com"`) {
		t.Errorf("html lost the template's own link:\n%s", masked.HTML)
	}
	if masked.Subject != sent.Subject {
		t.Errorf("subject = %q, want %q", masked.Subject, sent.Subject)
	}
}

// A template may print a code cut up, in a subject, in an attribute: the
// render with the masked code masks it wherever it went.
func TestMaskSentMasksACodeWhereverTheTemplatePutsIt(t *testing.T) {
	tmpl := TemplateParts{
		Subject:   "Kodun: {{.code}}",
		PlainText: "Kodun {{.code}} ({{slice .code 0 3}} {{slice .code 3 6}}), {{.codeExpirationMinutes}} dakika geçerli.",
		HTML:      `<p data-code="{{.code}}">Kodun <b>{{.code}}</b></p><p>{{slice .code 0 3}}&nbsp;{{slice .code 3 6}}</p>`,
	}
	vars := map[string]any{"code": "483920", "codeExpirationMinutes": "10", "firstName": "Ayşe"}
	sent, raw := sendOf(t, tmpl, vars)

	masked, err := MaskSent(sent, raw, ayse, []TemplateParts{tmpl})
	if err != nil {
		t.Fatal(err)
	}
	if masked.Method != MaskedByVariables {
		t.Errorf("method = %s, want %s", masked.Method, MaskedByVariables)
	}
	assertNoneOf(t, masked.Rendered, "483920", "483 920", "483&nbsp;920")
	for _, piece := range []string{"483", "920"} {
		for name, part := range map[string]string{"subject": masked.Subject, "plain text": masked.PlainText, "html": masked.HTML} {
			if strings.Contains(part, piece) {
				t.Errorf("%s holds a piece of the code %q:\n%s", name, piece, part)
			}
		}
	}
	if !strings.Contains(masked.Subject, Mask) || !strings.Contains(masked.PlainText, "10 dakika") {
		t.Errorf("masked mail = %+v", masked.Rendered)
	}
}

// The template changed since the send: an older version renders the mail
// that was sent and is the one used.
func TestMaskSentFindsTheVersionTheSendUsed(t *testing.T) {
	sent, raw := sendOf(t, keycloakVerify, keycloakVars)
	current := keycloakVerify
	current.HTML = strings.Replace(current.HTML, "Adresimi doğrula", "Doğrula", 1)

	masked, err := MaskSent(sent, raw, ayse, []TemplateParts{current, keycloakVerify})
	if err != nil {
		t.Fatal(err)
	}
	if masked.Method != MaskedByVariables || !strings.Contains(masked.HTML, "Adresimi doğrula") {
		t.Errorf("method %s, html:\n%s\nwant the version the send used", masked.Method, masked.HTML)
	}
	assertNoneOf(t, masked.Rendered, actionLink, "action-token")
}

// No template renders the mail that was sent: the stored mail is masked by
// the values and by patterns, the link in its href (escaped &amp;) too.
func TestMaskSentMasksTheStoredMailWhenNoTemplateMatches(t *testing.T) {
	sent, raw := sendOf(t, keycloakVerify, keycloakVars)
	if !strings.Contains(sent.HTML, "&amp;client_id") {
		t.Fatalf("the href is not escaped as the test expects: %s", sent.HTML)
	}
	changed := keycloakVerify
	changed.Subject = "Başka bir konu"

	masked, err := MaskSent(sent, raw, ayse, []TemplateParts{changed})
	if err != nil {
		t.Fatal(err)
	}
	if masked.Method != MaskedByPatterns {
		t.Errorf("method = %s, want %s", masked.Method, MaskedByPatterns)
	}
	assertNoneOf(t, masked.Rendered, actionLink, "action-token", "eyJhbGciOiJIUzI1NiJ9", "AbC123xYz")
	if !strings.Contains(masked.HTML, "Merhaba Ayşe") || !strings.Contains(masked.PlainText, "Merhaba Ayşe") {
		t.Errorf("the rest of the mail is not open: %+v", masked.Rendered)
	}
	if !strings.Contains(masked.HTML, maskMark) {
		t.Errorf("html does not mark the masked link:\n%s", masked.HTML)
	}
}

// Account erasure emptied the variables, and the template changed: no value
// is known, and the patterns alone mask every link, code and token.
func TestMaskSentMasksByPatternsWhenTheVariablesAreGone(t *testing.T) {
	sent := Rendered{
		Subject:   "Giriş kodun 715204",
		PlainText: "Kodun: 715204\nBağlantı: https://place.yildizskylab.com/login?token=Zm9vYmFyYmF6cXV4MTIzNDU2\nBilet: TCK-7A2F9Q\n13.10.2026 14:20",
		HTML: `<!doctype html><html><head><title>Kod 715204</title><style>.a{color:#ff0000}</style></head><body>` +
			`<!-- https://place.yildizskylab.com/login?token=Zm9vYmFyYmF6cXV4MTIzNDU2 -->` +
			`<p>Kodun: <b>715204</b></p>` +
			`<a href="https://place.yildizskylab.com/login?token=Zm9vYmFyYmF6cXV4MTIzNDU2&amp;next=%2F" title="715204">Giriş yap</a>` +
			`<p>Bilet: TCK-7A2F9Q</p><p>Belirteç: Zm9vYmFyYmF6cXV4MTIzNDU2</p><p>13.10.2026 14:20</p></body></html>`,
	}
	masked, err := MaskSent(sent, []byte(`{}`), ayse, nil)
	if err != nil {
		t.Fatal(err)
	}
	if masked.Method != MaskedByPatterns {
		t.Errorf("method = %s, want %s", masked.Method, MaskedByPatterns)
	}
	assertNoneOf(t, masked.Rendered, "715204", "Zm9vYmFyYmF6cXV4MTIzNDU2", "place.yildizskylab.com/login", "TCK-7A2F9Q")
	// A date and a time are not codes; the style sheet keeps its colour.
	if !strings.Contains(masked.PlainText, "13.10.2026 14:20") || !strings.Contains(masked.HTML, "13.10.2026 14:20") {
		t.Errorf("the date was masked: %+v", masked.Rendered)
	}
	if !strings.Contains(masked.HTML, "color:#ff0000") {
		t.Errorf("the style sheet changed:\n%s", masked.HTML)
	}
}

// A credential the masking cannot hide — here the same token, unnamed and
// in another case, in a second variable — is ErrCannotMask: nothing shown.
func TestMaskSentRefusesAMailStillHoldingACredential(t *testing.T) {
	tmpl := TemplateParts{
		Subject:   "Bağlantın",
		PlainText: "{{.link}} — {{.Note}}",
		HTML:      `<a href="{{.link}}">aç</a> {{.Note}}`,
	}
	vars := map[string]any{
		"link": "https://x.example/y?key=SecretValue42",
		"Note": "referans secretvalue42",
	}
	sent, raw := sendOf(t, tmpl, vars)
	if _, err := MaskSent(sent, raw, ayse, []TemplateParts{tmpl}); !errors.Is(err, ErrCannotMask) {
		t.Fatalf("err = %v, want ErrCannotMask", err)
	}
}

// Nested values, numbers and links inside text are masked too.
func TestMaskSentMasksNestedValuesNumbersAndLinksInText(t *testing.T) {
	tmpl := TemplateParts{
		Subject:   "Form",
		PlainText: "{{range .answers}}{{.label}}: {{.value}}\n{{end}}Kod: {{.TicketCode}} PIN: {{.pin}}",
		HTML:      `{{range .answers}}<p>{{.label}}: {{.value}}</p>{{end}}<p>{{.TicketCode}} {{.pin}}</p>`,
	}
	vars := map[string]any{
		"answers": []any{
			map[string]any{"label": "Ad", "value": "Ayşe"},
			map[string]any{"label": "Site", "value": "bak: https://drive.example/file?id=0B1x2y3z4w5v6u7t8s9r0q (özel)"},
		},
		"TicketCode": "QR-88K2",
		"pin":        4821,
	}
	sent, raw := sendOf(t, tmpl, vars)
	masked, err := MaskSent(sent, raw, ayse, []TemplateParts{tmpl})
	if err != nil {
		t.Fatal(err)
	}
	assertNoneOf(t, masked.Rendered, "https://drive.example/file?id=0B1x2y3z4w5v6u7t8s9r0q", "0B1x2y3z4w5v6u7t8s9r0q", "QR-88K2", "4821")
	if !strings.Contains(masked.PlainText, "Ad: Ayşe") || !strings.Contains(masked.PlainText, "bak: "+Mask+" (özel)") {
		t.Errorf("plain text = %q", masked.PlainText)
	}
	want := []string{"TicketCode", "answers[1].value", "pin"}
	if !reflect.DeepEqual(masked.Variables, want) {
		t.Errorf("variables = %v, want %v", masked.Variables, want)
	}
}

func TestSecretNameIsTheLastWordNotAnyWord(t *testing.T) {
	for name, want := range map[string]bool{
		"link": true, "code": true, "CancelUrl": true, "SecureAccountUrl": true, "TicketCode": true, "TicketQrUrl": true,
		"ActionURL": true, "magic_link": true, "otp": true, "accessToken": true, "password": true, "dogrulamaKodu": true,
		"linkExpirationMinutes": false, "codeExpirationMinutes": false, "subjectKey": false, "username": false,
		"firstName": false, "realmDisplayName": false, "EventName": false, "Venue": false,
	} {
		if got := secretName(name); got != want {
			t.Errorf("secretName(%q) = %v, want %v", name, got, want)
		}
	}
}
