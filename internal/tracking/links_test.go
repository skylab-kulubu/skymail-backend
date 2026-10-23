package tracking_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

const base = "https://api.yildizskylab.com/api/skymail"

// The links a click is counted on are the http and https addresses of the
// mail's <a> tags, in order, each once, as the browser reads them (entities
// decoded, blanks around trimmed). Everything else is left as it is: other
// schemes, fragments, relative paths, addresses with a user, overlong ones,
// unsubscribe and preference links, SkyMail's own /t/ addresses and a link
// the template marks data-skymail-notrack. Nothing outside an <a> is a link.
func TestLinksAreTheMailsTrackableAnchors(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", 2049-len("https://example.com/"))
	body := `<!DOCTYPE html><html><head><link href="https://cdn.example.com/x.css"><style>a{color:red}</style></head>
<body>
<a href="https://skylab.example/etkinlik?a=1&amp;b=2">Etkinlik</a>
<a href=" https://skylab.example/form ">Form</a>
<a href="http://eski.example/">Eski</a>
<a href="https://skylab.example/etkinlik?a=1&b=2">Aynı</a>
<a href="HTTPS://Skylab.example/Buyuk">Büyük</a>
<a href="mailto:yk@example.com">Yaz</a>
<a href="tel:+905551112233">Ara</a>
<a href="#bolum">Bölüm</a>
<a href="/goreli">Göreli</a>
<a href="javascript:alert(1)">Kötü</a>
<a href="https://user:pass@skylab.example/">Kullanıcı</a>
<a href="` + long + `">Uzun</a>
<a href="https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=abc">Çık</a>
<a href="https://my.yildizskylab.com/communication-preferences">Tercihler</a>
<a href="https://liste.example/unsubscribe/42">Abonelik</a>
<a href="` + base + `/t/c/AAAAAAAAAAAAAAAAAAAAAA/AAAAAAAAAAA">Eski takip</a>
<a data-skymail-notrack href="https://skylab.example/gizli">İşaretli</a>
<a>Bağlantısız</a>
<img src="https://cdn.example.com/logo.png">
<a href="https://skylab.example/bosluk icinde">Boşluklu</a>
</body></html>`
	got := tracking.Links(body, base)
	want := []string{
		"https://skylab.example/etkinlik?a=1&b=2",
		"https://skylab.example/form",
		"http://eski.example/",
		"HTTPS://Skylab.example/Buyuk",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Links = %q\nwant %q", got, want)
	}
}

// A link that differs between recipients — one a variable made personal — is
// not counted: only the links every recipient's mail has, in the order the
// first mail has them.
func TestCommonLinksAreTheOnesEveryRecipientHas(t *testing.T) {
	mails := []string{
		`<body><a href="https://skylab.example/a">A</a><a href="https://skylab.example/u?e=ayse%40example.com">Sen</a><a href="https://skylab.example/b">B</a></body>`,
		`<body><a href="https://skylab.example/b">B</a><a href="https://skylab.example/u?e=mehmet%40example.com">Sen</a><a href="https://skylab.example/a">A</a></body>`,
		`<body><a href="https://skylab.example/a">A</a><a href="https://skylab.example/b">B</a><a href="https://skylab.example/c">C</a></body>`,
	}
	got := tracking.CommonLinks(mails, base)
	want := []string{"https://skylab.example/a", "https://skylab.example/b"}
	if !slices.Equal(got, want) {
		t.Fatalf("CommonLinks = %q, want %q", got, want)
	}
	if got := tracking.CommonLinks(nil, base); len(got) != 0 {
		t.Fatalf("CommonLinks of no mail = %q", got)
	}
}

// A send counts clicks on at most MaxLinksPerSend links, the first ones.
func TestCommonLinksStopAtTheLimit(t *testing.T) {
	var body strings.Builder
	body.WriteString("<body>")
	for i := 0; i < tracking.MaxLinksPerSend+20; i++ {
		body.WriteString(`<a href="https://skylab.example/` + strings.Repeat("x", i+1) + `">x</a>`)
	}
	got := tracking.CommonLinks([]string{body.String()}, base)
	if len(got) != tracking.MaxLinksPerSend || got[0] != "https://skylab.example/x" {
		t.Fatalf("%d links, first %q; want the first %d", len(got), got[0], tracking.MaxLinksPerSend)
	}
}

// A service's mail counts clicks only on the links its template writes out
// as they are (Yusuf, 2026-10-21): an href a template action is part of — a
// password reset, an e-mail verification or a login link, any address with
// a variable — is never a literal link, wherever the action sits in it, and
// neither is one marked data-skymail-notrack or one Trackable refuses. A
// link inside an HTML comment is no link.
func TestLiteralLinksAreTheOnesTheTemplateWritesOut(t *testing.T) {
	template := `<!DOCTYPE html><html><body>
<p>Merhaba {{.FullName}},</p>
<a href="https://yildizskylab.com/etkinlikler?a=1&amp;b=2">Etkinlikler</a>
<a href="{{.link}}">Parolanı sıfırla</a>
<a href="https://my.yildizskylab.com/reset?token={{.token}}">Sıfırla</a>
<a href="https://my.yildizskylab.com/{{.path}}/onay">Onayla</a>
<a href={{.loginUrl}}>Giriş</a>
<a href="https://my.yildizskylab.com/verify?k={{/* yorum */}}x">Yorumlu</a>
<a href="https://yildizskylab.com/gizli" data-skymail-notrack>İşaretli</a>
<a href="mailto:info@yildizskylab.com">Yaz</a>
<a href="https://my.yildizskylab.com/communication-preferences">Tercihler</a>
<!-- <a href="https://yildizskylab.com/yorumda">Yorumda</a> -->
{{if .event}}<a title="{{.event}}" href=" https://yildizskylab.com/takvim ">Takvim</a>{{end}}
</body></html>`
	got := tracking.LiteralLinks(template, base)
	want := []string{"https://yildizskylab.com/etkinlikler?a=1&b=2", "https://yildizskylab.com/takvim"}
	if !slices.Equal(got, want) {
		t.Fatalf("LiteralLinks = %q\nwant %q", got, want)
	}
}

// The links of a service's mail a click is counted on are those every mail
// has and the template writes out; a reset link in a single mail — the only
// mail of its send, so CommonLinks alone would count it — is not.
func TestCommonLiteralLinksLeaveOutWhatAVariableMade(t *testing.T) {
	template := `<body><a href="https://yildizskylab.com/">Site</a><a href="{{.link}}">Sıfırla</a><a href="https://yildizskylab.com/yardim">Yardım</a></body>`
	mail := `<body><a href="https://yildizskylab.com/">Site</a><a href="https://e.yildizskylab.com/realms/e-skylab/login-actions/action-token?key=eyJhbGciOi.gizli">Sıfırla</a><a href="https://yildizskylab.com/yardim">Yardım</a></body>`
	if got := tracking.CommonLinks([]string{mail}, base); len(got) != 3 {
		t.Fatalf("CommonLinks of a single mail = %q, want all three", got)
	}
	got := tracking.CommonLiteralLinks([]string{mail}, template, base)
	want := []string{"https://yildizskylab.com/", "https://yildizskylab.com/yardim"}
	if !slices.Equal(got, want) {
		t.Fatalf("CommonLiteralLinks = %q, want %q", got, want)
	}
	// A variable that happens to print a literal link's address is that
	// address: nothing of its own goes to /t/.
	same := `<body><a href="https://yildizskylab.com/">Site</a><a href="https://yildizskylab.com/yardim">Sıfırla</a></body>`
	if got := tracking.CommonLiteralLinks([]string{same}, template, base); !slices.Equal(got, want) {
		t.Fatalf("CommonLiteralLinks = %q, want %q", got, want)
	}
	if got := tracking.CommonLiteralLinks([]string{mail}, `<body>{{safeHTML .BodyHtml}}</body>`, base); len(got) != 0 {
		t.Fatalf("a template that writes no link out counts %q", got)
	}
}
