// Package requiredvars holds the one rule every write of a Mail template's
// body obeys (ADR-0046): the body references each of the template's Required
// variables. The rule is enforced by the server, where every writer — the old
// panel, the Template seed, the editor's draft and publish — passes through.
package requiredvars

import (
	"sort"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// ErrUnparseable refuses a subject or an HTML body the mailer could not
// parse: the mailer parses both before every send, so the mail could not be
// sent at all, and what a body references cannot be said. params.part is
// "subject" or "html"; params.error is the parser's message, with the line it
// stopped at.
var ErrUnparseable = apperrors.New(
	"template.unparseable",
	"The subject or the HTML body is not a Go template the mailer can parse.",
	fiber.StatusUnprocessableEntity,
)

// The parts of a version ErrUnparseable names.
const (
	PartSubject = "subject"
	PartHTML    = "html"
)

// ErrMissing refuses a body that no longer references every Required
// variable. params.missing lists each one it lacks, by name, with why it is
// required: the set it is in and, for a contract one, the repo's reason.
var ErrMissing = apperrors.New(
	"template.required_variables_missing",
	"The HTML body does not reference every Required variable of the template.",
	fiber.StatusUnprocessableEntity,
)

// Source is why a variable is required.
type Source string

const (
	// The sending service's contract, declared in the repo and written by the
	// Template seed; the panel cannot release it.
	SourceContract Source = "contract"
	// An operator marked it; an operator can release it.
	SourceOperator Source = "operator"
)

// Missing is a Required variable a body does not reference, and why it is
// required: the set it is in and, for a contract one, the repo's reason.
type Missing struct {
	Name   string `json:"name"`
	Source Source `json:"source" enums:"contract,operator"`
	// Why the mail cannot do without it: the contract's reason; null for an operator's variable, and for a contract one the seed sent without one.
	Reason *string `json:"reason"`
}

// CheckBody reports whether html may become the body of template: it parses
// as a Go template the way the mailer parses it, and references every one of
// the template's Required variables by the rule of mailer.ReferencedVariables
// — inside a conditional section counts, a comment or plain text does not.
//
// template supplies the Required variables, both sets; html is the version's
// rendered HTML body, the one being saved or published. Call it inside the
// transaction that writes, after the template row is locked (the write itself
// locks it), and return its error from there so that nothing is written: the
// sets cannot then change between the check and the write.
//
// It returns nil, ErrUnparseable with params {"part": "html", "error"}, or
// ErrMissing with params {"missing": [{name, source, reason}…]} sorted by
// name. Both are 422s.
// A version's subject is checked by CheckSubject; a write calls both.
func CheckBody(template database.Template, html string) error {
	referenced, err := mailer.ReferencedVariables(html)
	if err != nil {
		return unparseable(PartHTML, err)
	}

	has := make(map[string]bool, len(referenced))
	for _, name := range referenced {
		has[name] = true
	}
	var missing []Missing
	for _, variable := range template.ContractRequiredVariables {
		if !has[variable.Name] {
			missing = append(missing, Missing{Name: variable.Name, Source: SourceContract, Reason: variable.Reason})
		}
	}
	for _, name := range template.OperatorRequiredVariables {
		if !has[name] {
			missing = append(missing, Missing{Name: name, Source: SourceOperator})
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// Byte by byte, the order the sets are kept in.
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
	return ErrMissing.WithParams(map[string]interface{}{"missing": missing})
}

// CheckSubject reports whether subject may become a version's subject: it
// parses the way the mailer parses a subject before every send. It returns
// nil or ErrUnparseable with params {"part": "subject", "error"}. Like
// CheckBody, call it inside the writing transaction and return its error.
func CheckSubject(subject string) error {
	if err := mailer.ParseSubject(subject); err != nil {
		return unparseable(PartSubject, err)
	}
	return nil
}

func unparseable(part string, err error) error {
	return ErrUnparseable.WithParams(map[string]interface{}{"part": part, "error": err.Error()})
}
