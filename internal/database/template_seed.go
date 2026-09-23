package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/ptr"
)

// SeedConflictRule is one way an operator can have changed a template since
// the Template seed last wrote it. Any of them refuses the seed unless it is
// forced (ADR-0047).
type SeedConflictRule string

const (
	// The version the template sends is not the last one a seed wrote: an
	// operator published since — a draft, a forced publish, an edit in the old
	// panel — or no seed has ever written the template.
	SeedConflictPublishedByOperator SeedConflictRule = "published_by_operator"
	// An operator wrote a version after the last seed — a draft counts, a
	// discarded one does not.
	SeedConflictNewerOperatorVersion SeedConflictRule = "newer_operator_version"
	// The subject sent now is an operator's, and the seed would overwrite it:
	// it is neither the one the last seed asked for (that seed kept it, or an
	// operator published it since) nor the one asked for now.
	SeedConflictOperatorSubject SeedConflictRule = "operator_subject"
)

// SeedConflict is how an operator changed a template since the Template seed
// last wrote it. As an error it refuses a seed, and nothing was written; a
// forced seed returns it as what it wrote over.
type SeedConflict struct {
	TemplateID uuid.UUID
	Key        string
	// Every rule that holds, in the order the rules are listed above.
	Rules []SeedConflictRule
	// The version the template sent when the seed came; nil when it had none.
	PublishedVersion *TemplateVersionSummary
	// The last version a seed wrote; nil when no seed has.
	LastSeedVersion *TemplateVersionSummary
	// The operator's versions after the last seed, oldest first, discarded
	// drafts left out.
	OperatorVersions []TemplateVersionSummary
	// The subject the template sends now, and the one the seed sent.
	Subject, RequestedSubject string
}

func (e *SeedConflict) Error() string {
	return fmt.Sprintf("the Template seed of %s is refused: %v", e.Key, e.Rules)
}

// TemplateSeed is one template the Template seed writes.
type TemplateSeed struct {
	Key    string
	Author VersionAuthor
	// The subject the seed sent.
	Subject string
	// Write even over an operator's change. Their versions stay in the history.
	Force bool
	// A hash of everything the seed asked to write, kept with a refusal to
	// tell the same content refused again from other content.
	PayloadSHA256 string
}

// SeedTemplate writes a template the Template seed sent, by key, with write —
// the by-key upsert — and records what the row then holds as a published
// Template seed version, the subject the seed asked for beside it, in one
// transaction, as PublishTemplateWrite does for the old panel.
//
// Unless seed.Force, a template an operator changed since the last seed is
// not written: the error is a *SeedConflict naming what the operator did, and
// the refusal is kept on the template (RecordSeedRefusal). Forced, the seed is
// written anyway, and overrode is that *SeedConflict — what it wrote over,
// with the versions read again as they are after the write. A seed that
// leaves the template as its published version already is — it would record
// no version — overwrites nothing: it is never refused and overrides nothing.
// A seed that goes through clears a refusal kept before it.
func (s *Store) SeedTemplate(ctx context.Context, seed TemplateSeed, write func(*Queries) (Template, error)) (seeded Template, overrode *SeedConflict, err error) {
	var refused *SeedConflict
	err = pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		q := s.WithTx(tx)
		var conflict *SeedConflict
		existing, err := q.LockTemplateByKey(ctx, &seed.Key)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if conflict, err = seedConflict(ctx, q, existing, seed); err != nil {
				return err
			}
		}

		// Whether the seed changes anything a version holds is known once the
		// row is written — the version is built from the row, as for every
		// writer — so the write goes in a savepoint, undone when it would
		// overwrite an operator's change unforced.
		err = pgx.BeginFunc(ctx, tx, func(savepoint pgx.Tx) error {
			q := s.WithTx(savepoint)
			var recorded bool
			var err error
			seeded, recorded, err = recordWrite(ctx, q, seed.Author, &seed.Subject, write)
			if err != nil || !recorded || conflict == nil {
				return err
			}
			if !seed.Force {
				return conflict
			}
			overrode = conflict
			return conflict.reread(ctx, q)
		})
		if !errors.As(err, &refused) {
			return err
		}
		rules := make([]string, len(refused.Rules))
		for i, rule := range refused.Rules {
			rules[i] = string(rule)
		}
		return q.RecordSeedRefusal(ctx, RecordSeedRefusalParams{ID: existing.ID, Rules: rules, PayloadSha256: seed.PayloadSHA256})
	})
	if err != nil {
		return Template{}, nil, err
	}
	if refused != nil {
		return Template{}, nil, refused
	}
	return seeded, overrode, nil
}

// reread reads the versions a conflict names again, as they are now: after a
// forced seed, the version that was sent is no longer current.
func (c *SeedConflict) reread(ctx context.Context, q *Queries) error {
	for _, version := range []**TemplateVersionSummary{&c.PublishedVersion, &c.LastSeedVersion} {
		if *version == nil {
			continue
		}
		again, err := q.GetTemplateVersionSummary(ctx, GetTemplateVersionSummaryParams{TemplateID: c.TemplateID, ID: (*version).ID})
		if err != nil {
			return err
		}
		*version = &again
	}
	after := 0
	if c.LastSeedVersion != nil {
		after = c.LastSeedVersion.Seq
	}
	var err error
	c.OperatorVersions, err = q.ListOperatorVersionsAfter(ctx, ListOperatorVersionsAfterParams{TemplateID: c.TemplateID, AfterSeq: after})
	return err
}

// seedConflict is how an operator changed a locked template since the last
// seed, or nil when they have not.
func seedConflict(ctx context.Context, q *Queries, template Template, seed TemplateSeed) (*SeedConflict, error) {
	conflict := &SeedConflict{TemplateID: template.ID, Key: seed.Key, Subject: template.Subject, RequestedSubject: seed.Subject}

	lastSeed, err := q.LastTemplateSeedVersion(ctx, template.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		conflict.LastSeedVersion = &lastSeed
	}
	if template.PublishedVersionID != nil {
		published, err := q.GetTemplateVersionSummary(ctx, GetTemplateVersionSummaryParams{TemplateID: template.ID, ID: *template.PublishedVersionID})
		if err != nil {
			return nil, err
		}
		conflict.PublishedVersion = &published
	}
	after := 0
	if conflict.LastSeedVersion != nil {
		after = conflict.LastSeedVersion.Seq
	}
	conflict.OperatorVersions, err = q.ListOperatorVersionsAfter(ctx, ListOperatorVersionsAfterParams{TemplateID: template.ID, AfterSeq: after})
	if err != nil {
		return nil, err
	}

	if !ptr.Equal(template.PublishedVersionID, summaryID(conflict.LastSeedVersion)) {
		conflict.Rules = append(conflict.Rules, SeedConflictPublishedByOperator)
	}
	if len(conflict.OperatorVersions) > 0 {
		conflict.Rules = append(conflict.Rules, SeedConflictNewerOperatorVersion)
	}
	if keptAnOperatorsSubject(conflict.LastSeedVersion, template.Subject, seed.Subject) {
		conflict.Rules = append(conflict.Rules, SeedConflictOperatorSubject)
	}
	if len(conflict.Rules) == 0 {
		return nil, nil
	}
	return conflict, nil
}

// summaryID is a version's id, or nil for no version.
func summaryID(version *TemplateVersionSummary) *uuid.UUID {
	if version == nil {
		return nil
	}
	return &version.ID
}

// keptAnOperatorsSubject is whether the subject a template sends is an
// operator's that the seed now asking for requested would overwrite: the last
// seed version asked for another subject than the one sent — it kept the
// template's own, as the upsert did before this rule (#18) — and requested is
// not that one either. A repo that has taken up the operator's subject leaves
// nothing of theirs to overwrite.
//
// The migration's first versions do not know what the seed asked for
// (requested_subject is null), and their subject may be one #18 kept. For
// them, a seed asking for another subject than the one sent counts: refusing a
// subject the repo changed costs a force, while writing over an operator's
// wording is what the rule exists to prevent.
func keptAnOperatorsSubject(lastSeed *TemplateVersionSummary, sent, requested string) bool {
	if lastSeed == nil || sent == requested {
		return false
	}
	return lastSeed.RequestedSubject == nil || sent != *lastSeed.RequestedSubject
}
