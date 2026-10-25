package mailer

import (
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

const notrackBase = "https://mail.example.test"

// A sender marks a personal or one-off link in the free body "do not count":
// the mark survives the allowlist, and the link is then neither counted nor
// sent through /t/ (hub review T7; SendGrid clicktracking=off, Postmark
// data-pm-no-track). Only on a link, only empty or "true".
func TestSanitizeKeepsNoTrackOnALink(t *testing.T) {
	for _, body := range []string{
		`<p><a href="https://zoom.example/j/1?pwd=kisisel" data-skymail-notrack="">Toplantı</a></p>`,
		`<p><a href="https://zoom.example/j/1?pwd=kisisel" data-skymail-notrack="true">Toplantı</a></p>`,
		`<p><a data-skymail-notrack href="https://zoom.example/j/1?pwd=kisisel">Toplantı</a></p>`,
	} {
		got := sanitizeEmailHTML(body)
		if !strings.Contains(got, tracking.NoTrackAttribute) {
			t.Fatalf("sanitize(%q) = %q, want the no-track mark kept", body, got)
		}
		if links := tracking.Links(got, notrackBase); len(links) != 0 {
			t.Fatalf("Links(%q) = %v, want the marked link left out", got, links)
		}
		clicks := map[string]string{"https://zoom.example/j/1?pwd=kisisel": notrackBase + "/t/c/x"}
		if decorated := tracking.Decorate(got, "", clicks); strings.Contains(decorated, "/t/c/") {
			t.Fatalf("Decorate(%q) = %q, want the marked link not rewritten", got, decorated)
		}
	}

	unmarked := sanitizeEmailHTML(`<p><a href="https://yildizskylab.com/etkinlik">Etkinlik</a></p>`)
	if links := tracking.Links(unmarked, notrackBase); len(links) != 1 {
		t.Fatalf("Links(%q) = %v, want the unmarked link counted", unmarked, links)
	}
}

// The mark is the one attribute added to the allowlist: no other data-
// attribute, no other attribute, no mark off a link, no mark with a value
// that could carry anything.
func TestSanitizePassesNoOtherAttribute(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		notWant []string
	}{
		{
			name:    "other data attributes on a link",
			body:    `<a href="https://yildizskylab.com" data-skymail-track="x" data-pm-no-track="" data-x="1" data-skymail-notrack-x="">a</a>`,
			notWant: []string{"data-skymail-track", "data-pm-no-track", "data-x", "data-skymail-notrack-x"},
		},
		{
			name:    "other attributes on a link",
			body:    `<a href="https://yildizskylab.com" data-skymail-notrack="" style="color:red" class="c" id="i" title="t" onclick="x()" clicktracking="off">a</a>`,
			notWant: []string{"style", "class", "id=", "title", "onclick", "clicktracking"},
		},
		{
			name:    "the mark off a link",
			body:    `<p data-skymail-notrack="">p</p><strong data-skymail-notrack="true">s</strong><img src="https://x.example/p.gif" data-skymail-notrack="">`,
			notWant: []string{"data-skymail-notrack", "<img"},
		},
		{
			name:    "the mark with another value",
			body:    `<a href="https://yildizskylab.com" data-skymail-notrack="javascript:x()">a</a><a href="https://yildizskylab.com/2" data-skymail-notrack="false">b</a>`,
			notWant: []string{"data-skymail-notrack", "javascript"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeEmailHTML(tt.body)
			for _, bad := range tt.notWant {
				if strings.Contains(got, bad) {
					t.Errorf("sanitize(%q) = %q, want no %q", tt.body, got, bad)
				}
			}
		})
	}
}

// A News record's body (hub ticket duyuru-haber 06, SPEC §5.7 G7) may also
// strike text through, show code and a preformatted block, and draw a rule;
// a script, a javascript: link, an image and a table still never reach a
// mailbox.
func TestSanitizeKeepsTheNewsElements(t *testing.T) {
	body := `<p><s>eski</s> <code>go test</code></p><pre>satır 1
satır 2</pre><hr><script>alert(1)</script><a href="javascript:alert(1)">x</a><img src="https://x.example/a.png"><table><tr><td>t</td></tr></table>`
	got := sanitizeEmailHTML(body)
	for _, want := range []string{"<s>eski</s>", "<code>go test</code>", "<pre>satır 1\nsatır 2</pre>", "<hr"} {
		if !strings.Contains(got, want) {
			t.Errorf("sanitize = %q, want %q kept", got, want)
		}
	}
	for _, notWant := range []string{"<script", "alert(1)</script>", "javascript:", "<img", "<table", "<td"} {
		if strings.Contains(got, notWant) {
			t.Errorf("sanitize = %q, want %q dropped", got, notWant)
		}
	}
}
