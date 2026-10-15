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
