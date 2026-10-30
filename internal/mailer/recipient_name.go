package mailer

import (
	htmlt "html/template"
	"regexp"
	"strings"
	textt "text/template"
	"text/template/parse"
	"unicode"

	"golang.org/x/net/html"
)

// A send's own variables are data, never a template: an operator's free
// announcement prints {{.Email}} as those characters, not as the recipient's
// address (README, "Operatör postaları"). That stays. The one exception is
// the recipient's name, the merge tag every mail tool has (Mailchimp's
// *|FNAME|*, SendGrid's {{first_name}}, HubSpot's personalisation token), so
// a free announcement can greet each person by name (hub ticket
// skymail-serbest-duyuru-alici-adi 01, AlgoLab 2026-10-27).
//
// Exactly two tags are filled, written as they are here and nothing else:
//
//   - {{.FullName}}: the recipient's name;
//   - {{or .FullName "katılımcı"}}: the name, or the text in quotes when the
//     recipient has none (1 to 60 characters, none of " \ { } < > & and no
//     control character) — the default value Mailchimp, HubSpot, Braze and
//     Klaviyo give a merge tag. Without one an empty name prints nothing.
//
// Any other {{…}} — another variable, an action, a tag with spaces or a trim
// marker — is left as the text it is. No template engine runs: the tags are
// found as text and replaced. Go's own reading of the same two tags gives
// the same mail, so a body reads the same to a person who knows templates.
//
// Where: in the send's variables the subject prints (free.basic's Subject,
// which its preheader prints too) and those printed as markup through
// safeHTML (free.basic's BodyHtml). In markup only in text — the name is
// escaped, and never reaches a link's address, so the links every recipient
// gets alike stay alike (linksSendLevel's probes). Only in a template that
// prints markup: a service mail's subject variables are as before. A
// recipient's own variables, the template's own text and every other
// variable are as before.
//
// The name is the one the row already carries (mail_queue.recipient_full_name),
// its runs of white space and control characters made one space. No first
// name is made from it: a row has only the full name, and which part of a
// Turkish name with two given names is the first is not something to guess.
var recipientNameTag = regexp.MustCompile(`\{\{(?:\.FullName|or \.FullName "([^"\\{}<>&\p{Cc}]{1,60})")\}\}`)

// recipientNameMarker is in every tag: a value without it is left alone, as
// it was before the tags were filled.
const recipientNameMarker = ".FullName"

// recipientName is the name a tag prints: the recipient's full name with its
// white space and control characters made single spaces, trimmed.
func recipientName(fullName string) string {
	return strings.Join(strings.FieldsFunc(fullName, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

// HasRecipientName is whether a recipient's full name prints as a name: one
// of only white space and control characters is none, and a tag prints its
// fallback for it. A Forms audience counts its unnamed respondents with it.
func HasRecipientName(fullName string) bool { return recipientName(fullName) != "" }

// fillRecipientName fills the tags in text with name (recipientName), or
// each tag's fallback when name is empty.
func fillRecipientName(text, name string) string {
	if !strings.Contains(text, recipientNameMarker) {
		return text
	}
	return recipientNameTag.ReplaceAllStringFunc(text, func(tag string) string {
		if name != "" {
			return name
		}
		return recipientNameTag.FindStringSubmatch(tag)[1]
	})
}

// fillRecipientNameMarkup fills the tags in markup's text, escaped, after
// narrowing it to the allowlist: what the tags leave is what safeHTML would
// have kept, with the name in the text. An attribute, a link's address
// among them, is never read.
func fillRecipientNameMarkup(markup, name string) string {
	if !strings.Contains(markup, recipientNameMarker) {
		return markup
	}
	return fillRecipientNameInText(sanitizeEmailHTML(markup), name)
}

// fillRecipientNameInText fills the tags in the text of trusted markup —
// the allowlist's output, or a masked mail's markup (mask.go) — escaped,
// and leaves every tag and attribute as it is.
func fillRecipientNameInText(markup, name string) string {
	if !strings.Contains(markup, recipientNameMarker) {
		return markup
	}
	tokens := html.NewTokenizer(strings.NewReader(markup))
	var out strings.Builder
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			return out.String()
		}
		raw := tokens.Raw()
		if kind == html.TextToken {
			text := html.UnescapeString(string(raw))
			if filled := fillRecipientName(text, name); filled != text {
				out.WriteString(html.EscapeString(filled))
				continue
			}
		}
		out.Write(raw)
	}
}

// nameFields are the variables of a template whose values may hold the
// recipient's name tags: those it prints as markup (safeHTML .X) and those
// its subject prints.
type nameFields struct {
	markup map[string]bool
	plain  map[string]bool
}

// nameFieldsOf reads them from a template's subject and HTML, both of which
// have parsed already. A template that prints no markup — a service mail,
// whose subject may print a form's title or what a respondent wrote — has
// none: the tags are a free announcement's (and a news send's).
func nameFieldsOf(subject, html string) nameFields {
	fields := nameFields{markup: map[string]bool{}, plain: map[string]bool{}}
	if tmpl, err := textt.New("html").Funcs(htmlFuncs).Parse(html); err == nil && tmpl.Tree != nil {
		markupFields(tmpl.Tree.Root, fields.markup)
	}
	if len(fields.markup) == 0 {
		return fields
	}
	if names, err := ReferencedVariables(subject); err == nil {
		for _, name := range names {
			if !fields.markup[name] {
				fields.plain[name] = true
			}
		}
	}
	return fields
}

// markupFields finds safeHTML .X and safeHTML $.X where dot is the mailer's
// data: outside range and with, and not in a define.
func markupFields(node parse.Node, found map[string]bool) {
	switch n := node.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, child := range n.Nodes {
			markupFields(child, found)
		}
	case *parse.ActionNode:
		markupFields(n.Pipe, found)
	case *parse.IfNode:
		markupFields(n.Pipe, found)
		markupFields(n.List, found)
		markupFields(n.ElseList, found)
	case *parse.PipeNode:
		if n == nil {
			return
		}
		for _, command := range n.Cmds {
			markupFields(command, found)
		}
	case *parse.CommandNode:
		if len(n.Args) == 2 {
			if fn, ok := n.Args[0].(*parse.IdentifierNode); ok && fn.Ident == "safeHTML" {
				switch arg := n.Args[1].(type) {
				case *parse.FieldNode:
					if len(arg.Ident) == 1 {
						found[arg.Ident[0]] = true
					}
				case *parse.VariableNode:
					if len(arg.Ident) == 2 && arg.Ident[0] == "$" {
						found[arg.Ident[1]] = true
					}
				}
			}
		}
		for _, arg := range n.Args {
			if pipe, ok := arg.(*parse.PipeNode); ok {
				markupFields(pipe, found)
			}
		}
	}
}

// withName is a send variable's value with the recipient's name filled in,
// when the template takes the tags in it. A masked mail's markup values
// (htmlt.HTML, mask.go) are filled in their text.
func (f nameFields) withName(key string, value any, name string) any {
	if !f.markup[key] && !f.plain[key] {
		return value
	}
	switch v := value.(type) {
	case htmlt.HTML:
		return htmlt.HTML(fillRecipientNameInText(string(v), name))
	case string:
		if f.markup[key] {
			return fillRecipientNameMarkup(v, name)
		}
		return fillRecipientName(v, name)
	}
	return value
}
