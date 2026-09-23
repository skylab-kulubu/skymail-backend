package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
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
	Rules []string `json:"rules" enums:"published_by_operator,newer_operator_version,operator_subject"`
	// A SHA-256 of what the seed asked to write, in hex: the same content refused again has the same one.
	PayloadSHA256 string `json:"payload_sha256"`
}

// seedRefusal is the refused seed a template row keeps, or nil.
func seedRefusal(row database.Template) *SeedRefusal {
	if row.SeedRefusedAt == nil || row.SeedRefusedPayloadSha256 == nil {
		return nil
	}
	return &SeedRefusal{RefusedAt: *row.SeedRefusedAt, Rules: row.SeedRefusedRules, PayloadSHA256: *row.SeedRefusedPayloadSha256}
}

// seedPayloadSHA256 is a SHA-256 of what a seed asks to write, in hex: every
// field of the payload, the contract set as the database would keep it, in a
// fixed order. The same content hashes the same whatever the JSON looked like.
func seedPayloadSHA256(key string, params requests.UpsertTemplateByKey, contract []byte) string {
	canonical, _ := json.Marshal(struct {
		Key               string          `json:"key"`
		Name              string          `json:"name"`
		Subject           string          `json:"subject"`
		HTMLContent       string          `json:"html_content"`
		PlainTextContent  string          `json:"plain_text_content"`
		ReactEmailContent string          `json:"react_email_content"`
		System            bool            `json:"system"`
		Contract          json.RawMessage `json:"contract_required_variables"`
	}{key, params.Name, params.Subject, params.HTMLContent, params.PlainTextContent, params.ReactEmailContent, params.System, contract})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// SeedConflictVersion is a version a refused seed names.
type SeedConflictVersion struct {
	ID  uuid.UUID `json:"id"`
	Seq int       `json:"seq"`
	// Who wrote it.
	Author database.VersionAuthor `json:"author"`
	// When it was published; null for a draft.
	PublishedAt *time.Time `json:"published_at"`
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

// seedError is the API error for what refused a seed, or err itself.
func seedError(err error) error {
	var conflict *database.SeedConflictError
	if !errors.As(err, &conflict) {
		return err
	}
	operator := make([]SeedConflictVersion, 0, len(conflict.OperatorVersions))
	for _, version := range conflict.OperatorVersions {
		operator = append(operator, *seedConflictVersion(&version))
	}
	return errSeedConflict.WithParams(map[string]interface{}{
		"key":               conflict.Key,
		"template_id":       conflict.TemplateID,
		"rules":             conflict.Rules,
		"published_version": seedConflictVersion(conflict.PublishedVersion),
		"last_seed_version": seedConflictVersion(conflict.LastSeedVersion),
		"operator_versions": operator,
		"subject":           conflict.Subject,
		"requested_subject": conflict.RequestedSubject,
	})
}

func seedConflictVersion(version *database.TemplateVersionSummary) *SeedConflictVersion {
	if version == nil {
		return nil
	}
	return &SeedConflictVersion{ID: version.ID, Seq: version.Seq, Author: version.Author(), PublishedAt: version.PublishedAt}
}
