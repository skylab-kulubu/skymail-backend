package mailer

import (
	"bytes"
	"encoding/json"
	"errors"
	htmlt "html/template"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"golang.org/x/net/html"
)

// A sensitive send's content (a login or verification code, a one-time link)
// is shown masked (Yusuf, 2026-10-13: "kod/bağlantı maskeli, gerisi açık"):
// the whole mail as it was sent, with every code and one-time link replaced by
// Mask before it leaves the server. The unmasked value never reaches a client.
//
// How, most reliable first:
//
//  1. Variables: the send's variables that carry a code or a link are masked
//     and the mail is rendered again. That render is trusted only when the
//     same template rendered with the unmasked variables gives back exactly
//     the mail that was sent — so it is the template the send used, whatever
//     the template became since — and a value is masked wherever the template
//     put it: in the text, in an href, in the subject, cut up or escaped.
//  2. Patterns, when no template renders the mail that was sent (the
//     template changed outside its versions, Account erasure emptied the
//     variables): the known values are replaced in the stored mail, then every
//     URL, every run of four or more digits, and every code- or token-shaped
//     word is masked in the subject, the plain text, the HTML's text and its
//     attributes. It masks more than it must; it never shows less than 1 does.
//
// Either way a last check looks for each credential the variables held, raw
// and in the forms the renderers escape it to; one found is ErrCannotMask and
// nothing is shown.

// Mask is what a masked code or link reads as.
const Mask = "••••••"

// maskMark is a masked value in the HTML part: a <mark> the panel can make
// stand out. Within an attribute html/template strips it to its text.
const maskMark = `<mark data-skymail-masked="">` + Mask + `</mark>`

// MaskMethod says how a mail was masked.
type MaskMethod string

const (
	// MaskedByVariables: rendered again with its code and link variables masked.
	MaskedByVariables MaskMethod = "variables"
	// MaskedByPatterns: the stored mail with every URL, long number and
	// code-shaped word masked.
	MaskedByPatterns MaskMethod = "patterns"
)

// TemplateParts is one version of a template's three parts, a candidate for
// the template a send rendered.
type TemplateParts struct {
	Subject   string
	PlainText string
	HTML      string
}

// MaskedMail is a sent mail with its codes and one-time links masked.
type MaskedMail struct {
	Rendered
	// The variables masked, by their path in the send's variables (link,
	// answers[0].url), sorted.
	Variables []string
	Method    MaskMethod
}

// ErrCannotMask is a mail in which a code or a link the send's variables held
// is still found after masking: it is not shown.
var ErrCannotMask = errors.New("mailer: a code or a link in the mail could not be masked")

// MaskSent masks a sent mail: sent is what the queue row holds, variables the
// send's body_variables, recipient whom the row went to, and candidates the
// template's versions, newest first, the one in use first.
func MaskSent(sent Rendered, variables []byte, recipient RecipientInfo, candidates []TemplateParts) (MaskedMail, error) {
	vars, varsKnown := parseSendVariables(variables)
	masked := maskVariables(vars)

	if varsKnown {
		for _, candidate := range candidates {
			parsed, err := parseMailTemplates(candidate.Subject, candidate.PlainText, candidate.HTML)
			if err != nil {
				continue
			}
			original, err := parsed.render(vars, recipient)
			if err != nil || original != sent {
				continue
			}
			out, err := parsed.renderWith(masked.text, masked.html, recipient)
			if err != nil {
				break
			}
			if leaks(out, masked.credentials) {
				break
			}
			return MaskedMail{Rendered: out, Variables: masked.names, Method: MaskedByVariables}, nil
		}
	}

	known := append(append([]string{}, masked.values...), masked.credentials...)
	out := Rendered{
		Subject:   maskText(replaceValues(sent.Subject, known)),
		PlainText: maskText(replaceValues(sent.PlainText, known)),
		HTML:      maskHTML(replaceValues(sent.HTML, known)),
	}
	if leaks(out, masked.credentials) {
		return MaskedMail{}, ErrCannotMask
	}
	return MaskedMail{Rendered: out, Variables: masked.names, Method: MaskedByPatterns}, nil
}

// parseSendVariables reads a send's body_variables; an empty one is no
// variables. false is variables that do not read as an object.
func parseSendVariables(raw []byte) (map[string]any, bool) {
	vars := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return vars, true
	}
	if err := json.Unmarshal(raw, &vars); err != nil || vars == nil {
		return map[string]any{}, false
	}
	return vars, true
}

// maskedVariables is a send's variables with what carries a code or a link
// masked: text for the subject and the plain text, html (with maskMark) for
// the HTML.
type maskedVariables struct {
	text, html map[string]any
	names      []string
	// Every value masked, to replace in a stored mail.
	values []string
	// The values that are a credential — a code, a token, a link that carries
	// one — which must not be found in a masked mail.
	credentials []string
}

func maskVariables(vars map[string]any) maskedVariables {
	m := maskedVariables{text: map[string]any{}, html: map[string]any{}}
	names := map[string]bool{}
	for key, value := range vars {
		m.text[key], m.html[key] = m.mask(key, key, value, names)
	}
	for name := range names {
		m.names = append(m.names, name)
	}
	sort.Strings(m.names)
	return m
}

// mask masks one value found under name (its last key) at path.
func (m *maskedVariables) mask(name, path string, value any, names map[string]bool) (text, markup any) {
	switch v := value.(type) {
	case map[string]any:
		text, markup := map[string]any{}, map[string]any{}
		for key, inner := range v {
			text[key], markup[key] = m.mask(key, path+"."+key, inner, names)
		}
		return text, markup
	case []any:
		text, markup := make([]any, len(v)), make([]any, len(v))
		for i, inner := range v {
			text[i], markup[i] = m.mask(name, path+"["+strconv.Itoa(i)+"]", inner, names)
		}
		return text, markup
	case string:
		if v == "" {
			return v, v
		}
		if secretName(name) || secretShaped(strings.TrimSpace(v)) {
			names[path] = true
			m.addValue(v)
			return Mask, htmlt.HTML(maskMark)
		}
		urls := urlInText.FindAllString(v, -1)
		if len(urls) == 0 {
			return v, v
		}
		names[path] = true
		for _, u := range urls {
			m.addValue(u)
		}
		parts := urlInText.Split(v, -1)
		var markup strings.Builder
		for i, part := range parts {
			markup.WriteString(htmlt.HTMLEscapeString(part))
			if i < len(parts)-1 {
				markup.WriteString(maskMark)
			}
		}
		return strings.Join(parts, Mask), htmlt.HTML(markup.String())
	case float64:
		digits := strconv.FormatFloat(v, 'f', -1, 64)
		if secretName(name) || (v == float64(int64(v)) && digitRun.MatchString(digits) && len(digits) >= 6) {
			names[path] = true
			m.addValue(digits)
			return Mask, htmlt.HTML(maskMark)
		}
		return v, v
	default:
		return v, v
	}
}

func (m *maskedVariables) addValue(v string) {
	m.values = append(m.values, v)
	m.credentials = append(m.credentials, credentialsIn(v)...)
}

// secretNameEnds are the last word of a variable that carries a code or a
// link: link, ActionUrl, CancelUrl, code, TicketCode, otp, token… A name only
// containing one (codeExpirationMinutes, linkExpirationMinutes, subjectKey) is
// not one: masking a variable a template branches on would change the mail.
var secretNameEnds = []string{
	"link", "links", "url", "urls", "uri", "href", "code", "codes", "otp", "pin", "token", "tokens",
	"passcode", "password", "secret", "kod", "kodu", "baglanti", "bağlantı", "bağlantısı", "sifre", "şifre", "parola",
}

// secretNameParts are words that make any variable a credential.
var secretNameParts = []string{"token", "password", "passcode", "secret", "otp"}

func secretName(name string) bool {
	lower := strings.ToLower(strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, name))
	for _, end := range secretNameEnds {
		if strings.HasSuffix(lower, end) {
			return true
		}
	}
	for _, part := range secretNameParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

var (
	// An address in text: what a mail client would make a link of.
	urlInText = regexp.MustCompile(`(?i)(?:\b[a-z][a-z0-9+.-]*://|\bwww\.)[^\s<>"'` + "`" + `]+`)
	digitRun  = regexp.MustCompile(`^[0-9]+$`)
	// A word a code or a token could be: letters, digits and the URL-safe
	// marks, not broken by a space.
	codeWord   = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_~+/=.-]*[A-Za-z0-9=]`)
	numberWord = regexp.MustCompile(`[0-9]+(?:[.,:/-][0-9]+)*`)
)

// secretShaped is a value that looks like a credential whatever its name: an
// address, a number of four to ten digits, a code of capitals and digits, a
// long token.
func secretShaped(v string) bool {
	if v == "" {
		return false
	}
	if loc := urlInText.FindStringIndex(v); loc != nil && loc[0] == 0 && loc[1] == len(v) {
		return true
	}
	if digitRun.MatchString(v) {
		return len(v) >= 4 && len(v) <= 10
	}
	return codeShaped(v) || tokenShaped(v)
}

// codeShaped is a ticket or invite code: 5 to 20 capitals, digits and dashes,
// with at least one of each of the first two.
func codeShaped(w string) bool {
	if len(w) < 5 || len(w) > 20 {
		return false
	}
	var letter, digit bool
	for _, r := range w {
		switch {
		case r >= 'A' && r <= 'Z':
			letter = true
		case r >= '0' && r <= '9':
			digit = true
		case r == '-':
		default:
			return false
		}
	}
	return letter && digit
}

// tokenShaped is a token: 20 or more URL-safe characters with letters and
// digits, no space.
func tokenShaped(w string) bool {
	if len(w) < 20 {
		return false
	}
	var letter, digit bool
	for _, r := range w {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letter = true
		case r >= '0' && r <= '9':
			digit = true
		case strings.ContainsRune("_~+/=.-", r):
		default:
			return false
		}
	}
	return letter && digit
}

// credentialsIn are the parts of a masked value that must not be found in
// the masked mail: a code, a token, and of a link the link itself when it
// carries a query or a long path segment, and each such value on its own. A
// bare address (https://yildizskylab.com) is masked but not looked for: the
// template may print the same address itself.
func credentialsIn(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	parsed, err := url.Parse(v)
	if err != nil || parsed.Host == "" {
		if loc := urlInText.FindStringIndex(v); loc == nil {
			if len(v) >= 4 {
				return []string{v}
			}
			return nil
		}
		parsed, err = url.Parse("https://" + strings.TrimPrefix(v, "www."))
		if err != nil {
			return []string{v}
		}
	}
	var found []string
	carries := false
	for _, values := range parsed.Query() {
		for _, value := range values {
			if len(value) >= 8 {
				found = append(found, value)
				carries = true
			} else if value != "" {
				carries = true
			}
		}
	}
	for _, segment := range strings.Split(parsed.EscapedPath(), "/") {
		if len(segment) >= 16 {
			found = append(found, segment)
			carries = true
		}
	}
	if parsed.Fragment != "" && len(parsed.Fragment) >= 8 {
		found = append(found, parsed.Fragment)
		carries = true
	}
	if carries {
		found = append(found, v)
	}
	return found
}

// replaceValues replaces each value, in the forms the renderers print it,
// with Mask, the longest first.
func replaceValues(s string, values []string) string {
	if s == "" || len(values) == 0 {
		return s
	}
	var forms []string
	seen := map[string]bool{}
	for _, value := range values {
		for _, form := range printedForms(value) {
			if form != "" && !seen[form] {
				seen[form] = true
				forms = append(forms, form)
			}
		}
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	for _, form := range forms {
		s = strings.ReplaceAll(s, form, Mask)
	}
	return s
}

// printedForms is a value as text/template, html/template (in text, in an
// attribute, in a URL) and a URL's query or path may print it.
func printedForms(v string) []string {
	return []string{
		v,
		htmlt.HTMLEscapeString(v),
		strings.ReplaceAll(v, "&", "&amp;"),
		urlAttrForm(v),
		url.QueryEscape(v),
		url.PathEscape(v),
	}
}

var urlAttrTemplate = sync.OnceValue(func() *htmlt.Template {
	return htmlt.Must(htmlt.New("href").Parse(`<a href="{{.}}">`))
})

// urlAttrForm is a value as html/template prints it in an href.
func urlAttrForm(v string) string {
	var out bytes.Buffer
	if err := urlAttrTemplate().Execute(&out, v); err != nil {
		return v
	}
	return strings.TrimSuffix(strings.TrimPrefix(out.String(), `<a href="`), `">`)
}

// leaks reports whether any credential, in any form the renderers print it,
// is in the mail, letter case aside.
func leaks(mail Rendered, credentials []string) bool {
	if len(credentials) == 0 {
		return false
	}
	parts := []string{
		strings.ToLower(mail.Subject),
		strings.ToLower(mail.PlainText),
		strings.ToLower(mail.HTML),
	}
	for _, credential := range credentials {
		for _, form := range printedForms(credential) {
			if len(form) < 4 {
				continue
			}
			form = strings.ToLower(form)
			for _, part := range parts {
				if strings.Contains(part, form) {
					return true
				}
			}
		}
	}
	return false
}

// maskText masks, in text, every address, every token- and code-shaped word
// and every run of four or more digits. A number with separators in it — a
// date, a time, an amount — stays.
func maskText(s string) string {
	if s == "" {
		return s
	}
	s = urlInText.ReplaceAllString(s, Mask)
	s = codeWord.ReplaceAllStringFunc(s, func(w string) string {
		if tokenShaped(w) || codeShaped(w) {
			return Mask
		}
		return w
	})
	return numberWord.ReplaceAllStringFunc(s, func(w string) string {
		if digitRun.MatchString(w) && len(w) >= 4 {
			return Mask
		}
		return w
	})
}

// textAttributes are the attributes whose value is read as text, masked as
// text is; every other attribute has its addresses masked.
var textAttributes = map[string]bool{"alt": true, "title": true, "value": true, "content": true, "aria-label": true, "placeholder": true}

// maskHTML masks the HTML's text, comments and attributes as maskText does,
// marking what it masked in the text. A style sheet, which shows no text, has
// its addresses masked; a title, a text area and a script are masked as text.
func maskHTML(src string) string {
	if strings.TrimSpace(src) == "" {
		return src
	}
	tokenizer := html.NewTokenizer(strings.NewReader(src))
	var out strings.Builder
	// The element whose raw text the tokenizer is in: style, script, title or
	// textarea.
	inRaw := ""
	for {
		kind := tokenizer.Next()
		// Raw is overwritten by Token: read it first.
		raw := string(tokenizer.Raw())
		if kind == html.ErrorToken {
			if tokenizer.Err() != io.EOF {
				// The tokenizer gave up: what is left is masked as text.
				out.WriteString(html.EscapeString(maskText(raw)))
			}
			break
		}
		token := tokenizer.Token()
		switch kind {
		case html.TextToken:
			switch inRaw {
			case "style":
				out.WriteString(urlInText.ReplaceAllString(raw, Mask))
			case "":
				out.WriteString(markMasked(maskText(token.Data)))
			default:
				out.WriteString(maskText(raw))
			}
		case html.CommentToken:
			out.WriteString("<!--" + maskText(token.Data) + "-->")
		case html.StartTagToken, html.SelfClosingTagToken:
			if kind == html.StartTagToken {
				switch token.Data {
				case "style", "script", "title", "textarea":
					inRaw = token.Data
				}
			}
			changed := false
			for i, attr := range token.Attr {
				masked := urlInText.ReplaceAllString(attr.Val, Mask)
				if textAttributes[attr.Key] {
					masked = maskText(attr.Val)
				}
				if masked != attr.Val {
					token.Attr[i].Val = masked
					changed = true
				}
			}
			if changed {
				out.WriteString(token.String())
			} else {
				out.WriteString(raw)
			}
		case html.EndTagToken:
			if token.Data == inRaw {
				inRaw = ""
			}
			out.WriteString(raw)
		default:
			out.WriteString(raw)
		}
	}
	return out.String()
}

// markMasked escapes text for HTML with each Mask in it marked.
func markMasked(text string) string {
	parts := strings.Split(text, Mask)
	var out strings.Builder
	for i, part := range parts {
		out.WriteString(html.EscapeString(part))
		if i < len(parts)-1 {
			out.WriteString(maskMark)
		}
	}
	return out.String()
}
