package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/ptr"
)

// VersionContent is what a Mail template version holds apart from who wrote
// it and when: its subject, at most one source per Authoring mode, which of
// them is the Main source, and that source's render as HTML and plain text.
type VersionContent struct {
	Subject          string
	JSXSource        *string
	VisualSource     []byte
	HTMLSource       *string
	MainMode         AuthoringMode
	HTMLContent      string
	PlainTextContent string
}

// VersionCheck judges content about to be saved as a draft or published. It
// runs inside the write's transaction with the template row locked, right
// before the write, and an error it returns refuses the write.
type VersionCheck func(ctx context.Context, q *Queries, template Template, content VersionContent) error

var (
	// ErrInvalidBase refuses a draft whose base is not a published version of
	// its template, or that names none when the template has one: a draft
	// starts from what was published.
	ErrInvalidBase = errors.New("the base is not a published version of this template")
	// ErrNotADraft refuses to publish a version that was published before and
	// has been replaced since: going back to it is restoring it as a draft.
	ErrNotADraft = errors.New("the version is not a draft")
	// ErrDraftDiscarded refuses to publish a draft its operator gave up on:
	// using it again is restoring it as a new draft.
	ErrDraftDiscarded = errors.New("the draft was discarded")
	// ErrNotJSXSource refuses a JSX source with nothing but whitespace and
	// comments in it — nothing in it renders. Stored, it would also not come
	// back through the old panel's react_email_content as the same source.
	ErrNotJSXSource = errors.New("the JSX source has no code in it")
)

// MainSourceMissingError refuses a version whose Main source's Authoring mode
// holds no source, even after the save kept the sources it left out.
type MainSourceMissingError struct {
	MainMode AuthoringMode
}

func (e *MainSourceMissingError) Error() string {
	return fmt.Sprintf("the Main source is %s, and the version holds no %s source", e.MainMode, e.MainMode)
}

// StaleBaseError refuses to publish a draft that started from a version other
// than the one published now: someone published after the draft was started,
// and publishing the draft would quietly revert their version. Publishing it
// takes force.
type StaleBaseError struct {
	// The draft that was to be published.
	VersionID uuid.UUID
	// The published version the draft started from; nil when it started from none.
	BaseVersionID *uuid.UUID
	// The version the template sends now; nil when it has none.
	PublishedVersionID *uuid.UUID
}

func (e *StaleBaseError) Error() string {
	return fmt.Sprintf("draft %s started from %s, but %s is published now", e.VersionID, uuidText(e.BaseVersionID), uuidText(e.PublishedVersionID))
}

// SaveTemplateDraft records content as an operator's draft of a template. The
// template row, which is what is sent, does not change.
//
// base is the published version the draft started from: a published version
// of this template, or nil only when the template has none (ErrInvalidBase).
// The draft continues author's draft in progress when that started from the
// same base, or else starts from the base; sources content leaves out are kept
// from the version it continues. A save that would repeat that version records
// nothing. It returns the draft, or the version it repeated, and whether it was
// written.
func (s *Store) SaveTemplateDraft(ctx context.Context, templateID uuid.UUID, author VersionAuthor, base *uuid.UUID, content VersionContent, check VersionCheck) (GetTemplateVersionRow, bool, error) {
	var saved GetTemplateVersionRow
	var created bool
	err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		q := s.WithTx(tx)
		template, err := q.LockTemplate(ctx, templateID)
		if err != nil {
			return err
		}
		baseVersion, err := publishedBase(ctx, q, template, base)
		if err != nil {
			return err
		}
		continued, err := continuedVersion(ctx, q, template.ID, author, baseVersion)
		if err != nil {
			return err
		}
		saved, created, err = writeDraft(ctx, q, template, author, baseVersion, continued, content.keeping(continued), check)
		return err
	})
	return saved, created, err
}

// RestoreTemplateVersion records a copy of any version of a template — its
// subject, sources, Main source and render — as an operator's draft started
// from the version published now, once check has passed it as it would a
// saved draft. The template row does not change. A copy that would repeat the
// version it continues records nothing.
//
// It returns the draft, or the version it repeated, and whether it was
// written.
func (s *Store) RestoreTemplateVersion(ctx context.Context, templateID, versionID uuid.UUID, author VersionAuthor, check VersionCheck) (GetTemplateVersionRow, bool, error) {
	var restored GetTemplateVersionRow
	var created bool
	err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		q := s.WithTx(tx)
		template, err := q.LockTemplate(ctx, templateID)
		if err != nil {
			return err
		}
		version, err := q.GetTemplateVersion(ctx, GetTemplateVersionParams{TemplateID: template.ID, ID: versionID})
		if err != nil {
			return err
		}
		current, err := publishedBase(ctx, q, template, template.PublishedVersionID)
		if err != nil {
			return err
		}
		continued, err := continuedVersion(ctx, q, template.ID, author, current)
		if err != nil {
			return err
		}
		restored, created, err = writeDraft(ctx, q, template, author, current, continued, contentOf(version), check)
		return err
	})
	return restored, created, err
}

// PublishTemplateDraft publishes a draft of a template: the draft is marked
// published and copied onto the template row, so it is what the template sends
// from then on. It returns the row as publishing left it.
//
// A draft is stale when its base is not the version published now. A stale
// draft is published only over the version the operator was shown and chose
// to replace, over; nil, or any other version — one published since they
// looked — is a *StaleBaseError naming what is published now, and nothing
// changes. The version the template sends already is published again as a
// repeat that changes nothing; any other published version is not a draft,
// ErrNotADraft.
func (s *Store) PublishTemplateDraft(ctx context.Context, templateID, versionID uuid.UUID, over *uuid.UUID, check VersionCheck) (Template, error) {
	var published Template
	err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		q := s.WithTx(tx)
		template, err := q.LockTemplate(ctx, templateID)
		if err != nil {
			return err
		}
		version, err := q.GetTemplateVersion(ctx, GetTemplateVersionParams{TemplateID: template.ID, ID: versionID})
		if err != nil {
			return err
		}
		if version.TemplateVersionSummary.PublishedAt != nil {
			// Publishing what is sent already is a repeat, and safe.
			if version.TemplateVersionSummary.IsCurrent {
				published = template
				return nil
			}
			return ErrNotADraft
		}
		if version.TemplateVersionSummary.DiscardedAt != nil {
			return ErrDraftDiscarded
		}
		base := version.TemplateVersionSummary.BaseVersionID
		if !ptr.Equal(base, template.PublishedVersionID) && (over == nil || !ptr.Equal(over, template.PublishedVersionID)) {
			return &StaleBaseError{VersionID: versionID, BaseVersionID: base, PublishedVersionID: template.PublishedVersionID}
		}
		if check != nil {
			if err := check(ctx, q, template, contentOf(version)); err != nil {
				return err
			}
		}
		published, err = q.PublishTemplateDraft(ctx, PublishTemplateDraftParams{VersionID: version.TemplateVersionSummary.ID, TemplateID: template.ID})
		return err
	})
	return published, err
}

// DiscardTemplateDraft discards a draft of a template: it stays in the
// history, readable and restorable, but it is nobody's draft in progress any
// more and it is never published. Discarding a discarded draft changes
// nothing; a published version is not a draft, ErrNotADraft. It returns the
// version as discarding left it.
func (s *Store) DiscardTemplateDraft(ctx context.Context, templateID, versionID uuid.UUID) (GetTemplateVersionRow, error) {
	var discarded GetTemplateVersionRow
	err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		q := s.WithTx(tx)
		template, err := q.LockTemplate(ctx, templateID)
		if err != nil {
			return err
		}
		version, err := q.GetTemplateVersion(ctx, GetTemplateVersionParams{TemplateID: template.ID, ID: versionID})
		if err != nil {
			return err
		}
		if version.TemplateVersionSummary.PublishedAt != nil {
			return ErrNotADraft
		}
		if err := q.DiscardTemplateDraft(ctx, DiscardTemplateDraftParams{TemplateID: template.ID, ID: versionID}); err != nil {
			return err
		}
		discarded, err = q.GetTemplateVersion(ctx, GetTemplateVersionParams{TemplateID: template.ID, ID: versionID})
		return err
	})
	return discarded, err
}

// publishedBase is the version a draft started from, read once: a published
// version of the template, or nil only when the template has none. Anything
// else is ErrInvalidBase.
func publishedBase(ctx context.Context, q *Queries, template Template, base *uuid.UUID) (*GetTemplateVersionRow, error) {
	if base == nil {
		if template.PublishedVersionID != nil {
			return nil, ErrInvalidBase
		}
		return nil, nil
	}
	version, err := q.GetTemplateVersion(ctx, GetTemplateVersionParams{TemplateID: template.ID, ID: *base})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && version.TemplateVersionSummary.PublishedAt == nil) {
		return nil, ErrInvalidBase
	}
	if err != nil {
		return nil, err
	}
	return &version, nil
}

// continuedVersion is the version author's next draft of a template continues:
// their draft in progress when it started from base, or else base itself.
func continuedVersion(ctx context.Context, q *Queries, templateID uuid.UUID, author VersionAuthor, base *GetTemplateVersionRow) (*GetTemplateVersionRow, error) {
	draft, err := draftInProgress(ctx, q, templateID, author)
	if err != nil {
		return nil, err
	}
	if draft != nil && ptr.Equal(draft.TemplateVersionSummary.BaseVersionID, idOf(base)) {
		return draft, nil
	}
	return base, nil
}

// writeDraft writes content as author's draft of a locked template, started
// from base, once its sources hold up and check has passed it — unless
// continued holds exactly that content already: then nothing is written and
// continued is returned. It reports whether it wrote the draft.
func writeDraft(ctx context.Context, q *Queries, template Template, author VersionAuthor, base, continued *GetTemplateVersionRow, content VersionContent, check VersionCheck) (GetTemplateVersionRow, bool, error) {
	if err := content.checkSources(ctx, q); err != nil {
		return GetTemplateVersionRow{}, false, err
	}
	if check != nil {
		if err := check(ctx, q, template, content); err != nil {
			return GetTemplateVersionRow{}, false, err
		}
	}
	recorded, err := q.RecordTemplateDraft(ctx, RecordTemplateDraftParams{
		TemplateID:       template.ID,
		ContinuedID:      idOf(continued),
		Subject:          content.Subject,
		JsxSource:        content.JSXSource,
		VisualSource:     content.VisualSource,
		HtmlSource:       content.HTMLSource,
		MainMode:         content.MainMode,
		HtmlContent:      content.HTMLContent,
		PlainTextContent: content.PlainTextContent,
		AuthorSub:        author.Sub,
		AuthorName:       author.Name,
		BaseVersionID:    idOf(base),
	})
	if err != nil {
		return GetTemplateVersionRow{}, false, err
	}
	version, err := q.GetTemplateVersion(ctx, GetTemplateVersionParams{TemplateID: template.ID, ID: recorded.ID})
	return version, recorded.Written, err
}

// draftInProgress is author's draft in progress on a template — their newest
// version of it, when it is not published — or nil.
func draftInProgress(ctx context.Context, q *Queries, templateID uuid.UUID, author VersionAuthor) (*GetTemplateVersionRow, error) {
	drafts, err := q.ListTemplateDrafts(ctx, []uuid.UUID{templateID})
	if err != nil {
		return nil, err
	}
	for _, draft := range drafts {
		if ptr.Equal(draft.AuthorSub, author.Sub) {
			version, err := q.GetTemplateVersion(ctx, GetTemplateVersionParams{TemplateID: templateID, ID: draft.ID})
			if err != nil {
				return nil, err
			}
			return &version, nil
		}
	}
	return nil, nil
}

// keeping is content with the sources it leaves out taken from the version a
// save continues, so that a save never drops a source.
func (c VersionContent) keeping(from *GetTemplateVersionRow) VersionContent {
	if from == nil {
		return c
	}
	if c.JSXSource == nil {
		c.JSXSource = from.JsxSource
	}
	if c.VisualSource == nil {
		c.VisualSource = from.VisualSource
	}
	if c.HTMLSource == nil {
		c.HTMLSource = from.HtmlSource
	}
	return c
}

// checkSources refuses content whose Main source's Authoring mode holds
// no source, or whose JSX source has no code in it.
func (c VersionContent) checkSources(ctx context.Context, q *Queries) error {
	source := map[AuthoringMode]bool{
		AuthoringModeJsx:    c.JSXSource != nil,
		AuthoringModeVisual: c.VisualSource != nil,
		AuthoringModeHtml:   c.HTMLSource != nil,
	}
	if !source[c.MainMode] {
		return &MainSourceMissingError{MainMode: c.MainMode}
	}
	if c.JSXSource != nil {
		isSource, err := q.IsJSXSource(ctx, *c.JSXSource)
		if err != nil {
			return err
		}
		if !isSource {
			return ErrNotJSXSource
		}
	}
	return nil
}

// contentOf is what a stored version holds.
func contentOf(v GetTemplateVersionRow) VersionContent {
	return VersionContent{
		Subject:          v.TemplateVersionSummary.Subject,
		JSXSource:        v.JsxSource,
		VisualSource:     v.VisualSource,
		HTMLSource:       v.HtmlSource,
		MainMode:         v.TemplateVersionSummary.MainMode,
		HTMLContent:      v.HtmlContent,
		PlainTextContent: v.PlainTextContent,
	}
}

// idOf is a version's id, or nil for no version.
func idOf(version *GetTemplateVersionRow) *uuid.UUID {
	if version == nil {
		return nil
	}
	id := version.TemplateVersionSummary.ID
	return &id
}

func uuidText(id *uuid.UUID) string {
	if id == nil {
		return "no version"
	}
	return id.String()
}
