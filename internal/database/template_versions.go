package database

import (
	"context"
)

// VersionAuthor is who wrote a Mail template version: an operator or a
// Template seed, with the Keycloak subject of the token and the name it
// carried at the time. It is what a version is written with and what the
// version routes serve.
type VersionAuthor struct {
	// operator: written in SkyMail; template_seed: written by the Template seed from the repo.
	Kind TemplateAuthorKind `json:"kind" enums:"operator,template_seed" swaggertype:"string"`
	// The Keycloak subject of the token that wrote the version. Null when not known: the first versions, made from templates written before versions were kept.
	Sub *string `json:"sub"`
	// The name the writer's token carried when the version was written. Null when not known.
	Name *string `json:"name"`
}

// Author is who wrote the version the summary describes.
func (s TemplateVersionSummary) Author() VersionAuthor {
	return VersionAuthor{Kind: s.AuthorKind, Sub: s.AuthorSub, Name: s.AuthorName}
}

// PublishTemplateWrite runs write — a statement that writes a template row
// directly: the old panel's create and edit, the Template seed's by-key
// upsert — and records what the row then holds as a published Mail template
// version, in one transaction. The row and its history cannot disagree, and a
// write that fails leaves neither behind. A write that leaves the template as
// its published version already is records nothing (see
// RecordTemplateRowAsVersion for how the version is built).
//
// requestedSubject is the subject a Template seed sent, recorded beside the
// one the row kept; nil for an operator's write.
//
// It returns the row as the write left it, copy of whichever version is now
// published.
func (s *Store) PublishTemplateWrite(ctx context.Context, author VersionAuthor, requestedSubject *string, write func(*Queries) (Template, error)) (Template, error) {
	var published Template
	err := s.InTx(ctx, func(queries *Queries) error {
		written, err := write(queries)
		if err != nil {
			return err
		}
		if _, err := queries.RecordTemplateRowAsVersion(ctx, RecordTemplateRowAsVersionParams{
			TemplateID:       written.ID,
			AuthorKind:       author.Kind,
			AuthorSub:        author.Sub,
			AuthorName:       author.Name,
			RequestedSubject: requestedSubject,
		}); err != nil {
			return err
		}
		published, err = queries.GetTemplateByIdIncludingArchived(ctx, written.ID)
		return err
	})
	return published, err
}
