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
	if !reflect.DeepEqual(NewMasker(raw).Variables(), []string{"link"}) {
		t.Errorf("variables = %v, want [link]", NewMasker(raw).Variables())
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

// A credential the masking cannot hide — here the link's key again, in
// another case and in no shape of a code, in a second variable — is
// ErrCannotMask: nothing kept.
func TestMaskSentRefusesAMailStillHoldingACredential(t *testing.T) {
	tmpl := TemplateParts{
		Subject:   "Bağlantın",
		PlainText: "{{.link}} — {{.Note}}",
		HTML:      `<a href="{{.link}}">aç</a> {{.Note}}`,
	}
	vars := map[string]any{
		"link": "https://x.example/y?key=abcdefgh",
		"Note": "referans ABCDEFGH",
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
	if !reflect.DeepEqual(NewMasker(raw).Variables(), want) {
		t.Errorf("variables = %v, want %v", NewMasker(raw).Variables(), want)
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

// A raw IP address is shown nowhere (the club's rule), and a person's
// whereabouts are masked with it: by name (IpAddress, ClientIP, Location), by
// value (an IPv4 or IPv6 address), inside text, and by the patterns too.
func TestMaskSentMasksIPAddressesAndLocations(t *testing.T) {
	tmpl := TemplateParts{
		Subject:   "Hesabında yeni giriş",
		PlainText: "{{.EventTitle}}: {{.EventDetail}}\nCihaz: {{.DeviceLabel}}\nIP: {{.IpAddress}} ({{.Location}})\nİstemci: {{.ClientIP}}\nSaat: {{.OccurredAt}}",
		HTML:      `<p>{{.EventTitle}}: {{.EventDetail}}</p><p>Cihaz: {{.DeviceLabel}}</p><p title="{{.IpAddress}}">IP: {{.IpAddress}} ({{.Location}})</p><p>{{.ClientIP}}</p><p>{{.OccurredAt}}</p>`,
	}
	vars := map[string]any{
		"EventTitle":  "Yeni giriş",
		"EventDetail": "2001:db8::42 adresinden, ardından 198.51.100.7 adresinden giriş yapıldı",
		"DeviceLabel": "Firefox · macOS",
		"IpAddress":   "203.0.113.42",
		"Location":    "Kadıköy, İstanbul",
		"ClientIP":    "fe80::1",
		"OccurredAt":  "13.10.2026 14:20:05",
	}
	sent, raw := sendOf(t, tmpl, vars)
	masked, err := MaskSent(sent, raw, ayse, []TemplateParts{tmpl})
	if err != nil {
		t.Fatal(err)
	}
	if masked.Method != MaskedByVariables {
		t.Errorf("method = %s, want %s", masked.Method, MaskedByVariables)
	}
	assertNoneOf(t, masked.Rendered, "203.0.113.42", "2001:db8::42", "198.51.100.7", "fe80::1", "Kadıköy")
	for _, open := range []string{"Firefox · macOS", "13.10.2026 14:20:05", "adresinden giriş yapıldı"} {
		if !strings.Contains(masked.PlainText, open) {
			t.Errorf("plain text lost %q: %s", open, masked.PlainText)
		}
	}
	want := []string{"ClientIP", "EventDetail", "IpAddress", "Location"}
	if !reflect.DeepEqual(NewMasker(raw).Variables(), want) {
		t.Errorf("variables = %v, want %v", NewMasker(raw).Variables(), want)
	}

	// No template renders it, no variables: the patterns mask the addresses
	// and keep the time.
	byPatterns, err := MaskSent(sent, []byte(`{}`), ayse, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertNoneOf(t, byPatterns.Rendered, "203.0.113.42", "2001:db8::42", "198.51.100.7", "fe80::1")
	if !strings.Contains(byPatterns.PlainText, "14:20:05") {
		t.Errorf("the time was masked: %s", byPatterns.PlainText)
	}
}

func TestPersonalNameIsAnIPOrAPlace(t *testing.T) {
	for name, want := range map[string]bool{
		"IpAddress": true, "ip": true, "ClientIP": true, "remoteIp": true, "ip_address": true, "Location": true,
		"Konum": true, "LoginLocation": true,
		"Membership": false, "Trip": false, "Description": false, "Relationship": false, "OccurredAt": false,
	} {
		if got := personalName(name); got != want {
			t.Errorf("personalName(%q) = %v, want %v", name, got, want)
		}
	}
}

// The #63 review's attacks (notes/inceleme-arsivi-2026-10-09/
// skymail-pr63-frontend-pr92-masked-content-review.md, S4–S6): each value
// must be gone from the masked mail, by the variables or by the patterns.
func TestMaskSentHidesWhatTheReviewLeaked(t *testing.T) {
	cases := []struct {
		name    string
		tmpl    TemplateParts
		vars    map[string]any
		secrets []string
	}{
		{"a token named verifier in a link the template builds",
			TemplateParts{Subject: "Giriş", PlainText: "https://x.example/v?k={{.verifier}}",
				HTML: `<a href="https://x.example/v?k={{.verifier}}">aç</a> {{.verifier}}`},
			map[string]any{"verifier": "k2j4h6g8f0d1"}, []string{"k2j4h6g8f0d1"}},
		{"the same token under a name that says nothing",
			TemplateParts{Subject: "Giriş", PlainText: "{{.ref}}", HTML: `<a href="/v/{{.ref}}">aç</a> {{.ref}}`},
			map[string]any{"ref": "k2j4h6g8f0d1"}, []string{"k2j4h6g8f0d1"}},
		{"a JSON number of four digits",
			TemplateParts{Subject: "Kod {{.dogrulama}}", PlainText: "{{.dogrulama}}", HTML: `<p>{{.dogrulama}}</p>`},
			map[string]any{"dogrulama": 4829}, []string{"4829"}},
		{"a code in prose",
			TemplateParts{Subject: "{{.Message}}", PlainText: "{{.Message}}", HTML: `<p>{{.Message}}</p>`},
			map[string]any{"Message": "Giriş kodun 483920, 10 dakika geçerli"}, []string{"483920"}},
		{"a link with no scheme",
			TemplateParts{Subject: "Bağlantı", PlainText: "{{.Note}}", HTML: `<p>{{.Note}}</p>`},
			map[string]any{"Note": "Aç: skyl.app/r/x7Gk2pQ9aa?k=Zz12"}, []string{"skyl.app/r/x7Gk2pQ9aa", "x7Gk2pQ9aa", "Zz12"}},
		{"a short mixed-case invite code",
			TemplateParts{Subject: "Davet", PlainText: "{{.invite}} / {{.Ref}}", HTML: `<p>{{.invite}} {{.Ref}}</p>`},
			map[string]any{"invite": "x7Gk2pQ9", "Ref": "p3Q9z7Lm"}, []string{"x7Gk2pQ9", "p3Q9z7Lm"}},
		{"a token with a + that html/template prints as &#43;",
			TemplateParts{Subject: "Bağlantı", PlainText: "{{.link}} {{.Note}}", HTML: `<a href="{{.link}}">aç</a> {{.Note}}`},
			map[string]any{"link": "https://x.example/y?t=ab+cd12", "Note": "referans ab+cd12"}, []string{"ab+cd12"}},
		{"an IP address with a port, after IP:, before a colon",
			TemplateParts{Subject: "Giriş", PlainText: "{{.EventDetail}}", HTML: `<p>{{.EventDetail}}</p>`},
			map[string]any{"EventDetail": "85.105.12.34:51234 · IP:85.105.12.40 · 85.105.12.41: yeni cihaz · IP:2001:db8::1"},
			[]string{"85.105.12.34", "85.105.12.40", "85.105.12.41", "2001:db8::1"}},
	}
	for _, tc := range cases {
		sent, raw := sendOf(t, tc.tmpl, tc.vars)
		for mode, candidates := range map[string][]TemplateParts{"variables": {tc.tmpl}, "patterns": nil} {
			masked, err := MaskSent(sent, raw, ayse, candidates)
			if err != nil {
				// Not kept is safe too; but each of these can be masked.
				t.Errorf("%s (%s): %v", tc.name, mode, err)
				continue
			}
			assertNoneOf(t, masked.Rendered, tc.secrets...)
		}
	}
}

// The patterns mask a code wherever the stored mail holds it: in a data-
// attribute, cut by a space or a no-break space, in a style sheet, in a
// relative link; with the variables gone or known.
func TestMaskSentPatternsReachAttributesStylesAndCutCodes(t *testing.T) {
	sent := Rendered{
		Subject:   "Kodun 483 920",
		PlainText: "Kodun 483 920 — 13.10.2026 14:20",
		HTML: `<html><head><style>.c::after{content:"483920"} .a{color:#FF0000;width:1200px}</style></head><body>` +
			`<p data-code="483920">Kod</p><p>483&nbsp;920</p><a href="/login?token=abcdef">aç</a>` +
			`<p style="color:#123456">13.10.2026 14:20</p></body></html>`,
	}
	for name, vars := range map[string][]byte{"no variables": []byte(`{}`), "variables known": []byte(`{"code":"483920"}`)} {
		masked, err := MaskSent(sent, vars, ayse, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertNoneOf(t, masked.Rendered, "483920", "abcdef")
		for part, text := range map[string]string{"subject": masked.Subject, "plain text": masked.PlainText, "html": masked.HTML} {
			if strings.Contains(joinDigits(text), "483920") {
				t.Errorf("%s: %s holds the code cut apart: %s", name, part, text)
			}
		}
		if !strings.Contains(masked.PlainText, "13.10.2026 14:20") || !strings.Contains(masked.HTML, "13.10.2026 14:20") {
			t.Errorf("%s: the date was masked: %+v", name, masked.Rendered)
		}
		if !strings.Contains(masked.HTML, "#FF0000") || !strings.Contains(masked.HTML, "1200px") {
			t.Errorf("%s: the style sheet's colour or size changed: %s", name, masked.HTML)
		}
	}
}

// A value only the known values catch — lowercase, no digit, no shape — is
// replaced in the stored mail.
func TestMaskSentReplacesTheKnownValuesInTheStoredMail(t *testing.T) {
	tmpl := TemplateParts{Subject: "Kod", PlainText: "Kodun {{.code}}", HTML: `<p title="{{.code}}">Kodun <b>{{.code}}</b></p>`}
	sent, raw := sendOf(t, tmpl, map[string]any{"code": "qwerty"})
	masked, err := MaskSent(sent, raw, ayse, nil)
	if err != nil {
		t.Fatal(err)
	}
	if masked.Method != MaskedByPatterns {
		t.Fatalf("method = %s", masked.Method)
	}
	assertNoneOf(t, masked.Rendered, "qwerty")
}

func TestPrintedFormsHoldWhatHTMLTemplatePrints(t *testing.T) {
	v := "https://x.example/a b?k=ç&t=ab+cd"
	if got, want := urlAttrForm(v), "https://x.example/a%20b?k=%c3%a7&amp;t=ab&#43;cd"; got != want {
		t.Errorf("urlAttrForm = %q, want %q", got, want)
	}
	forms := printedForms("ab+cd&<")
	for _, want := range []string{"ab+cd&<", "ab&#43;cd&amp;&lt;"} {
		found := false
		for _, form := range forms {
			found = found || form == want
		}
		if !found {
			t.Errorf("printedForms lacks %q: %q", want, forms)
		}
	}
}

func TestIsIPTakesAPortAndNotATime(t *testing.T) {
	for v, want := range map[string]bool{
		"85.105.12.34": true, "85.105.12.34:51234": true, "[2001:db8::1]:443": true, "2001:db8::1": true,
		"14:20:05": false, "13.10.2026": false, "1.2.3": false,
	} {
		if got := isIP(v); got != want {
			t.Errorf("isIP(%q) = %v, want %v", v, got, want)
		}
	}
}
