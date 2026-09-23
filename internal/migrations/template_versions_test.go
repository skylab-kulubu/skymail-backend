package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	dbmigrations "github.com/skylab-kulubu/skymail-backend/db/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The schema the template-version migration finds in production: every
// migration before it.
const beforeTemplateVersions = uint(20260922235000)

// The seed's pointer comment, exactly as scripts/seed-templates.ts in
// skymail-frontend writes it into react_email_content instead of the .tsx
// source.
func seedPointer(key string) string {
	return "// Kaynak: skymail-frontend/emails/" + key + ".tsx — burada düzenlersen repodaki kaynakla ayrışır.\n"
}

// A React Email source as the old panel saves it: the panel refuses to save
// one it did not render, so a panel row always holds code like this — URLs and
// comments included, which a comment check must not mistake for a comment.
const panelJSX = `import { Html, Body, Container, Text, Img } from '@react-email/components';

// Etkinlik duyurusu — panelde yazıldı.
export default function Email() {
  return (
    <Html>
      <Body>
        <Container>
          <Img src="https://yildizskylab.com/logo.png" alt="SKY LAB" />
          {/* karşılama */}
          <Text>Merhaba {"{{.FullName}}"}</Text>
        </Container>
      </Body>
    </Html>
  );
}
`

type existingTemplate struct {
	name      string
	key       *string
	system    bool
	subject   string
	html      string
	plainText string
	react     string
	archived  bool
}

func ptr(s string) *string { return &s }

// Rows shaped like the ones production holds on the day this runs: the seed's
// System templates and keyed templates with its pointer comment (one of them
// reworded by an operator, one archived), templates written in the old panel
// with real JSX (one archived), a row with no JSX at all, and a keyed row
// where someone pasted the real source under the pointer.
var existingTemplates = []existingTemplate{
	{
		name: "Parola Sıfırlama", key: ptr("keycloak.reset-password"), system: true,
		subject: "SKY LAB parola sıfırlama isteği",
		html:    `<!DOCTYPE html><html><body><a href="{{.link}}">Parolanı sıfırla</a></body></html>`, plainText: "Parolanı sıfırla: {{.link}}",
		react: seedPointer("keycloak.reset-password"),
	},
	{
		name: "Hoş Geldin", key: ptr("core.welcome"), system: true,
		// Reworded in SkyMail; #18 kept it through later seeds.
		subject: "SKY LAB'e hoş geldin, {{.FullName}}",
		html:    `<!DOCTYPE html><html><body><p>Hoş geldin {{.FullName}}</p></body></html>`, plainText: "Hoş geldin {{.FullName}}",
		react: seedPointer("core.welcome"),
	},
	{
		name: "Serbest duyuru", key: ptr("free.basic"),
		subject: "{{.Subject}}",
		html:    `<!DOCTYPE html><html><body>{{safeHTML .Body}}</body></html>`, plainText: "{{.Body}}",
		react: seedPointer("free.basic"), archived: true,
	},
	{
		name: "Etkinlik duyurusu", subject: "Merhaba {{.FullName}}",
		html: `<!DOCTYPE html><html><body><img src="https://yildizskylab.com/logo.png"><p>Merhaba {{.FullName}}</p></body></html>`, plainText: "Merhaba {{.FullName}}",
		react: panelJSX,
	},
	{
		name: "Eski bülten", subject: "Bülten",
		html: `<!DOCTYPE html><html><body><p>Bülten</p></body></html>`, plainText: "Bülten",
		react: panelJSX, archived: true,
	},
	{
		name: "Elle yazılmış", subject: "Elle",
		html: `<p>Elle</p>`, plainText: "Elle",
		react: "",
	},
	{
		name: "Sertifika", key: ptr("core.certificate"), system: true,
		subject: "Katılım sertifikan hazır",
		html:    `<!DOCTYPE html><html><body><a href="{{.VerifyURL}}">Doğrula</a></body></html>`, plainText: "Doğrula: {{.VerifyURL}}",
		react: seedPointer("core.certificate") + panelJSX,
	},
}

type migratedVersion struct {
	key              *string
	name             string
	seq              int
	subject          string
	requestedSubject *string
	jsxSource        *string
	visualSource     *string
	htmlSource       *string
	mainMode         string
	htmlContent      string
	plainTextContent string
	authorKind       string
	authorSub        *string
	authorName       *string
	createdIsUpdated bool
	published        bool
	publishedIsRow   bool
	baseVersion      *string
	isRowsVersion    bool
}

func TestTemplateVersionMigrationGivesEveryTemplateOnePublishedFirstVersion(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()

	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(beforeTemplateVersions); err != nil {
		t.Fatalf("migrate to %d: %v", beforeTemplateVersions, err)
	}

	for _, existing := range existingTemplates {
		if _, err := database.Pool.Exec(ctx, `
			INSERT INTO templates (name, key, system, subject, html_content, plain_text_content, react_email_content,
			                       created_at, updated_at, archived_at, archived_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7,
			        NOW() - INTERVAL '30 days', NOW() - INTERVAL '3 days',
			        CASE WHEN $8::boolean THEN NOW() - INTERVAL '3 days' END,
			        CASE WHEN $8::boolean THEN '31ef736f-72da-4a40-8791-d523199cf9f0' END)
		`, existing.name, existing.key, existing.system, existing.subject, existing.html, existing.plainText, existing.react, existing.archived); err != nil {
			t.Fatalf("insert %s: %v", existing.name, err)
		}
	}
	rowsBefore := templateRows(t, database)

	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatal(err)
	}

	assertOneFirstVersionPerTemplate(t, database)
	if rows := templateRows(t, database); !equalRows(rows, rowsBefore) {
		t.Fatalf("the migration changed template rows:\nbefore %v\nafter  %v", rowsBefore, rows)
	}
	firstUp := versionsByName(t, database)

	// Down puts the schema back and leaves every row — the published copy the
	// send path reads — as it was. Later migrations go down first.
	if err := runner.Migrate(beforeTemplateVersions); err != nil {
		t.Fatalf("down: %v", err)
	}
	for _, leftover := range []string{
		`SELECT to_regclass('public.template_versions') IS NOT NULL`,
		`SELECT to_regclass('public.template_version_summaries') IS NOT NULL`,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'templates' AND column_name = 'published_version_id')`,
		`SELECT EXISTS (SELECT 1 FROM pg_type WHERE typname IN ('authoring_mode', 'template_author_kind'))`,
		`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'template_jsx_source')`,
	} {
		var exists bool
		if err := database.Pool.QueryRow(ctx, leftover).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Errorf("down left behind: %s", leftover)
		}
	}
	if rows := templateRows(t, database); !equalRows(rows, rowsBefore) {
		t.Fatalf("down changed template rows:\nbefore %v\nafter  %v", rowsBefore, rows)
	}

	// And up again arrives at the same history.
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertOneFirstVersionPerTemplate(t, database)
	secondUp := versionsByName(t, database)
	for name, version := range firstUp {
		if !equalVersions(secondUp[name], version) {
			t.Errorf("%s after down and up = %+v, want %+v", name, secondUp[name], version)
		}
	}
}

func assertOneFirstVersionPerTemplate(t *testing.T, database testpostgres.Database) {
	t.Helper()
	versions := versionsByName(t, database)
	if len(versions) != len(existingTemplates) {
		t.Fatalf("templates with a version = %d, want %d", len(versions), len(existingTemplates))
	}
	var total int
	if err := database.Pool.QueryRow(context.Background(), `SELECT count(*) FROM template_versions`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != len(existingTemplates) {
		t.Fatalf("versions = %d, want exactly one per template (%d)", total, len(existingTemplates))
	}

	for _, existing := range existingTemplates {
		version, ok := versions[existing.name]
		if !ok {
			t.Errorf("%s: no version", existing.name)
			continue
		}
		if version.seq != 1 || !version.published || !version.publishedIsRow || version.baseVersion != nil {
			t.Errorf("%s: seq=%d published=%v base=%v, want the first version, published, no base", existing.name, version.seq, version.published, version.baseVersion)
		}
		if !version.isRowsVersion {
			t.Errorf("%s: the row does not point at its first version", existing.name)
		}
		if !version.createdIsUpdated {
			t.Errorf("%s: the first version is not dated when the row was last written", existing.name)
		}

		wantAuthor := "operator"
		if existing.key != nil {
			wantAuthor = "template_seed"
		}
		if version.authorKind != wantAuthor || version.authorSub != nil || version.authorName != nil {
			t.Errorf("%s: author = %s %v %v, want %s with no identity (unknown before versioning)", existing.name, version.authorKind, version.authorSub, version.authorName, wantAuthor)
		}

		if version.requestedSubject != nil {
			t.Errorf("%s: requested_subject = %q, but what the seed once sent is not known", existing.name, *version.requestedSubject)
		}
		if version.subject != existing.subject || version.htmlContent != existing.html || version.plainTextContent != existing.plainText {
			t.Errorf("%s: subject/render = %q %q %q, want the row's", existing.name, version.subject, version.htmlContent, version.plainTextContent)
		}
		if version.visualSource != nil {
			t.Errorf("%s: a Visual source appeared from nowhere", existing.name)
		}

		isSource := existing.react == panelJSX || existing.react == seedPointer("core.certificate")+panelJSX
		if isSource {
			if version.mainMode != "jsx" || version.jsxSource == nil || *version.jsxSource != existing.react || version.htmlSource != nil {
				t.Errorf("%s: main=%s jsx=%v html=%v, want JSX as the Main source with the row's code and no HTML source", existing.name, version.mainMode, version.jsxSource != nil, version.htmlSource != nil)
			}
		} else {
			if version.mainMode != "html" || version.jsxSource != nil || version.htmlSource == nil || *version.htmlSource != existing.html {
				t.Errorf("%s: main=%s jsx=%v html=%v, want HTML as the Main source holding html_content and no JSX source", existing.name, version.mainMode, version.jsxSource, version.htmlSource != nil)
			}
		}
	}
}

func versionsByName(t *testing.T, database testpostgres.Database) map[string]migratedVersion {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		SELECT t.key, t.name, v.seq, v.subject, v.requested_subject, v.jsx_source, v.visual_source::text, v.html_source, v.main_mode::text,
		       v.html_content, v.plain_text_content, v.author_kind::text, v.author_sub, v.author_name,
		       v.created_at = t.updated_at, v.published_at IS NOT NULL, v.published_at = t.updated_at,
		       v.base_version_id::text, t.published_version_id = v.id
		FROM template_versions v
		         JOIN templates t ON t.id = v.template_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	versions := map[string]migratedVersion{}
	for rows.Next() {
		var v migratedVersion
		if err := rows.Scan(&v.key, &v.name, &v.seq, &v.subject, &v.requestedSubject, &v.jsxSource, &v.visualSource, &v.htmlSource, &v.mainMode,
			&v.htmlContent, &v.plainTextContent, &v.authorKind, &v.authorSub, &v.authorName,
			&v.createdIsUpdated, &v.published, &v.publishedIsRow, &v.baseVersion, &v.isRowsVersion); err != nil {
			t.Fatal(err)
		}
		versions[v.name] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return versions
}

// templateRows reads every template row whole, except the pointer the
// migration adds, so a comparison catches any column it touched.
func templateRows(t *testing.T, database testpostgres.Database) map[string]string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		SELECT name, (to_jsonb(t) - 'published_version_id')::text FROM templates t`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	byName := map[string]string{}
	for rows.Next() {
		var name, row string
		if err := rows.Scan(&name, &row); err != nil {
			t.Fatal(err)
		}
		byName[name] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return byName
}

func equalRows(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, row := range a {
		if b[name] != row {
			return false
		}
	}
	return true
}

func equalVersions(a, b migratedVersion) bool {
	same := func(x, y *string) bool { return (x == nil && y == nil) || (x != nil && y != nil && *x == *y) }
	return a.name == b.name && same(a.key, b.key) && a.seq == b.seq && a.subject == b.subject && same(a.requestedSubject, b.requestedSubject) &&
		same(a.jsxSource, b.jsxSource) && same(a.visualSource, b.visualSource) && same(a.htmlSource, b.htmlSource) &&
		a.mainMode == b.mainMode && a.htmlContent == b.htmlContent && a.plainTextContent == b.plainTextContent &&
		a.authorKind == b.authorKind && same(a.authorSub, b.authorSub) && same(a.authorName, b.authorName) &&
		a.createdIsUpdated == b.createdIsUpdated && a.published == b.published && a.publishedIsRow == b.publishedIsRow &&
		same(a.baseVersion, b.baseVersion) && a.isRowsVersion == b.isRowsVersion
}

// migrationRunner drives the embedded migrations with golang-migrate itself,
// the tool behind migrations.Run and make migrate-down, so a step down runs
// the real down file.
func migrationRunner(t *testing.T, databaseURL string) *migrate.Migrate {
	t.Helper()
	source, err := iofs.New(dbmigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	runner, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sourceErr, databaseErr := runner.Close()
		if err := errors.Join(sourceErr, databaseErr); err != nil {
			t.Logf("close migration runner: %v", err)
		}
	})
	return runner
}
