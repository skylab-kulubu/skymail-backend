package mailer

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
	"time"
)

// The free-form template is the one place a sender's own markup reaches the
// renderer, so what survives the allowlist is a security boundary, not styling.
func TestSafeHTMLKeepsFormattingAndDropsScripts(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		notWant string
	}{
		{
			name: "keeps bold",
			body: "<strong>duyuru</strong>",
			want: "<strong>duyuru</strong>",
		},
		{
			name: "keeps links",
			body: `<a href="https://yildizskylab.com">site</a>`,
			want: `href="https://yildizskylab.com"`,
		},
		{
			name:    "drops script tags",
			body:    `merhaba<script>alert(1)</script>`,
			want:    "merhaba",
			notWant: "alert(1)",
		},
		{
			name:    "drops event handlers",
			body:    `<p onclick="steal()">metin</p>`,
			want:    "<p>metin</p>",
			notWant: "onclick",
		},
		{
			name:    "drops javascript urls",
			body:    `<a href="javascript:steal()">tıkla</a>`,
			want:    "tıkla",
			notWant: "javascript:",
		},
		{
			name:    "drops style and images",
			body:    `<img src="https://tracker.example/pixel.gif"><p style="x">metin</p>`,
			want:    "<p>metin</p>",
			notWant: "tracker.example",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl := template.Must(template.New("body").Funcs(mailFuncs).Parse(`{{safeHTML .BodyHtml}}`))

			var out bytes.Buffer
			if err := tmpl.Execute(&out, map[string]any{"BodyHtml": tt.body}); err != nil {
				t.Fatalf("execute: %v", err)
			}

			got := out.String()
			if !strings.Contains(got, tt.want) {
				t.Errorf("got %q, want it to contain %q", got, tt.want)
			}
			if tt.notWant != "" && strings.Contains(got, tt.notWant) {
				t.Errorf("got %q, want it NOT to contain %q", got, tt.notWant)
			}
		})
	}
}

// Every other variable must stay escaped — safeHTML is opt-in per template.
func TestOrdinaryVariablesStayEscaped(t *testing.T) {
	tmpl := template.Must(template.New("body").Funcs(mailFuncs).Parse(`{{.FormTitle}}`))

	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{"FormTitle": "<b>x</b>"}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got := out.String(); strings.Contains(got, "<b>") {
		t.Errorf("got %q, want the markup escaped", got)
	}
}

func TestSafeHTMLHandlesMissingValue(t *testing.T) {
	tmpl := template.Must(template.New("body").Funcs(mailFuncs).Parse(`[{{safeHTML .Missing}}]`))

	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got := out.String(); got != "[]" {
		t.Errorf("got %q, want %q", got, "[]")
	}
}

func TestRetryDelayBacksOffAndCaps(t *testing.T) {
	want := []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
	}

	for attempts, expected := range want {
		if got := retryDelay(attempts); got != expected {
			t.Errorf("retryDelay(%d) = %v, want %v", attempts, got, expected)
		}
	}

	if got := retryDelay(20); got != 10*time.Minute {
		t.Errorf("retryDelay(20) = %v, want the 10m cap", got)
	}
}

// A Parse failure hands back a nil template, so a part whose error is not
// returned reaches Execute and dereferences nil. The subject used to be that
// part: its error was logged and execution carried on. Anyone with
// templates:write could reach it by saving `{{.Broken` as a subject.
func TestParseMailTemplatesReturnsOnEveryMalformedPart(t *testing.T) {
	const broken = "{{.Broken"

	tests := []struct {
		name      string
		subject   string
		plainText string
		html      string
		want      string
	}{
		{name: "subject", subject: broken, want: "invalid subject template"},
		{name: "plain text", plainText: broken, want: "invalid plain text template"},
		{name: "html", html: broken, want: "invalid html template"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parseMailTemplates(tt.subject, tt.plainText, tt.html)
			if err == nil {
				t.Fatalf("expected an error for a malformed %s, got none", tt.name)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name the part that failed: got %q, want it to contain %q", err, tt.want)
			}
			if parsed.subject != nil || parsed.text != nil || parsed.html != nil {
				t.Error("a failed parse must hand back no templates at all")
			}
		})
	}
}

// The renderer executes all three parts, so all three have to come back usable.
func TestParseMailTemplatesReturnsAllThreeWhenValid(t *testing.T) {
	parsed, err := parseMailTemplates("Merhaba {{.FullName}}", "{{.FullName}}", "<p>{{.FullName}}</p>")
	if err != nil {
		t.Fatalf("valid templates should parse: %v", err)
	}
	for name, ok := range map[string]bool{
		"subject": parsed.subject != nil,
		"text":    parsed.text != nil,
		"html":    parsed.html != nil,
	} {
		if !ok {
			t.Errorf("%s template is nil", name)
		}
	}

	var buf bytes.Buffer
	if err := parsed.subject.Execute(&buf, map[string]any{"FullName": "Ali"}); err != nil {
		t.Fatalf("subject should execute: %v", err)
	}
	if got := buf.String(); got != "Merhaba Ali" {
		t.Errorf("subject rendered %q, want %q", got, "Merhaba Ali")
	}
}
