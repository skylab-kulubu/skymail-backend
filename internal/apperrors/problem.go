package apperrors

import (
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"
)

// ProblemContentType is the media type of an error's body (RFC 9457 §3).
const ProblemContentType = "application/problem+json"

// Problem is an error's body: an RFC 9457 problem details object (ADR-0010,
// as core-backend writes it) whose extension members (§3.2) are the body
// SkyNotify answered before — code, message, params — unchanged, so a client
// that reads code reads it as before.
//
// The members are written code and message first: a caller that prints only
// the head of a refusal (a Template seed's log line) still finds them. Core's
// diagnostic read cuts the body at 2 KiB and cannot parse a cut JSON; on a
// whole body it reads message, which stays. RFC 9457 gives the order no meaning.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Type is about:blank: the problem is what the status says (§4.2.1);
	// what SkyNotify means is code.
	Type string `json:"type"`
	// Title is the status's reason phrase (§4.2.1).
	Title  string `json:"title"`
	Status int    `json:"status"`
	// Detail is the message, for a person.
	Detail string `json:"detail"`
	// Instance is the request's path, without its query.
	Instance string                 `json:"instance,omitempty"`
	Params   map[string]interface{} `json:"params,omitempty"`
}

// ProblemOf is e answered with status at instance.
func ProblemOf(e *AppError, status int, instance string) Problem {
	return Problem{
		Type:     "about:blank",
		Title:    http.StatusText(status),
		Status:   status,
		Detail:   e.Message,
		Instance: instance,
		Code:     e.Code,
		Message:  e.Message,
		Params:   e.Params,
	}
}

// WriteProblem answers e with status as application/problem+json.
func WriteProblem(c fiber.Ctx, status int, e *AppError) error {
	raw, err := json.Marshal(ProblemOf(e, status, c.Path()))
	if err != nil {
		return err
	}
	c.Set(fiber.HeaderContentType, ProblemContentType)
	return c.Status(status).Send(raw)
}
