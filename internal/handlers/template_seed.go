package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// errSeedConflict refuses a Template seed that would overwrite an operator's
// change (ADR-0047). Its params name the template, the rules that hold and the
// versions involved.
var errSeedConflict = apperrors.New(
	"template.seed_conflict",
	"An operator changed this template since the last Template seed, so nothing was written. To overwrite it, seed with ?force=true.",
	fiber.StatusConflict,
)

// SeedRefusal is a Template seed refused because an operator changed the
// template, as the template keeps it until a seed goes through.
type SeedRefusal struct {
	// When the Template seed was first refused with the content it asked for last; refused again with the same content, the time stays.
	RefusedAt time.Time `json:"refused_at"`
	// The conflict rules that held the last time: published_by_operator (the version sent is an operator's, not the last seed's), newer_operator_version (an operator wrote a version after the last seed, a draft included), operator_subject (the subject sent is an operator's).
	Rules []database.SeedConflictRule `json:"rules"`
	// A SHA-256 of what the seed asked to write, in hex: the same content refused again has the same one.
	PayloadSHA256 string `json:"payload_sha256"`
}

// seedRefusal is the refused seed a template row keeps, or nil.
func seedRefusal(row database.Template) *SeedRefusal {
	if row.SeedRefusedAt == nil || row.SeedRefusedPayloadSha256 == nil {
		return nil
	}
	rules := make([]database.SeedConflictRule, len(row.SeedRefusedRules))
	for i, rule := range row.SeedRefusedRules {
		rules[i] = database.SeedConflictRule(rule)
	}
	return &SeedRefusal{RefusedAt: *row.SeedRefusedAt, Rules: rules, PayloadSHA256: *row.SeedRefusedPayloadSha256}
}

// seedPayloadSHA256 is a SHA-256 of what a seed asks to write, in hex: every
// field of the payload, the contract set as the database would keep it, in a
// fixed order. The same content hashes the same whatever the JSON looked like.
func seedPayloadSHA256(key string, params requests.UpsertTemplateByKey, contract []byte) (string, error) {
	canonical, err := json.Marshal(struct {
		Key               string          `json:"key"`
		Name              string          `json:"name"`
		Subject           string          `json:"subject"`
		HTMLContent       string          `json:"html_content"`
		PlainTextContent  string          `json:"plain_text_content"`
		ReactEmailContent string          `json:"react_email_content"`
		System            bool            `json:"system"`
		Contract          json.RawMessage `json:"contract_required_variables"`
	}{key, params.Name, params.Subject, params.HTMLContent, params.PlainTextContent, params.ReactEmailContent, params.System, contract})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// SeededTemplate is a template as the Template seed's upsert answers with it:
// what the other template routes serve, and what a forced seed wrote over.
type SeededTemplate struct {
	Template
	// What a forced seed wrote over: the rules that held, the version that was sent and the operator's versions after the last seed — what an unforced seed's 409 names. Left out when the seed overrode nothing, forced or not.
	Overrode *SeedOverride `json:"overrode,omitempty"`
}

// SeedOverride is what a forced Template seed wrote over. The operator's
// versions stay in the history and can be restored.
type SeedOverride struct {
	// The conflict rules that held, as template.seed_conflict names them.
	Rules []database.SeedConflictRule `json:"rules"`
	// The version the template sent before the seed; null when it had none.
	PublishedVersion *TemplateVersionSummary `json:"published_version"`
	// The operator's versions after the last seed, oldest first, discarded drafts left out.
	OperatorVersions []TemplateVersionSummary `json:"operator_versions"`
}

// summaries is versions as the version routes serve them.
func summaries(versions []database.TemplateVersionSummary) []TemplateVersionSummary {
	served := make([]TemplateVersionSummary, 0, len(versions))
	for _, version := range versions {
		served = append(served, versionSummary(version))
	}
	return served
}

// optionalSummary is a version as the version routes serve it, or nil.
func optionalSummary(version *database.TemplateVersionSummary) *TemplateVersionSummary {
	if version == nil {
		return nil
	}
	served := versionSummary(*version)
	return &served
}

// seedOverride is what a forced seed wrote over, or nil when nothing.
func seedOverride(overrode *database.SeedConflict) *SeedOverride {
	if overrode == nil {
		return nil
	}
	return &SeedOverride{
		Rules:            overrode.Rules,
		PublishedVersion: optionalSummary(overrode.PublishedVersion),
		OperatorVersions: summaries(overrode.OperatorVersions),
	}
}

// seedForce reads the force query parameter of the seed's upsert: true writes
// over an operator's change. Left out, it is false; anything that is not a
// boolean is the caller's mistake.
func seedForce(c fiber.Ctx) (bool, error) {
	raw := c.Query("force")
	if raw == "" {
		return false, nil
	}
	force, err := strconv.ParseBool(raw)
	if err != nil {
		return false, apperrors.ErrValidation.WithParams(map[string]interface{}{
			"errors": []validator.FieldError{{Field: "force", Code: "invalid"}},
		})
	}
	return force, nil
}

// seedError is the API error for what refused a seed, or err itself. Its
// params name the versions the way the version routes serve them.
func seedError(err error) error {
	var conflict *database.SeedConflict
	if !errors.As(err, &conflict) {
		return err
	}
	return errSeedConflict.WithParams(map[string]interface{}{
		"key":               conflict.Key,
		"template_id":       conflict.TemplateID,
		"rules":             conflict.Rules,
		"published_version": optionalSummary(conflict.PublishedVersion),
		"last_seed_version": optionalSummary(conflict.LastSeedVersion),
		"operator_versions": summaries(conflict.OperatorVersions),
		"subject":           conflict.Subject,
		"requested_subject": conflict.RequestedSubject,
	})
}
