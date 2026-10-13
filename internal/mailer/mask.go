package mailer

import (
	"bytes"
	"encoding/json"
	"errors"
	htmlt "html/template"
	"io"
	"net/netip"
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
// is kept masked (Yusuf, 2026-10-13: "kod/bağlantı maskeli, gerisi açık"; the
// orchestrator after the #63 review: mask when it is stored). Once none of the
// send's rows is still to go out, the content sweep (MaskSensitiveContent)
// replaces each row's mail with the masked copy MaskSent makes and the send's
// variables with their masked values. The raw value is not kept; nothing is
// masked on reading.
//
// How, most reliable first:
//
//  1. Variables: the send's variables that carry a code, a link, an IP address
//     or a place are masked — by name, by shape, and inside text — and the
//     mail is rendered again with the template version in force when the send
//     was queued. That render is trusted only when the same template rendered
//     with the unmasked variables gives back exactly the mail that was sent.
//  2. Patterns, when it does not (the template changed outside its versions,
//     Account erasure emptied the variables): the known values are replaced
//     in the stored mail, then every link, IP address, run of four or more
//     digits (a date or a time stays), code- or token-shaped word and code cut
//     by spaces is masked in the subject, the plain text, and the HTML's text,
//     comments, attributes and style sheets. It masks more than it must.
//
// Either way a last check looks for every credential the variables held, raw,
// in each form the renderers escape it to, and with its digits cut apart; and
// for any code-shaped string the template did not print itself. One found is
// ErrCannotMask: the content is not kept.

// Mask is what a masked code or link reads as.
const Mask = "••••••"

// maskMark is a masked value in the HTML part's text: a <mark> the panel can
// make stand out. In an attribute html/template strips it to its text, and in
// a URL attribute escapes it to an address that goes nowhere.
const maskMark = `<mark data-skymail-masked="">` + Mask + `</mark>`

// MaskMethod says how a mail was masked.
type MaskMethod string

const (
	// MaskedByVariables: rendered again with its code and link variables masked.
	MaskedByVariables MaskMethod = "variables"
	// MaskedByPatterns: the stored mail with every link, IP address, long
	// number and code-shaped word masked.
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
	Method MaskMethod
}

// ErrCannotMask is a mail in which a credential is still found after
// masking: it is not kept.
var ErrCannotMask = errors.New("mailer: a code or a link in the mail could not be masked")

// Masker masks the mails of one send: its variables are read and masked once.
type Masker struct {
	vars   map[string]any
	known  bool
	masked maskedVariables
}

// NewMasker reads a send's body_variables. Variables that do not read as an
// object are unknown: only the patterns mask its mails.
func NewMasker(variables []byte) *Masker {
	vars, known := parseSendVariables(variables)
	return &Masker{vars: vars, known: known, masked: maskVariables(vars)}
}

// Variables are the paths of the variables masked (link, answers[0].url),
// sorted.
func (m *Masker) Variables() []string {
	if m.masked.names == nil {
		return []string{}
	}
	return m.masked.names
}

// MaskedVariables is the send's body_variables with every masked value
// replaced by Mask, to keep in their place; {} when they were unknown.
func (m *Masker) MaskedVariables() []byte {
	if !m.known {
		return []byte(`{}`)
	}
	out, err := json.Marshal(m.masked.text)
	if err != nil {
		return []byte(`{}`)
	}
	return out
}

// Mail masks one recipient's sent mail. candidates are the template's parts
// as they stood when the send was queued: never a version written after it,
// whose render could depend on the hidden value.
func (m *Masker) Mail(sent Rendered, recipient RecipientInfo, candidates []TemplateParts) (MaskedMail, error) {
	if m.known {
		for _, candidate := range candidates {
			parsed, err := parseMailTemplates(candidate.Subject, candidate.PlainText, candidate.HTML)
			if err != nil {
				continue
			}
			original, err := parsed.render(m.vars, recipient)
			if err != nil || original != sent {
				continue
			}
			out, err := parsed.renderWith(m.masked.text, m.masked.html, recipient)
			if err != nil {
				break
			}
			// What may look like a code: what the template writes itself, the
			// recipient, and the values left as they are.
			allowed := map[string]bool{}
			for _, own := range append([]string{candidate.Subject, candidate.PlainText, candidate.HTML,
				recipient.Email, recipient.FullName}, m.masked.kept...) {
				for item := range codeItemsOf(own) {
					allowed[item] = true
				}
			}
			if leaks(out, m.masked.credentials) || unexplained(out, allowed) {
				break
			}
			return MaskedMail{Rendered: out, Method: MaskedByVariables}, nil
		}
	}

	known := append(append([]string{}, m.masked.values...), m.masked.credentials...)
	out := Rendered{
		Subject:   maskText(replaceValues(sent.Subject, known), nil),
		PlainText: maskText(replaceValues(sent.PlainText, known), nil),
		HTML:      maskHTML(replaceValues(sent.HTML, known)),
	}
	if leaks(out, m.masked.credentials) || unexplained(out, nil) {
		return MaskedMail{}, ErrCannotMask
	}
	return MaskedMail{Rendered: out, Method: MaskedByPatterns}, nil
}

// MaskSent masks one sent mail: sent is what the queue row holds, variables
// the send's body_variables, recipient whom the row went to, and candidates
// the template's parts in force when it was sent.
func MaskSent(sent Rendered, variables []byte, recipient RecipientInfo, candidates []TemplateParts) (MaskedMail, error) {
	return NewMasker(variables).Mail(sent, recipient, candidates)
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

// maskedVariables is a send's variables with what carries a code, a link, an
// IP address or a place masked: text for the subject and the plain text, html
// (with maskMark) for the HTML.
type maskedVariables struct {
	text, html map[string]any
	names      []string
	// Every value masked, to replace in a stored mail.
	values []string
	// What must not be found in a masked mail: every code, token, link that
	// carries one, and IP address the variables held.
	credentials []string
	// The values left as they are, which may print a number on purpose
	// (linkExpirationMinutes: 1440).
	kept []string
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
		trimmed := strings.TrimSpace(v)
		if quantityName(name) && digitRun.MatchString(trimmed) {
			m.kept = append(m.kept, v)
			return v, v
		}
		if secretName(name) || secretShaped(trimmed) {
			names[path] = true
			m.addValue(v, true)
			return Mask, htmlt.HTML(maskMark)
		}
		if personalName(name) {
			// A place is masked but not looked for: the template may name the
			// same city itself.
			names[path] = true
			m.addValue(v, false)
			return Mask, htmlt.HTML(maskMark)
		}
		// Text: each link, IP address, number or code in it is masked on its own.
		const cut = "\x00"
		found := false
		marked := maskText(v, func(piece string) {
			found = true
			m.addValue(piece, true)
		})
		if !found {
			m.kept = append(m.kept, v)
			return v, v
		}
		names[path] = true
		parts := strings.Split(strings.ReplaceAll(marked, Mask, cut), cut)
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
		if quantityName(name) {
			m.kept = append(m.kept, digits)
			return v, v
		}
		if secretName(name) || (v == float64(int64(v)) && len(strings.TrimPrefix(digits, "-")) >= 4) {
			names[path] = true
			m.addValue(digits, true)
			return Mask, htmlt.HTML(maskMark)
		}
		return v, v
	default:
		return v, v
	}
}

// addValue keeps a masked value to replace in a stored mail, and, when guard,
// its credentials to look for in the masked one.
func (m *maskedVariables) addValue(v string, guard bool) {
	m.values = append(m.values, v)
	if guard {
		m.credentials = append(m.credentials, credentialsIn(v)...)
	}
}

// secretNameEnds are the last word of a variable that carries a code or a
// link: link, ActionUrl, CancelUrl, code, TicketCode, otp, token, invite… A
// name only containing one (codeExpirationMinutes, linkExpirationMinutes,
// subjectKey) is not one: masking a variable a template branches on would
// change the mail.
var secretNameEnds = []string{
	"link", "links", "url", "urls", "uri", "href", "code", "codes", "otp", "pin", "token", "tokens",
	"passcode", "password", "secret", "verifier", "invite", "invitation", "nonce", "key",
	"kod", "kodu", "baglanti", "bağlantı", "bağlantısı", "baglantisi", "sifre", "şifre", "parola", "davet",
}

// secretNameParts are words that make any variable a credential.
var secretNameParts = []string{"token", "password", "passcode", "secret", "otp"}

// notSecretNames end like one but are not: a template's subject key.
var notSecretNames = []string{"subjectkey"}

func secretName(name string) bool {
	lower := strings.ToLower(strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, name))
	for _, not := range notSecretNames {
		if lower == not {
			return false
		}
	}
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

// quantityNameEnds are the last word of a variable that holds an amount, not
// a code: its number is left as it is (linkExpirationMinutes: 1440).
var quantityNameEnds = []string{"minutes", "seconds", "hours", "days", "count"}

func quantityName(name string) bool {
	last := strings.ToLower(lastWord(name))
	for _, end := range quantityNameEnds {
		if last == end {
			return true
		}
	}
	return false
}

// A person's IP address and whereabouts are masked too (the club's rule: a
// raw IP address is shown nowhere): a variable whose last word is one of
// these, IpAddress, ClientIP, Location, Konum.
var personalNameEnds = []string{"ip", "ipaddress", "ipaddr", "ipadresi", "location", "geolocation", "konum", "coordinates"}

func personalName(name string) bool {
	last := strings.ToLower(lastWord(name))
	whole := strings.ToLower(strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, name))
	for _, end := range personalNameEnds {
		if last == end || (len(end) > 2 && strings.HasSuffix(whole, end)) {
			return true
		}
	}
	return false
}

// lastWord is a name's last word: after its last _, -, . or space, or its
// last capital that follows a small letter (ClientIP → IP, ipAddress →
// Address, remoteIp → Ip).
func lastWord(name string) string {
	runes := []rune(name)
	start := 0
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '.' || unicode.IsSpace(r):
			start = i + 1
		case i > 0 && unicode.IsUpper(r) && unicode.IsLower(runes[i-1]):
			start = i
		}
	}
	return string(runes[start:])
}

var (
	// An address in text: with a scheme or www., or a host with a path or a
	// query (skyl.app/r/x7Gk2pQ9).
	urlInText  = regexp.MustCompile(`(?i)(?:\b[a-z][a-z0-9+.-]*://|\bwww\.)[^\s<>"'` + "`" + `]+|\b(?:[a-z0-9-]+\.)+[a-z]{2,}(?::[0-9]+)?[/?][^\s<>"'` + "`" + `]*`)
	ipv4       = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	ipv6       = regexp.MustCompile(`(?i)[0-9a-f:.]*:[0-9a-f:.]*`)
	digitRun   = regexp.MustCompile(`^[0-9]+$`)
	codeWord   = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_~+/=.-]*[A-Za-z0-9=]`)
	numberWord = regexp.MustCompile(`[0-9]+(?:[.,:/-][0-9]+)*`)
	// Groups of digits cut by spaces: a code printed 483 920.
	splitDigits = regexp.MustCompile(`\d{2,4}(?:[ \x{00a0}]+\d{2,4})+`)
)

// secretShaped is a value that looks like a credential whatever its name: an
// address, an IP address, a number of four or more digits, a code, a token.
func secretShaped(v string) bool {
	if v == "" {
		return false
	}
	if loc := urlInText.FindStringIndex(v); loc != nil && loc[0] == 0 && loc[1] == len(v) {
		return true
	}
	if isIP(v) {
		return true
	}
	if digitRun.MatchString(v) {
		return len(v) >= 4
	}
	return codeShaped(v) || tokenShaped(v)
}

// codeShaped is a ticket, invite or login code: 5 to 20 letters, digits and
// dashes, with a digit and a letter, and either no small letter or at least
// two digits (QR-88K2, x7Gk2pQ9; not Firefox, not k4b-a).
func codeShaped(w string) bool {
	if len(w) < 5 || len(w) > 20 {
		return false
	}
	var upper, lower bool
	digits := 0
	for _, r := range w {
		switch {
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= '0' && r <= '9':
			digits++
		case r == '-':
		default:
			return false
		}
	}
	if digits == 0 || (!upper && !lower) {
		return false
	}
	return !lower || digits >= 2
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

// isIP is an IPv4 or IPv6 address, written whole, with or without a port (a
// time like 14:20:05 is not one).
func isIP(v string) bool {
	if !strings.ContainsAny(v, ".:") {
		return false
	}
	if _, err := netip.ParseAddrPort(v); err == nil {
		return true
	}
	_, err := netip.ParseAddr(strings.Trim(v, "[]"))
	return err == nil
}

// credentialsIn are the parts of a masked value that must not be found in
// the masked mail: a code, a token, an IP address, and of a link the link
// itself when it carries a query or a long path segment, and each such value
// on its own. A bare address (https://yildizskylab.com) is masked but not
// looked for: the template may print the same address itself.
func credentialsIn(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	if loc := urlInText.FindStringIndex(v); loc == nil || isIP(v) {
		if len(v) >= 4 {
			return []string{v}
		}
		return nil
	}
	parsed, err := url.Parse(v)
	if err != nil || parsed.Host == "" {
		parsed, err = url.Parse("https://" + strings.TrimPrefix(v, "www."))
		if err != nil {
			return []string{v}
		}
	}
	var found []string
	carries := false
	// Each query value as written (ab+cd12) and as decoded (ab cd12).
	for _, pair := range strings.Split(parsed.RawQuery, "&") {
		_, raw, _ := strings.Cut(pair, "=")
		values := []string{raw}
		if decoded, err := url.QueryUnescape(raw); err == nil && decoded != raw {
			values = append(values, decoded)
		}
		for _, value := range values {
			if len(value) >= 4 {
				found = append(found, value)
			}
			if value != "" {
				carries = true
			}
		}
	}
	for _, segment := range strings.Split(parsed.EscapedPath(), "/") {
		if len(segment) >= 16 || codeShaped(segment) || tokenShaped(segment) {
			found = append(found, segment)
			carries = true
		}
	}
	if len(parsed.Fragment) >= 8 {
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
		htmlForm(htmlTextTemplate, v),
		htmlForm(htmlAttrTemplate, v),
		htmlForm(htmlURLTemplate, v),
		url.QueryEscape(v),
		url.PathEscape(v),
	}
}

var (
	htmlTextTemplate = sync.OnceValue(func() *htmlt.Template {
		return htmlt.Must(htmlt.New("text").Parse(`<p>{{.}}</p>`))
	})
	htmlAttrTemplate = sync.OnceValue(func() *htmlt.Template {
		return htmlt.Must(htmlt.New("attr").Parse(`<p title="{{.}}">`))
	})
	htmlURLTemplate = sync.OnceValue(func() *htmlt.Template {
		return htmlt.Must(htmlt.New("href").Parse(`<a href="{{.}}">`))
	})
)

// htmlForm is a value as html/template prints it in one context: text
// (+ is &#43;), an attribute, an href.
func htmlForm(tmpl func() *htmlt.Template, v string) string {
	var out bytes.Buffer
	if err := tmpl().Execute(&out, v); err != nil {
		return v
	}
	s := out.String()
	for _, cut := range []string{`<p>`, `</p>`, `<p title="`, `<a href="`, `">`} {
		s = strings.TrimPrefix(s, cut)
		s = strings.TrimSuffix(s, cut)
	}
	return s
}

// urlAttrForm is a value as html/template prints it in an href.
func urlAttrForm(v string) string { return htmlForm(htmlURLTemplate, v) }

// digitGap joins digits cut apart by spaces, no-break spaces or dashes.
var digitGap = regexp.MustCompile(`(\d)(?:[\s\x{00a0}-]|&nbsp;|&#160;|&#xa0;)+(\d)`)

func joinDigits(s string) string {
	for {
		joined := digitGap.ReplaceAllString(s, "$1$2")
		if joined == s {
			return s
		}
		s = joined
	}
}

// leaks reports whether any credential, in any form the renderers print it,
// letter case aside, or a number credential with its digits cut apart, is in
// the mail.
func leaks(mail Rendered, credentials []string) bool {
	if len(credentials) == 0 {
		return false
	}
	parts := []string{
		strings.ToLower(mail.Subject),
		strings.ToLower(mail.PlainText),
		strings.ToLower(mail.HTML),
	}
	joined := make([]string, len(parts))
	for i, part := range parts {
		joined[i] = joinDigits(part)
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
		if digits := strings.Map(keepDigit, credential); len(digits) >= 4 && len(digits)*2 >= len(credential) {
			for _, part := range joined {
				if strings.Contains(part, digits) {
					return true
				}
			}
		}
	}
	return false
}

func keepDigit(r rune) rune {
	if r >= '0' && r <= '9' {
		return r
	}
	return -1
}

// unexplained reports whether the mail shows a code-shaped string — a run of
// four or more digits, a code, a token, an IP address — that allowed does not
// hold. With allowed nil, any such string is one.
func unexplained(mail Rendered, allowed map[string]bool) bool {
	for item := range codeItems(mail) {
		if !allowed[item] {
			return true
		}
	}
	return false
}

// codeItems are the code-shaped strings a reader sees in a mail: in the
// subject, the plain text, and the HTML's text and attributes but its style
// and layout ones.
func codeItems(mail Rendered) map[string]bool {
	items := codeItemsOf(mail.Subject)
	for item := range codeItemsOf(mail.PlainText) {
		items[item] = true
	}
	for _, text := range visibleHTML(mail.HTML) {
		for item := range codeItemsOf(text) {
			items[item] = true
		}
	}
	return items
}

// maskForms are a mask as the renderers print it (an escaped <mark> in an
// href): not a code.
var maskForms = sync.OnceValue(func() []string {
	var forms []string
	for _, v := range []string{maskMark, Mask} {
		for _, form := range printedForms(v) {
			if form != Mask {
				forms = append(forms, form)
			}
		}
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
})

func codeItemsOf(s string) map[string]bool {
	items := map[string]bool{}
	if s == "" {
		return items
	}
	for _, form := range maskForms() {
		s = strings.ReplaceAll(s, form, Mask)
	}
	for _, ip := range ipv4.FindAllString(s, -1) {
		if isIP(ip) {
			items[ip] = true
		}
	}
	for _, candidate := range ipv6.FindAllString(s, -1) {
		if trimmed := strings.Trim(candidate, ":."); isIP(trimmed) && strings.Contains(trimmed, ":") {
			items[strings.ToLower(trimmed)] = true
		}
	}
	for _, w := range codeWord.FindAllString(s, -1) {
		if tokenShaped(w) || codeShaped(w) {
			items[w] = true
		}
	}
	for _, w := range numberWord.FindAllString(s, -1) {
		if digitRun.MatchString(w) && len(w) >= 4 {
			items[w] = true
		}
	}
	for _, loc := range splitDigits.FindAllStringIndex(s, -1) {
		if standalone(s, loc) {
			if digits := strings.Map(keepDigit, s[loc[0]:loc[1]]); len(digits) >= 4 {
				items[digits] = true
			}
		}
	}
	return items
}

// layoutAttributes say how a mail looks, not what it says: they are not
// masked as text, and not looked into.
var layoutAttributes = map[string]bool{
	"class": true, "id": true, "bgcolor": true, "color": true, "width": true, "height": true, "align": true,
	"valign": true, "cellpadding": true, "cellspacing": true, "border": true, "dir": true, "lang": true,
	"role": true, "type": true, "name": true, "target": true, "rel": true, "xmlns": true, "http-equiv": true,
	"charset": true, "style": true, "face": true, "size": true, "colspan": true, "rowspan": true,
}

// urlAttributes hold an address: in a masked mail each is masked whole but a
// mail address, a phone number or a link within the page.
var urlAttributes = map[string]bool{
	"href": true, "src": true, "action": true, "formaction": true, "background": true, "poster": true,
	"cite": true, "srcset": true, "data": true, "longdesc": true, "usemap": true, "ping": true,
}

// visibleHTML is the HTML's text and the attributes a reader may see.
func visibleHTML(src string) []string {
	var out []string
	tokenizer := html.NewTokenizer(strings.NewReader(src))
	inRaw := ""
	for {
		kind := tokenizer.Next()
		if kind == html.ErrorToken {
			return out
		}
		token := tokenizer.Token()
		switch kind {
		case html.TextToken:
			if inRaw != "style" {
				out = append(out, token.Data)
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			if kind == html.StartTagToken && (token.Data == "style" || token.Data == "script" || token.Data == "title" || token.Data == "textarea") {
				inRaw = token.Data
			}
			for _, attr := range token.Attr {
				if !layoutAttributes[attr.Key] {
					out = append(out, attr.Val)
				}
			}
		case html.EndTagToken:
			if token.Data == inRaw {
				inRaw = ""
			}
		case html.CommentToken:
			out = append(out, token.Data)
		}
	}
}

// standalone reports whether s[loc] stands on its own: no digit, dot, colon,
// slash or comma touching it (13.10.2026 14:20 is a date and a time).
func standalone(s string, loc []int) bool {
	touches := func(r byte) bool { return (r >= '0' && r <= '9') || strings.IndexByte(".:/,", r) >= 0 }
	if loc[0] > 0 && touches(s[loc[0]-1]) {
		return false
	}
	return loc[1] >= len(s) || !touches(s[loc[1]])
}

// maskText masks, in text, every address, IP address, token- and code-shaped
// word, code cut by spaces and run of four or more digits, handing each piece
// it masks to found (when not nil). A number with separators in it — a date,
// a time, an amount — stays.
func maskText(s string, found func(string)) string {
	if s == "" {
		return s
	}
	note := func(piece string) string {
		if found != nil {
			found(piece)
		}
		return Mask
	}
	s = urlInText.ReplaceAllStringFunc(s, note)
	s = ipv4.ReplaceAllStringFunc(s, func(c string) string {
		if isIP(c) {
			return note(c)
		}
		return c
	})
	s = ipv6.ReplaceAllStringFunc(s, func(c string) string {
		trimmed := strings.Trim(c, ":.")
		if !strings.Contains(trimmed, ":") || !isIP(trimmed) {
			return c
		}
		at := strings.Index(c, trimmed)
		return c[:at] + note(trimmed) + c[at+len(trimmed):]
	})
	s = codeWord.ReplaceAllStringFunc(s, func(w string) string {
		if tokenShaped(w) || codeShaped(w) {
			return note(w)
		}
		return w
	})
	var out strings.Builder
	last := 0
	for _, loc := range splitDigits.FindAllStringIndex(s, -1) {
		piece := s[loc[0]:loc[1]]
		if !standalone(s, loc) || len(strings.Map(keepDigit, piece)) < 4 {
			continue
		}
		out.WriteString(s[last:loc[0]])
		out.WriteString(note(piece))
		last = loc[1]
	}
	out.WriteString(s[last:])
	s = out.String()
	return numberWord.ReplaceAllStringFunc(s, func(w string) string {
		if digitRun.MatchString(w) && len(w) >= 4 {
			return note(w)
		}
		return w
	})
}

// cssString is a quoted string in a style sheet; cssNumber a run of digits
// not part of a colour (#FF0000) or followed by a unit (1200px).
var (
	cssString = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	cssNumber = regexp.MustCompile(`[#0-9A-Za-z.]*\d{4,}[A-Za-z%]*`)
)

// maskCSS masks a style sheet or a style attribute: its addresses, the text
// of its quoted strings (content: "…"), and a long number that is no colour
// or size.
func maskCSS(s string) string {
	s = urlInText.ReplaceAllString(s, Mask)
	s = cssString.ReplaceAllStringFunc(s, func(q string) string {
		return q[:1] + maskText(q[1:len(q)-1], nil) + q[len(q)-1:]
	})
	return cssNumber.ReplaceAllStringFunc(s, func(w string) string {
		if digitRun.MatchString(w) {
			return Mask
		}
		return w
	})
}

// maskURLAttribute masks an address in an attribute whole: a mail address, a
// phone number and a link within the page stay.
func maskURLAttribute(v string) string {
	trimmed := strings.ToLower(strings.TrimSpace(v))
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "mailto:") || strings.HasPrefix(trimmed, "tel:") {
		return v
	}
	return Mask
}

// maskHTML masks the HTML's text, comments and attributes as maskText does,
// marking what it masked in the text; every address in an attribute whole;
// style sheets and style attributes as maskCSS does.
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
				out.WriteString(html.EscapeString(maskText(raw, nil)))
			}
			break
		}
		token := tokenizer.Token()
		switch kind {
		case html.TextToken:
			switch inRaw {
			case "style":
				out.WriteString(maskCSS(raw))
			case "":
				out.WriteString(markMasked(maskText(token.Data, nil)))
			default:
				out.WriteString(maskText(raw, nil))
			}
		case html.CommentToken:
			out.WriteString("<!--" + maskText(token.Data, nil) + "-->")
		case html.StartTagToken, html.SelfClosingTagToken:
			if kind == html.StartTagToken {
				switch token.Data {
				case "style", "script", "title", "textarea":
					inRaw = token.Data
				}
			}
			changed := false
			for i, attr := range token.Attr {
				var masked string
				switch {
				case attr.Key == "style":
					masked = maskCSS(attr.Val)
				case urlAttributes[attr.Key]:
					masked = maskURLAttribute(attr.Val)
				case layoutAttributes[attr.Key]:
					masked = attr.Val
				default:
					masked = maskText(attr.Val, nil)
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
