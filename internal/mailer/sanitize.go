package mailer

import (
	"sync"

	"github.com/microcosm-cc/bluemonday"
)

// The free-form template lets a club admin write the body themselves, so that
// body is the one place a variable carries markup instead of plain text. Rather
// than trusting whatever arrives in body_variables, we narrow it to the tags an
// e-mail actually needs. Everything outside this list is dropped, so a template
// cannot become a way to inject markup into someone's inbox.
var emailPolicy = sync.OnceValue(func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()

	p.AllowElements("p", "br", "strong", "b", "em", "i", "u", "ul", "ol", "li", "h2", "h3", "blockquote")
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("http", "https", "mailto")
	p.RequireParseableURLs(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)

	return p
})

// sanitizeEmailHTML narrows rich text to the allowlist above. It is applied at
// render time as well as before storage, so an older row written before the
// allowlist existed is still narrowed on its way out.
func sanitizeEmailHTML(raw string) string {
	return emailPolicy().Sanitize(raw)
}
