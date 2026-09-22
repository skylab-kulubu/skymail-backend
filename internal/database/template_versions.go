package database

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// VersionAuthor is who wrote a Mail template version: an operator or a
// Template seed, with the Keycloak subject of the token and the name it
// carried at the time. Either may be missing when the token did not say.
type VersionAuthor struct {
	Kind TemplateAuthorKind
	Sub  *string
	Name *string
}

// PublishTemplateWrite runs write — a statement that writes a template row
// directly: the old panel's create and edit, the Template seed's by-key
// upsert — and records what the row then holds as a published Mail template
// version, in one transaction. The row and its history cannot disagree, and a
// write that fails leaves neither behind.
//
// It returns the row as the write left it.
func (s *Store) PublishTemplateWrite(ctx context.Context, author VersionAuthor, write func(*Queries) (Template, error)) (Template, error) {
	var published Template
	err := pgx.BeginFunc(ctx, s.Conn, func(tx pgx.Tx) error {
		queries := s.WithTx(tx)
		written, err := write(queries)
		if err != nil {
			return err
		}
		published, err = queries.PublishTemplateRowAsVersion(ctx, PublishTemplateRowAsVersionParams{
			TemplateID: written.ID,
			AuthorKind: author.Kind,
			AuthorSub:  author.Sub,
			AuthorName: author.Name,
		})
		return err
	})
	return published, err
}
