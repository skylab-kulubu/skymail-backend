package database

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// template_jsx_source decides whether react_email_content holds a JSX source.
// The Template seed has been writing a pointer comment there instead of the
// .tsx source, and a comment is not a source.
func TestTemplateJSXSourceRule(t *testing.T) {
	db := lifecycleStore(t)
	pointer := "// Kaynak: skymail-frontend/emails/keycloak.verify-email.tsx — burada düzenlersen repodaki kaynakla ayrışır.\n"
	component := "export default function Email() {\n  return <Img src=\"https://yildizskylab.com/logo.png\" />; // logo\n}\n"

	for name, tc := range map[string]struct {
		content  string
		isSource bool
	}{
		"empty":                           {"", false},
		"blank":                           {"  \n\t\n", false},
		"the seed's pointer":              {pointer, false},
		"the pointer without its newline": {pointer[:len(pointer)-1], false},
		"a block comment":                 {"/* taslak */\n", false},
		"line and block comments":         {"// bir\n/* iki\n üç */\n// dört", false},
		"a component":                     {component, true},
		"a component under the pointer":   {pointer + component, true},
		"a URL is not a comment":          {`<a href="https://yildizskylab.com">x</a>`, true},
		"code after a block comment":      {"/* not */ export default () => null", true},
		"anything with code":              {"{}", true},
	} {
		t.Run(name, func(t *testing.T) {
			var source *string
			if err := db.Conn.QueryRow(context.Background(), `SELECT template_jsx_source($1)`, tc.content).Scan(&source); err != nil {
				t.Fatal(err)
			}
			if tc.isSource && (source == nil || *source != tc.content) {
				t.Fatalf("template_jsx_source(%q) = %v, want the content as the JSX source", tc.content, source)
			}
			if !tc.isSource && source != nil {
				t.Fatalf("template_jsx_source(%q) = %q, want no JSX source", tc.content, *source)
			}
		})
	}
}

// The version table refuses what would make a history dishonest.
func TestTemplateVersionConstraints(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()

	create := func(name string) Template {
		t.Helper()
		template, err := db.PublishTemplateWrite(ctx, VersionAuthor{Kind: TemplateAuthorKindOperator}, func(q *Queries) (Template, error) {
			return q.CreateTemplate(ctx, CreateTemplateParams{
				Name: name, Subject: name, HtmlContent: "<p>" + name + "</p>", PlainTextContent: name, ReactEmailContent: "",
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		return template
	}
	first, second := create("Birinci"), create("İkinci")

	insert := func(columns string, values ...any) error {
		_, err := db.Conn.Exec(ctx, `
			INSERT INTO template_versions (template_id, seq, subject, main_mode, html_content, plain_text_content, author_kind`+columns+`)
			VALUES ($1, $2, 'Konu', $3, '<p>x</p>', 'x', 'operator'`+placeholders(4, len(values)-3)+`)`, values...)
		return err
	}

	for name, tc := range map[string]struct {
		err     error
		code    string
		purpose string
	}{
		"main mode without its source": {
			err:  insert(", html_source", first.ID, 2, "jsx", "<p>x</p>"),
			code: "23514", purpose: "the Main source must exist",
		},
		"a Visual source that is not a document": {
			err:  insert(", visual_source", first.ID, 2, "visual", `"metin"`),
			code: "23514", purpose: "a Visual source is a JSON document",
		},
		"a second version 1": {
			err:  insert(", html_source", first.ID, 1, "html", "<p>x</p>"),
			code: "23505", purpose: "one number per version within a template",
		},
		"a base from another template": {
			err:  insert(", html_source, base_version_id", first.ID, 2, "html", "<p>x</p>", *second.PublishedVersionID),
			code: "23503", purpose: "a draft starts from a version of its own template",
		},
		"published before written": {
			err:  insert(", html_source, created_at, published_at", first.ID, 2, "html", "<p>x</p>", "2026-09-23T12:00:00Z", "2026-09-23T11:00:00Z"),
			code: "23514", purpose: "publishing follows writing",
		},
	} {
		var pgErr *pgconn.PgError
		if !errors.As(tc.err, &pgErr) || pgErr.Code != tc.code {
			t.Errorf("%s: err = %v, want %s (%s)", name, tc.err, tc.code, tc.purpose)
		}
	}

	// The row is the copy of a version of its own template, never another's.
	_, err := db.Conn.Exec(ctx, `UPDATE templates SET published_version_id = $2 WHERE id = $1`, first.ID, *second.PublishedVersionID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("pointing a row at another template's version: err = %v, want 23503", err)
	}
	_, err = db.Conn.Exec(ctx, `UPDATE templates SET published_version_id = $2 WHERE id = $1`, first.ID, uuid.New())
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("pointing a row at no version: err = %v, want 23503", err)
	}
}

// A write that fails leaves no version behind, and a version that cannot be
// recorded undoes the write: the row and its history move together.
func TestPublishTemplateWriteIsOneTransaction(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	operator := VersionAuthor{Kind: TemplateAuthorKindOperator}

	template, err := db.PublishTemplateWrite(ctx, operator, func(q *Queries) (Template, error) {
		return q.CreateTemplate(ctx, CreateTemplateParams{
			Name: "Bülten", Subject: "Bülten", HtmlContent: "<p>Bülten</p>", PlainTextContent: "Bülten", ReactEmailContent: "",
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	// The row write goes through, then there is no row to record a version
	// of — the write names a template that does not exist.
	if _, err := db.PublishTemplateWrite(ctx, operator, func(q *Queries) (Template, error) {
		written, err := q.UpdateTemplate(ctx, UpdateTemplateParams{
			ID: template.ID, Name: "Bülten", Subject: "Değişti", HtmlContent: "<p>Değişti</p>", PlainTextContent: "Değişti", ReactEmailContent: "",
		})
		written.ID = uuid.New()
		return written, err
	}); err == nil {
		t.Fatal("the write went through without its version")
	}
	row, err := db.GetTemplateById(ctx, template.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Subject != "Bülten" || row.PublishedVersionID == nil || *row.PublishedVersionID != *template.PublishedVersionID {
		t.Fatalf("row after a failed version = %q, copy of %v; want the row as it was", row.Subject, row.PublishedVersionID)
	}

	// A write that fails records nothing: an archived template is not edited.
	if _, err := db.ArchiveTemplate(ctx, ArchiveTemplateParams{ID: template.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PublishTemplateWrite(ctx, operator, func(q *Queries) (Template, error) {
		return q.UpdateTemplate(ctx, UpdateTemplateParams{
			ID: template.ID, Name: "x", Subject: "x", HtmlContent: "x", PlainTextContent: "x", ReactEmailContent: "",
		})
	}); err == nil {
		t.Fatal("editing an archived template succeeded")
	}
	var versions int
	if err := db.Conn.QueryRow(ctx, `SELECT count(*) FROM template_versions WHERE template_id = $1`, template.ID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Fatalf("versions = %d, want only the first", versions)
	}
}

// Writers that meet on one template wait for its row's lock, so they number
// their versions one after another, and the row ends up a copy of the last.
func TestConcurrentTemplateWritesNumberVersionsInTurn(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	operator := VersionAuthor{Kind: TemplateAuthorKindOperator}

	template, err := db.PublishTemplateWrite(ctx, operator, func(q *Queries) (Template, error) {
		return q.CreateTemplate(ctx, CreateTemplateParams{
			Name: "Bülten", Subject: "0", HtmlContent: "<p>0</p>", PlainTextContent: "0", ReactEmailContent: "",
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	const writers = 12
	errs := make(chan error, writers)
	for i := 1; i <= writers; i++ {
		go func(i int) {
			subject := fmt.Sprint(i)
			_, err := db.PublishTemplateWrite(ctx, operator, func(q *Queries) (Template, error) {
				return q.UpdateTemplate(ctx, UpdateTemplateParams{
					ID: template.ID, Name: "Bülten", Subject: subject, HtmlContent: "<p>" + subject + "</p>", PlainTextContent: subject, ReactEmailContent: "",
				})
			})
			errs <- err
		}(i)
	}
	for i := 0; i < writers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("a concurrent write failed: %v", err)
		}
	}

	var count, highest int
	var lastSubject, rowSubject string
	var rowIsLast bool
	if err := db.Conn.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM template_versions WHERE template_id = t.id),
		       (SELECT max(seq) FROM template_versions WHERE template_id = t.id),
		       v.subject, t.subject, v.seq = (SELECT max(seq) FROM template_versions WHERE template_id = t.id)
		FROM templates t
		         JOIN template_versions v ON v.id = t.published_version_id
		WHERE t.id = $1`, template.ID).Scan(&count, &highest, &lastSubject, &rowSubject, &rowIsLast); err != nil {
		t.Fatal(err)
	}
	if count != writers+1 || highest != writers+1 {
		t.Fatalf("versions = %d numbered up to %d, want 1…%d", count, highest, writers+1)
	}
	if !rowIsLast || lastSubject != rowSubject {
		t.Fatalf("the row is a copy of a version that is not the last (row %q, its version %q)", rowSubject, lastSubject)
	}
}

func placeholders(from, count int) string {
	out := ""
	for i := 0; i < count; i++ {
		out += fmt.Sprintf(", $%d", from+i)
	}
	return out
}
