package tracking_test

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

const pixel = base + "/t/o/AAAAAAAAAAAAAAAAAAAAAA"

// undecorate takes the pixel and the rewritten links back out of a decorated
// mail: what is left must be the mail as it was, byte for byte.
func undecorate(t *testing.T, decorated string, clicks map[string]string) string {
	t.Helper()
	img := tracking.PixelHTML(pixel)
	if strings.Count(decorated, img) != 1 {
		t.Fatalf("the pixel is in the mail %d times, want once:\n%s", strings.Count(decorated, img), decorated)
	}
	out := strings.Replace(decorated, img, "", 1)
	for target, click := range clicks {
		out = strings.ReplaceAll(out, `href="`+click+`"`, "href=\x00"+target+"\x00")
	}
	return out
}

// The pixel goes right after <body>, before anything a client could clip,
// and only the hrefs that are a registered link become their click address.
// Everything else — the doctype, the head, the styles, the attributes, other
// links, a link the template marks data-skymail-notrack, text that looks like
// a link — is as it was, byte for byte. No sentence, no footer and no /t/x
// link is added (T8).
func TestDecorateAddsThePixelAndRewritesOnlyRegisteredLinks(t *testing.T) {
	body := `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Transitional//EN"><html lang="tr"><head><meta charset="UTF-8"><style>a[href^="https"]{color:#003694}</style><!--[if mso]><v:roundrect href="https://skylab.example/etkinlik"></v:roundrect><![endif]--></head><BODY style="margin:0" class='x'>
<table role="presentation"><tr><td><a class="btn" style="color:red" href="https://skylab.example/etkinlik?a=1&amp;b=2" target="_blank">Katıl</a></td></tr></table>
<p>Adres: https://skylab.example/etkinlik?a=1&amp;b=2</p>
<a href="https://skylab.example/form" data-skymail-notrack>Form (işaretli)</a>
<a href='https://skylab.example/form'>Form</a>
<a href="mailto:yk@example.com">Yaz</a>
<a href="https://skylab.example/kayitsiz">Kayıtsız</a>
<img src="https://cdn.example.com/logo.png" alt="SKY LAB"/>
</BODY></html>`
	clicks := map[string]string{
		"https://skylab.example/etkinlik?a=1&b=2": base + "/t/c/AAAAAAAAAAAAAAAAAAAAAA/BBBBBBBBBBB",
		"https://skylab.example/form":             base + "/t/c/AAAAAAAAAAAAAAAAAAAAAA/CCCCCCCCCCC",
	}
	got := tracking.Decorate(body, pixel, clicks)

	if !strings.HasPrefix(got, `<!DOCTYPE html PUBLIC`) ||
		!strings.Contains(got, `<BODY style="margin:0" class='x'>`+tracking.PixelHTML(pixel)+"\n<table") {
		t.Fatalf("the pixel is not right after <body>:\n%s", got)
	}
	for _, want := range []string{
		`<a class="btn" style="color:red" href="` + clicks["https://skylab.example/etkinlik?a=1&b=2"] + `" target="_blank">Katıl</a>`,
		`<a href="` + clicks["https://skylab.example/form"] + `">Form</a>`,
		`<a href="https://skylab.example/form" data-skymail-notrack>Form (işaretli)</a>`,
		`<a href="mailto:yk@example.com">Yaz</a>`,
		`<a href="https://skylab.example/kayitsiz">Kayıtsız</a>`,
		`<p>Adres: https://skylab.example/etkinlik?a=1&amp;b=2</p>`,
		`<v:roundrect href="https://skylab.example/etkinlik">`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the decorated mail lacks %s", want)
		}
	}
	for _, never := range []string{"/t/x", "takip", "izleme", "tracking"} {
		if strings.Contains(strings.ToLower(strings.ReplaceAll(got, pixel, "")), never) {
			t.Errorf("the decorated mail has %q: nothing but the pixel and the links is added", never)
		}
	}

	// Taken back out, the rest is the mail as it was. The rewritten tags are
	// written anew, so they are compared as tags.
	left := undecorate(t, got, clicks)
	wantLeft := strings.NewReplacer(
		`<a class="btn" style="color:red" href="https://skylab.example/etkinlik?a=1&amp;b=2" target="_blank">`,
		`<a class="btn" style="color:red" href=`+"\x00https://skylab.example/etkinlik?a=1&b=2\x00"+` target="_blank">`,
		`<a href='https://skylab.example/form'>`,
		`<a href=`+"\x00https://skylab.example/form\x00"+`>`,
	).Replace(body)
	if left != wantLeft {
		t.Fatalf("taken back out, the mail is not as it was:\n got %q\nwant %q", left, wantLeft)
	}
}

// A mail without <body> gets the pixel first; a mail with nothing to rewrite
// is otherwise unchanged; an empty one is the pixel alone.
func TestDecorateWithoutABody(t *testing.T) {
	body := `<p>Merhaba</p><a href="https://skylab.example/a">A</a>`
	got := tracking.Decorate(body, pixel, nil)
	if got != tracking.PixelHTML(pixel)+body {
		t.Fatalf("Decorate without a body = %q", got)
	}
	if got := tracking.Decorate("", pixel, nil); got != tracking.PixelHTML(pixel) {
		t.Fatalf("Decorate of nothing = %q", got)
	}
}

// A mail with a doctype but no <body> (valid HTML) keeps its doctype first:
// markup before it would put clients in quirks mode. The pixel goes where
// the body begins: after </head>, else after <html>, else after the
// doctype.
func TestDecorateWithoutABodyKeepsTheDoctypeFirst(t *testing.T) {
	img := tracking.PixelHTML(pixel)
	for body, want := range map[string]string{
		`<!DOCTYPE html><html lang="tr"><head><title>x</title></head><p>x</p></html>`: `<!DOCTYPE html><html lang="tr"><head><title>x</title></head>` + img + `<p>x</p></html>`,
		`<!DOCTYPE html><html><p>x</p></html>`:                                        `<!DOCTYPE html><html>` + img + `<p>x</p></html>`,
		`<!DOCTYPE html><p>x</p>`:                                                     `<!DOCTYPE html>` + img + `<p>x</p>`,
		"\n<!-- önce --><!doctype html>\n<p>x</p>":                                    "\n<!-- önce --><!doctype html>" + img + "\n<p>x</p>",
		`<html><head></head><p>x</p>`:                                                 `<html><head></head>` + img + `<p>x</p>`,
	} {
		if got := tracking.Decorate(body, pixel, nil); got != want {
			t.Errorf("Decorate(%q)\n = %q\nwant %q", body, got, want)
		}
	}
}

// Markup the tokenizer cannot finish (a mail cut in a tag) comes out as it
// went in, with the pixel.
func TestDecorateKeepsBrokenMarkup(t *testing.T) {
	for _, body := range []string{
		`<body><p>Yarım <a href="https://skylab.example/a"`,
		`<body><p>Yarım</p><!-- yorum`,
		`<body><style>p{color:red}`,
		"<body>\x00<p>nul</p>",
	} {
		got := tracking.Decorate(body, pixel, map[string]string{"https://skylab.example/b": base + "/t/c/x/y"})
		if left := undecorate(t, got, nil); left != body {
			t.Errorf("Decorate(%q) = %q; taken back out %q", body, got, left)
		}
	}
}

// The pixel is a 1×1 image with an empty alt, its address escaped.
func TestPixelHTML(t *testing.T) {
	img := tracking.PixelHTML(base + `/t/o/a"b`)
	if !strings.HasPrefix(img, `<img src="`+base+`/t/o/a&#34;b" width="1" height="1" alt=""`) {
		t.Fatalf("PixelHTML = %s", img)
	}
	if tracking.PixelURL(base, make([]byte, 16)) != base+"/t/o/AAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("PixelURL = %s", tracking.PixelURL(base, make([]byte, 16)))
	}
	if tracking.ClickURL(base, make([]byte, 16), make([]byte, 8)) != base+"/t/c/AAAAAAAAAAAAAAAAAAAAAA/AAAAAAAAAAA" {
		t.Fatalf("ClickURL = %s", tracking.ClickURL(base, make([]byte, 16), make([]byte, 8)))
	}
}
