package mailer

import (
	"bytes"
	"strings"
	"testing"
	textt "text/template"
)

// The free-form body is the one variable carrying markup, and it lands in both
// bodies of the same mail. Before the text renderer had its own safeHTML, the
// plain-text alternative showed the recipient the tags.
func TestPlainTextBodyHasNoTags(t *testing.T) {
	body := `<h2>GECEKODU</h2><p>Bu yıl <strong>12–13 Nisan</strong>'da.</p>` +
		`<ul><li>Takımını kur</li><li>24 saat</li></ul>` +
		`<p><a href="https://skyl.app/gecekodu">Başvuruya git</a></p>`

	tmpl := textt.Must(textt.New("text").Funcs(textFuncs).Parse(`{{safeHTML .BodyHtml}}`))

	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{"BodyHtml": body}); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	for _, tag := range []string{"<h2>", "<p>", "<strong>", "<ul>", "<li>", "<a ", "href="} {
		if strings.Contains(got, tag) {
			t.Errorf("plain text still carries %q:\n%s", tag, got)
		}
	}

	for _, want := range []string{
		"GECEKODU",
		"12–13 Nisan",
		"- Takımını kur",
		"- 24 saat",
		// html-to-text writes a link as its text then its address, and the rest
		// of the plain-text part already reads that way.
		"Başvuruya git https://skyl.app/gecekodu",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plain text lost %q:\n%s", want, got)
		}
	}
}

func TestPlainTextNumbersAnOrderedList(t *testing.T) {
	tmpl := textt.Must(textt.New("text").Funcs(textFuncs).Parse(`{{safeHTML .BodyHtml}}`))

	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{
		"BodyHtml": "<ol><li>bir</li><li>iki</li><li>üç</li></ol>",
	}); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"1. bir", "2. iki", "3. üç"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("got %q, want it to contain %q", out.String(), want)
		}
	}
}

// The sanitiser runs on this path too: the text body must not become a way to
// smuggle anything either, and a script's text should not survive as content.
func TestPlainTextDropsWhatTheAllowlistDrops(t *testing.T) {
	tmpl := textt.Must(textt.New("text").Funcs(textFuncs).Parse(`{{safeHTML .BodyHtml}}`))

	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{
		"BodyHtml": `<p>merhaba</p><script>alert(1)</script><img src="https://tracker.example/x.gif">`,
	}); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	if !strings.Contains(got, "merhaba") {
		t.Errorf("lost the text: %q", got)
	}
	for _, unwanted := range []string{"alert(1)", "tracker.example", "<script", "<img"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("plain text kept %q: %q", unwanted, got)
		}
	}
}

func TestPlainTextKeepsParagraphsApart(t *testing.T) {
	tmpl := textt.Must(textt.New("text").Funcs(textFuncs).Parse(`{{safeHTML .BodyHtml}}`))

	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{
		"BodyHtml": "<p>birinci</p><p>ikinci</p>",
	}); err != nil {
		t.Fatal(err)
	}

	if got := out.String(); got != "birinci\n\nikinci" {
		t.Errorf("got %q, want %q", got, "birinci\n\nikinci")
	}
}

func TestPlainTextHandlesAnEmptyBody(t *testing.T) {
	tmpl := textt.Must(textt.New("text").Funcs(textFuncs).Parse(`[{{safeHTML .Missing}}]`))

	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "[]" {
		t.Errorf("got %q, want %q", got, "[]")
	}
}
