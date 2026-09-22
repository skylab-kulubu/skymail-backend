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

// ErrBodyUnparseable refuses a body the mailer could not parse: it could not
// be sent at all, and what it references cannot be said. params.error is the
// parser's message, with the line it stopped at.
var ErrBodyUnparseable = apperrors.New(
	"template.body_unparseable",
	"The HTML body is not a Go template the mailer can parse.",
	fiber.StatusUnprocessableEntity,
)

// ErrMissing refuses a body that no longer references every Required
// variable. params.missing lists each one it lacks, by name, with the set it
// is in — which is why it is required.
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

// Missing is a Required variable a body does not reference.
type Missing struct {
	Name   string `json:"name"`
	Source Source `json:"source" enums:"contract,operator"`
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
// It returns nil, ErrBodyUnparseable with params {"error"}, or ErrMissing with
// params {"missing": [Missing…]} sorted by name. Both are 422s.
func CheckBody(template database.Template, html string) error {
	referenced, err := mailer.ReferencedVariables(html)
	if err != nil {
		return ErrBodyUnparseable.WithParams(map[string]interface{}{"error": err.Error()})
	}

	has := make(map[string]bool, len(referenced))
	for _, name := range referenced {
		has[name] = true
	}
	var missing []Missing
	for _, required := range []struct {
		names  []string
		source Source
	}{
		{template.ContractRequiredVariables, SourceContract},
		{template.OperatorRequiredVariables, SourceOperator},
	} {
		for _, name := range required.names {
			if !has[name] {
				missing = append(missing, Missing{Name: name, Source: required.source})
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].Name < missing[j].Name })
	return ErrMissing.WithParams(map[string]interface{}{"missing": missing})
}
