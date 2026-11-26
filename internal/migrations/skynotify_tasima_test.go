package migrations_test

// The SkyNotify rename (ADR 0091): the schema migration that renames the
// search fold, and the files the rename window runs by hand (db/tasima): the
// schema rollback and the data migration forward and back. The old names
// appear here as data: they are what these move from and back to.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	dbmigrations "github.com/skylab-kulubu/skynotify-backend/db/migrations"
	"github.com/skylab-kulubu/skynotify-backend/internal/database"
	"github.com/skylab-kulubu/skynotify-backend/internal/migrations"
	"github.com/skylab-kulubu/skynotify-backend/internal/testpostgres"
)

// The migration that renames the search fold and the comments.
const renameToSkyNotify = uint(20261125090000)

const (
	oldFold = "skymail_search_fold"
	newFold = "skynotify_search_fold"
)

// The comments the rename rewrites: the column's or the table's, by query.
var renamedComments = map[string]string{
	"mail_tasks.tracking_requested":     `SELECT col_description('mail_tasks'::regclass, (SELECT attnum FROM pg_attribute WHERE attrelid = 'mail_tasks'::regclass AND attname = 'tracking_requested'))`,
	"mail_approvals.tracking_requested": `SELECT col_description('mail_approvals'::regclass, (SELECT attnum FROM pg_attribute WHERE attrelid = 'mail_approvals'::regclass AND attname = 'tracking_requested'))`,
	"mail_unsubscribes":                 `SELECT obj_description('mail_unsubscribes'::regclass, 'pg_class')`,
	"mail_tasks.sent_by_service":        `SELECT col_description('mail_tasks'::regclass, (SELECT attnum FROM pg_attribute WHERE attrelid = 'mail_tasks'::regclass AND attname = 'sent_by_service'))`,
}

func foldFunctions(t *testing.T, db testpostgres.Database) []string {
	t.Helper()
	rows, err := db.Pool.Query(context.Background(), `SELECT proname::text FROM pg_proc WHERE proname LIKE '%search_fold' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// assertComments checks that every renamed comment names the service by
// name (SkyNotify or SKYNOTIFY) and none by the other.
func assertComments(t *testing.T, db testpostgres.Database, name, other string) {
	t.Helper()
	for object, query := range renamedComments {
		var comment string
		if err := db.Pool.QueryRow(context.Background(), query).Scan(&comment); err != nil {
			t.Fatalf("%s: %v", object, err)
		}
		lower := strings.ToLower(comment)
		if !strings.Contains(lower, name) || strings.Contains(lower, other) {
			t.Errorf("%s comment = %q; want it to name %s, not %s", object, comment, name, other)
		}
	}
}

// Up makes the fold anew under its new name (inlined, the same result) and
// drops the old one, and names SkyNotify in the comments; down puts the old
// function and comments back; up again works.
func TestRenameToSkyNotifyMigrationGoesUpAndDown(t *testing.T) {
	db := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, db.URL)
	if err := runner.Migrate(mailTaskKind); err != nil {
		t.Fatalf("before: %v", err)
	}
	if got := foldFunctions(t, db); !slices.Equal(got, []string{oldFold}) {
		t.Fatalf("before: fold functions %v, want %s", got, oldFold)
	}
	assertComments(t, db, "skymail", "skynotify")

	if err := runner.Migrate(renameToSkyNotify); err != nil {
		t.Fatalf("up: %v", err)
	}
	if got := foldFunctions(t, db); !slices.Equal(got, []string{newFold}) {
		t.Fatalf("up: fold functions %v, want %s", got, newFold)
	}
	assertComments(t, db, "skynotify", "skymail")
	// Inlined: a SQL function with a SET is not.
	var config []string
	if err := db.Pool.QueryRow(ctx, `SELECT COALESCE(proconfig, '{}') FROM pg_proc WHERE proname = $1`, newFold).Scan(&config); err != nil || len(config) != 0 {
		t.Errorf("%s settings = %v %v; want none", newFold, config, err)
	}
	rows, err := db.Pool.Query(ctx, `EXPLAIN (VERBOSE, COSTS OFF) SELECT `+newFold+`(name) FROM templates`)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	if text := strings.Join(plan, "\n"); strings.Contains(text, newFold) || !strings.Contains(text, "translate") {
		t.Errorf("not inlined:\n%s", text)
	}
	var folded string
	if err := db.Pool.QueryRow(ctx, `SELECT `+newFold+`('IŞIK İŞ Ğ Ü Ö Ç ı')`).Scan(&folded); err != nil || folded != "işik iş ğ ü ö ç i" {
		t.Errorf("fold = %q %v", folded, err)
	}

	if err := runner.Migrate(mailTaskKind); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := foldFunctions(t, db); !slices.Equal(got, []string{oldFold}) {
		t.Fatalf("down: fold functions %v, want %s", got, oldFold)
	}
	assertComments(t, db, "skymail", "skynotify")
	if err := db.Pool.QueryRow(ctx, `SELECT `+oldFold+`('IŞIK')`).Scan(&folded); err != nil || folded != "işik" {
		t.Errorf("old fold after down = %q %v", folded, err)
	}
	if err := runner.Migrate(renameToSkyNotify); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// tasimaFile reads one of the files the window runs by hand.
func tasimaFile(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "db", "tasima", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// runTasima runs a db/tasima file as the window does, through the container's
// psql as its superuser (-1: one transaction, unless oneTransaction is false),
// and returns its output lines.
func runTasima(t *testing.T, db testpostgres.Database, dbName, name string, oneTransaction bool) ([]string, error) {
	t.Helper()
	args := []string{"exec", "-i", db.Container, "psql", "-X", "-v", "ON_ERROR_STOP=1"}
	if oneTransaction {
		args = append(args, "-1")
	}
	args = append(args, "-U", "postgres", "-d", dbName, "-f", "-")
	cmd := exec.Command("docker", args...)
	cmd.Stdin = bytes.NewReader(tasimaFile(t, name))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var lines []string
	if out := strings.TrimRight(stdout.String(), "\n"); out != "" {
		lines = strings.Split(out, "\n")
	}
	if err != nil {
		return lines, fmt.Errorf("%s: %w\n%s", name, err, stderr.String())
	}
	return lines, nil
}

func dockerOutput(t *testing.T, db testpostgres.Database, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"exec", db.Container}, args...)...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, exit.Stderr)
		}
		t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// schemaDump is a database's schema as pg_dump writes it, without the lines a
// newer pg_dump makes random on every run.
func schemaDump(t *testing.T, db testpostgres.Database, dbName string) string {
	t.Helper()
	var kept []string
	for _, line := range strings.Split(dockerOutput(t, db, "pg_dump", "-U", "postgres", "-d", dbName, "--schema-only"), "\n") {
		if strings.HasPrefix(line, `\restrict `) || strings.HasPrefix(line, `\unrestrict `) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// embeddedBefore is the embedded migration set without version and the ones
// after it: what an image from before it embeds.
func embeddedBefore(t *testing.T, version uint) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(dbmigrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	older := fstest.MapFS{}
	for _, entry := range entries {
		if migrationVersion(t, entry.Name()) >= version {
			continue
		}
		data, err := fs.ReadFile(dbmigrations.Files, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		older[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return older
}

// sema-geri.sql leaves exactly the schema the image from before builds, at
// its version: that image starts with nothing to migrate, and this one then
// migrates forward again. It runs only at the rename's version.
func TestSchemaRollbackFileLeavesTheOldImagesSchema(t *testing.T) {
	db := testpostgres.StartDatabase(t)
	ctx := context.Background()

	// The old image's schema as it builds it, in a database of its own.
	dockerOutput(t, db, "createdb", "-U", "postgres", "eski")
	oldURL := strings.Replace(db.URL, "/skynotifytest?", "/eski?", 1)
	if err := migrationRunner(t, oldURL).Migrate(mailTaskKind); err != nil {
		t.Fatalf("the old image's schema: %v", err)
	}
	want := schemaDump(t, db, "eski")

	if _, err := migrations.Run(ctx, db.URL, 0); err != nil {
		t.Fatalf("up: %v", err)
	}
	// Without -1 it stops before it changes anything.
	if _, err := runTasima(t, db, "skynotifytest", "sema-geri.sql", false); err == nil {
		t.Fatal("sema-geri.sql without -1 ran; want it to stop")
	}
	if got := foldFunctions(t, db); !slices.Equal(got, []string{newFold}) {
		t.Fatalf("after a refused run: fold functions %v, want %s", got, newFold)
	}

	lines, err := runTasima(t, db, "skynotifytest", "sema-geri.sql", true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(lines, "\n"), fmt.Sprintf("sonuc|sema-geri|%d|f", mailTaskKind); got != want {
		t.Fatalf("sema-geri.sql printed %q, want %q", got, want)
	}
	if got := schemaDump(t, db, "skynotifytest"); got != want {
		t.Fatalf("after sema-geri.sql the schema is not the old image's:\n--- old image\n%s\n--- rolled back\n%s", want, got)
	}
	// Again: refused, nothing changes.
	if _, err := runTasima(t, db, "skynotifytest", "sema-geri.sql", true); err == nil || !strings.Contains(err.Error(), "zaten geri alınmış") {
		t.Fatalf("sema-geri.sql a second time: %v; want it refused", err)
	}
	if got := schemaDump(t, db, "skynotifytest"); got != want {
		t.Fatal("a refused sema-geri.sql changed the schema")
	}

	// The old image starts: its migrations find nothing to do, its fold works.
	source, err := iofs.New(embeddedBefore(t, renameToSkyNotify), ".")
	if err != nil {
		t.Fatal(err)
	}
	oldImage, err := migrate.NewWithSourceInstance("iofs", source, db.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := oldImage.Up(); !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("the old image's migrations: %v; want no change", err)
	}
	if version, dirty, err := oldImage.Version(); err != nil || version != mailTaskKind || dirty {
		t.Fatalf("the old image reads version %d dirty %v %v; want %d clean", version, dirty, err, mailTaskKind)
	}
	if sourceErr, dbErr := oldImage.Close(); sourceErr != nil || dbErr != nil {
		t.Fatal(sourceErr, dbErr)
	}
	var folded string
	if err := db.Pool.QueryRow(ctx, `SELECT `+oldFold+`('IŞIK')`).Scan(&folded); err != nil || folded != "işik" {
		t.Fatalf("the old image's fold = %q %v", folded, err)
	}

	// This image starts again: it migrates forward, and its queries fold.
	version, err := migrations.Run(ctx, db.URL, 0)
	if err != nil || version != renameToSkyNotify {
		t.Fatalf("up again: version %d %v; want %d", version, err, renameToSkyNotify)
	}
	search := "IŞIK"
	if _, err := database.New(db.Pool).CountTemplates(ctx, &search); err != nil {
		t.Fatalf("a search after up again: %v", err)
	}
}

// The rows the data migration moves, one or more of each kind, with the old
// markers: a template and its versions (an HTML one, a Visual draft, a JSX
// one), a request for approval with its person and an edit, a person's send
// and its queue rows, SkyNotify's own notice, a News send, an
// erase-after-send send still queued; and rows without a marker.
const seedOldMarkers = `
INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content,
                       contract_required_variables, operator_required_variables)
VALUES ('5e000000-0000-4000-8000-000000000001', 'Duyuru', '{{.Subject}}',
        '<p><a data-skymail-notrack href="https://a.example">a</a> <a data-skymail-notrack="true" href="https://b.example">b</a>{{.BodyHtml}}</p>',
        'a b', '<Link data-skymail-notrack href="https://a.example">a</Link>',
        '[{"name":"Subject","reason":"Konu gönderimde yazılır."}]', '{BodyHtml}'),
       ('5e000000-0000-4000-8000-000000000002', 'Bildirim', '{{.Subject}}', '<p>{{.BodyHtml}}</p>', '{{.BodyHtml}}', '',
        '[{"name":"Subject","reason":"Konu gönderimde yazılır."}]', '{BodyHtml}');

INSERT INTO template_versions (id, template_id, seq, name, subject, html_source, main_mode, html_content,
                               plain_text_content, author_kind, published_at)
VALUES ('5e000000-0000-4000-8000-000000000011', '5e000000-0000-4000-8000-000000000001', 1, 'Duyuru', '{{.Subject}}',
        '<p><a data-skymail-notrack href="https://a.example">a</a>{{.BodyHtml}}</p>', 'html',
        '<p><a data-skymail-notrack href="https://a.example">a</a>{{.BodyHtml}}</p>', 'a', 'template_seed', NOW());
INSERT INTO template_versions (id, template_id, seq, name, subject, visual_source, main_mode, html_content,
                               plain_text_content, author_kind, author_sub, author_name, base_version_id)
VALUES ('5e000000-0000-4000-8000-000000000012', '5e000000-0000-4000-8000-000000000001', 2, 'Duyuru', '{{.Subject}}',
        '{"type":"skymail.visual","version":1,"blocks":[{"type":"paragraph","content":[{"type":"text","text":"a","marks":[{"type":"link","href":"https://a.example","notrack":true}]}]}]}',
        'visual', '<p><a data-skymail-notrack href="https://a.example">a</a></p>', 'a', 'operator', 'sub', 'Ayşe Kaya',
        '5e000000-0000-4000-8000-000000000011');
INSERT INTO template_versions (id, template_id, seq, name, subject, jsx_source, main_mode, html_content,
                               plain_text_content, author_kind, author_sub, author_name, published_at)
VALUES ('5e000000-0000-4000-8000-000000000013', '5e000000-0000-4000-8000-000000000001', 3, 'Duyuru', '{{.Subject}}',
        '<Link data-skymail-notrack href="https://a.example">a</Link>', 'jsx',
        '<p><a data-skymail-notrack href="https://a.example">a</a></p>', 'a', 'operator', 'sub', 'Ayşe Kaya', NOW()),
       ('5e000000-0000-4000-8000-000000000021', '5e000000-0000-4000-8000-000000000002', 1, 'Bildirim', '{{.Subject}}',
        '<p>{{.BodyHtml}}</p>', 'jsx', '<p>{{.BodyHtml}}</p>', '{{.BodyHtml}}', 'template_seed', NULL, NULL, NOW());

INSERT INTO mail_approvals (id, submitter_sub, template_id, template_version_id, body_variables,
                            created_at, submitted_at, deadline_at, updated_at)
VALUES ('5e000000-0000-4000-8000-000000000031', 'sub', '5e000000-0000-4000-8000-000000000001',
        '5e000000-0000-4000-8000-000000000011',
        '{"Subject":"Konu","BodyHtml":"<p><a data-skymail-notrack href=\"https://c.example\">c</a></p>"}',
        NOW(), NOW(), NOW() + INTERVAL '7 days', NOW());
INSERT INTO mail_approval_recipients (approval_id, position, email, full_name)
VALUES ('5e000000-0000-4000-8000-000000000031', 1, 'ayse@example.com', 'Ayşe Kaya');
INSERT INTO mail_approval_events (approval_id, seq, kind, actor_sub, actor_name, changes, created_at)
VALUES ('5e000000-0000-4000-8000-000000000031', 1, 'submitted', 'sub', 'Ayşe Kaya', NULL, NOW()),
       ('5e000000-0000-4000-8000-000000000031', 2, 'edited', 'approver', 'Ali Veli',
        '[{"field":"body_variables","name":"BodyHtml","before":"<p><a data-skymail-notrack href=\"https://d.example\">d</a></p>","after":"<p>d</p>"}]',
        NOW());

INSERT INTO mail_tasks (id, sent_by, template_id, body_variables)
VALUES ('5e000000-0000-4000-8000-000000000041', 'sub', '5e000000-0000-4000-8000-000000000001',
        '{"Subject":"Konu","BodyHtml":"<p><a data-skymail-notrack href=\"https://e.example\">e</a></p>"}'),
       ('5e000000-0000-4000-8000-000000000042', 'skymail', '5e000000-0000-4000-8000-000000000002',
        '{"Subject":"Mail onayı","BodyHtml":"<p>süresi doldu</p>"}');
INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, source_kind, source_ref, source_version,
                        source_snapshot, source_audience)
VALUES ('5e000000-0000-4000-8000-000000000043', 'sub', '5e000000-0000-4000-8000-000000000002', '{}', 'news',
        'news:etkinlik', 1,
        '{"title":"Haber","html":"<p><a data-skymail-notrack href=\"https://n.example\">n</a></p>"}', 'members');
INSERT INTO mail_tasks (id, sent_by, template_id, body_variables, erase_after_send)
VALUES ('5e000000-0000-4000-8000-000000000044', 'service', '5e000000-0000-4000-8000-000000000002',
        '{"Subject":"Kod","BodyHtml":"<p><a data-skymail-notrack href=\"https://f.example\">f</a></p>"}', true);

INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, status)
VALUES ('5e000000-0000-4000-8000-000000000041', 'Ayşe Kaya', 'ayse@example.com', 'Konu', 'e',
        '<p><a data-skymail-notrack href="https://e.example">e</a> <a data-skymail-notrack href="https://g.example">g</a></p>', 'sent'),
       ('5e000000-0000-4000-8000-000000000041', 'Ali Veli', 'ali@example.com', 'Konu', 'e',
        '<p><a data-skymail-notrack href="https://e.example">e</a></p>', 'pending'),
       ('5e000000-0000-4000-8000-000000000042', 'Ayşe Kaya', 'ayse@example.com', 'Mail onayı', 'süresi doldu',
        '<p>süresi doldu</p>', 'sent'),
       ('5e000000-0000-4000-8000-000000000044', 'Can Er', 'can@example.com', 'Kod', 'f',
        '<p><a data-skymail-notrack href="https://f.example">f</a></p>', 'pending');
`

// The tables the data migration writes.
var tasimaTables = []string{"templates", "template_versions", "mail_approvals", "mail_approval_events", "mail_tasks", "mail_queue"}

// tableDigests is every row of the tables the data migration writes, hashed
// per table: equal digests are equal rows.
func tableDigests(t *testing.T, db testpostgres.Database) map[string]string {
	t.Helper()
	digests := map[string]string{}
	for _, table := range tasimaTables {
		var digest string
		if err := db.Pool.QueryRow(context.Background(),
			`SELECT md5(COALESCE(string_agg(r::text, E'\n' ORDER BY r::text), '')) FROM `+table+` r`).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		digests[table] = digest
	}
	return digests
}

func assertDigests(t *testing.T, db testpostgres.Database, want map[string]string, when string) {
	t.Helper()
	got := tableDigests(t, db)
	for _, table := range tasimaTables {
		if got[table] != want[table] {
			t.Errorf("%s: %s rows are not what they were", when, table)
		}
	}
}

// kalipLines are a file's kalip lines, by table|column|marker.
func kalipLines(lines []string) map[string]string {
	counts := map[string]string{}
	for _, line := range lines {
		fields := strings.Split(line, "|")
		if fields[0] == "kalip" && len(fields) == 7 {
			counts[strings.Join(fields[1:4], "|")] = strings.Join(fields[4:], "|")
		}
	}
	return counts
}

// The markers in the seed, column by column: rows|occurrences|other-case rows.
var seededOldMarkers = map[string]string{
	"templates|html_content|data-skymail-notrack":          "1|2|0",
	"templates|react_email_content|data-skymail-notrack":   "1|1|0",
	"template_versions|jsx_source|data-skymail-notrack":    "1|1|0",
	"template_versions|html_source|data-skymail-notrack":   "1|1|0",
	"template_versions|html_content|data-skymail-notrack":  "3|3|0",
	"template_versions|visual_source|skymail.visual":       "1|1|0",
	"template_versions|visual_source|data-skymail-notrack": "0|0|0",
	"mail_approvals|body_variables|data-skymail-notrack":   "1|1|0",
	"mail_approval_events|changes|data-skymail-notrack":    "1|1|0",
	"mail_tasks|body_variables|data-skymail-notrack":       "2|2|0",
	"mail_tasks|source_snapshot|data-skymail-notrack":      "1|1|0",
	"mail_tasks|sent_by|skymail":                           "1|1|0",
	"mail_queue|body_html|data-skymail-notrack":            "3|4|0",
}

// renamed is a marker as the forward migration writes it.
func renamed(marker string) string {
	return strings.Replace(marker, "skymail", "skynotify", 1)
}

// assertMarkers checks a file's kalip lines: the seed's counts under the
// markers moved to (or the old ones, when back), and none left of the others.
func assertMarkers(t *testing.T, lines []string, forward bool) {
	t.Helper()
	got := kalipLines(lines)
	if len(got) != 2*len(seededOldMarkers) {
		t.Errorf("%d kalip lines, want %d:\n%s", len(got), 2*len(seededOldMarkers), strings.Join(lines, "\n"))
	}
	for key, want := range seededOldMarkers {
		fields := strings.Split(key, "|")
		oldKey, newKey := key, strings.Join([]string{fields[0], fields[1], renamed(fields[2])}, "|")
		present, absent := oldKey, newKey
		if forward {
			present, absent = newKey, oldKey
		}
		if got[present] != want {
			t.Errorf("%s = %q, want %q", present, got[present], want)
		}
		if got[absent] != "0|0|0" {
			t.Errorf("%s = %q, want 0|0|0", absent, got[absent])
		}
	}
}

func lastLine(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// veri-ileri.sql moves every marker, and veri-geri.sql moves them back to the
// very rows they were; forward again gives the same rows as the first time.
// Each refuses to run twice, without -1, or on rows it could not move back
// exactly, and then changes nothing. veri-olcum.sql counts what they move.
func TestDataMigrationGoesForwardBackAndForward(t *testing.T) {
	db := testpostgres.StartDatabase(t)
	ctx := context.Background()
	if _, err := migrations.Run(ctx, db.URL, 0); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, seedOldMarkers); err != nil {
		t.Fatal(err)
	}
	original := tableDigests(t, db)

	measured, err := runTasima(t, db, "skynotifytest", "veri-olcum.sql", true)
	if err != nil {
		t.Fatal(err)
	}
	assertMarkers(t, measured, false)
	for _, want := range []string{
		"yol|mail_approval_events|changes|data-skymail-notrack|$[*].before|1",
		"yol|mail_approvals|body_variables|data-skymail-notrack|$.BodyHtml|1",
		"yol|mail_tasks|body_variables|data-skymail-notrack|$.BodyHtml|2",
		"yol|mail_tasks|source_snapshot|data-skymail-notrack|$.html|1",
		"yol|template_versions|visual_source|skymail.visual|$.type|1",
		"eski_ad|mail_tasks|sent_by|skymail|1",
		"satir|mail_queue|4",
		"risk|mail_queue_erase_after_send|0",
		"bilgi|onay_guncellenecek|1",
		"bilgi|kuyruk_bekleyen_cikis_baglantili|0",
		"sonuc|olcum|tamam",
	} {
		if !slices.Contains(measured, want) {
			t.Errorf("veri-olcum.sql did not print %q:\n%s", want, strings.Join(measured, "\n"))
		}
	}
	for _, line := range measured {
		if strings.HasPrefix(line, "disarida|") {
			t.Errorf("a marker outside the moved columns: %s", line)
		}
	}
	assertDigests(t, db, original, "after veri-olcum.sql")

	// What each file refuses, leaving every row as it was.
	type refusal struct {
		name, file, setup, undo, message string
		withoutOneTransaction            bool
	}
	refuse := func(t *testing.T, refusals []refusal, want map[string]string) {
		t.Helper()
		for _, refusal := range refusals {
			t.Run(refusal.name, func(t *testing.T) {
				if refusal.setup != "" {
					if _, err := db.Pool.Exec(ctx, refusal.setup); err != nil {
						t.Fatal(err)
					}
				}
				_, err := runTasima(t, db, "skynotifytest", refusal.file, !refusal.withoutOneTransaction)
				if refusal.undo != "" {
					if _, undoErr := db.Pool.Exec(ctx, refusal.undo); undoErr != nil {
						t.Fatal(undoErr)
					}
				}
				if err == nil || !strings.Contains(err.Error(), refusal.message) {
					t.Fatalf("%s ran: %v; want it refused with %q", refusal.file, err, refusal.message)
				}
				assertDigests(t, db, want, "after the refusal")
			})
		}
	}
	refuse(t, []refusal{
		{name: "without -1", file: "veri-ileri.sql", message: "tasima", withoutOneTransaction: true},
		{
			name:    "a marker in a column it does not move",
			file:    "veri-ileri.sql",
			setup:   `UPDATE mail_approval_events SET note = 'bkz. data-skymail-notrack' WHERE seq = 1`,
			undo:    `UPDATE mail_approval_events SET note = NULL WHERE seq = 1`,
			message: "mail_approval_events.note: taşınmayan sütunda eski kalıp data-skymail-notrack",
		},
		{
			name:    "a marker in another letter case",
			file:    "veri-ileri.sql",
			setup:   `UPDATE mail_queue SET body_html = body_html || '<a DATA-SKYMAIL-NOTRACK href="https://h.example">h</a>' WHERE status = 'pending' AND recipient_email = 'ali@example.com'`,
			undo:    `UPDATE mail_queue SET body_html = replace(body_html, '<a DATA-SKYMAIL-NOTRACK href="https://h.example">h</a>', '') WHERE recipient_email = 'ali@example.com'`,
			message: "başka harf büyüklüğüyle",
		},
		{
			name:    "the Visual type below the top level",
			file:    "veri-ileri.sql",
			setup:   `UPDATE template_versions SET visual_source = jsonb_set(visual_source, '{reason}', '"skymail.visual"') WHERE seq = 2`,
			undo:    `UPDATE template_versions SET visual_source = visual_source - 'reason' WHERE seq = 2`,
			message: "üst düzey türü",
		},
		{
			name: "a sent row the erase trigger would empty",
			file: "veri-ileri.sql",
			setup: `BEGIN;
				SET LOCAL session_replication_role = replica;
				INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status)
				VALUES ('5e000000-0000-4000-8000-000000000051', '5e000000-0000-4000-8000-000000000044', 'Can Er', 'can@example.com',
				        'Kod', 'f', '<a data-skymail-notrack href="https://f.example">f</a>', 'sent');
				COMMIT;`,
			undo:    `DELETE FROM mail_queue WHERE id = '5e000000-0000-4000-8000-000000000051'`,
			message: "erase_after_send",
		},
		{
			name:    "moving back what is not there",
			file:    "veri-geri.sql",
			message: "yeni kalıp data-skymail-notrack zaten",
		},
	}, original)

	forward, err := runTasima(t, db, "skynotifytest", "veri-ileri.sql", true)
	if err != nil {
		t.Fatal(err)
	}
	if lastLine(forward) != "sonuc|ileri|tamam" {
		t.Fatalf("veri-ileri.sql ended with %q:\n%s", lastLine(forward), strings.Join(forward, "\n"))
	}
	assertMarkers(t, forward, true)
	for _, want := range []string{"guncellenen|mail_queue|body_html|3", "guncellenen|mail_tasks|sent_by|1", "guncellenen|template_versions|visual_source|1"} {
		if !slices.Contains(forward, want) {
			t.Errorf("veri-ileri.sql did not print %q", want)
		}
	}
	var visualType, sentBy, queued string
	if err := db.Pool.QueryRow(ctx, `
		SELECT (SELECT visual_source ->> 'type' FROM template_versions WHERE seq = 2),
		       (SELECT sent_by FROM mail_tasks WHERE id = '5e000000-0000-4000-8000-000000000042'),
		       (SELECT body_html FROM mail_queue WHERE recipient_email = 'can@example.com')`).Scan(&visualType, &sentBy, &queued); err != nil {
		t.Fatal(err)
	}
	if visualType != "skynotify.visual" || sentBy != "skynotify" || queued != `<p><a data-skynotify-notrack href="https://f.example">f</a></p>` {
		t.Errorf("after veri-ileri.sql: type %q, sent_by %q, a queued body %q", visualType, sentBy, queued)
	}
	measuredForward, err := runTasima(t, db, "skynotifytest", "veri-olcum.sql", true)
	if err != nil {
		t.Fatal(err)
	}
	assertMarkers(t, measuredForward, true)
	moved := tableDigests(t, db)

	// Forward again refuses: the new markers are there.
	refuse(t, []refusal{
		{name: "forward twice", file: "veri-ileri.sql", message: "yeni kalıp data-skynotify-notrack zaten"},
		{name: "back without -1", file: "veri-geri.sql", message: "tasima", withoutOneTransaction: true},
	}, moved)

	back, err := runTasima(t, db, "skynotifytest", "veri-geri.sql", true)
	if err != nil {
		t.Fatal(err)
	}
	if lastLine(back) != "sonuc|geri|tamam" {
		t.Fatalf("veri-geri.sql ended with %q:\n%s", lastLine(back), strings.Join(back, "\n"))
	}
	assertMarkers(t, back, false)
	assertDigests(t, db, original, "after forward and back")

	if _, err := runTasima(t, db, "skynotifytest", "veri-ileri.sql", true); err != nil {
		t.Fatal(err)
	}
	assertDigests(t, db, moved, "forward again")
}

// veri-geri.sql is veri-ileri.sql with its markers the other way round, and
// both count as veri-olcum.sql counts.
func TestDataMigrationFilesShareTheirBody(t *testing.T) {
	section := func(name, from, to string) string {
		t.Helper()
		body := string(tasimaFile(t, name))
		start := strings.Index(body, from)
		if start < 0 {
			t.Fatalf("%s has no %q", name, from)
		}
		end := len(body)
		if to != "" {
			end = strings.Index(body, to)
			if end < start {
				t.Fatalf("%s has no %q after %q", name, to, from)
			}
		}
		return body[start:end]
	}
	const body = "-- ===== ortak gövde"
	if section("veri-ileri.sql", body, "") != section("veri-geri.sql", body, "") {
		t.Error("veri-ileri.sql and veri-geri.sql differ below " + body)
	}
	count := section("veri-olcum.sql", "-- sayım başı", "-- sayım sonu")
	for _, name := range []string{"veri-ileri.sql", "veri-geri.sql"} {
		if section(name, "-- sayım başı", "-- sayım sonu") != count {
			t.Errorf("%s counts otherwise than veri-olcum.sql", name)
		}
	}
	variables := func(name string) []string {
		t.Helper()
		var lines []string
		for _, line := range strings.Split(section(name, `\set yon`, body), "\n") {
			if strings.HasPrefix(line, `\set `) {
				lines = append(lines, line)
			}
		}
		return lines
	}
	forward, back := variables("veri-ileri.sql"), variables("veri-geri.sql")
	if len(forward) != 7 || len(back) != 7 {
		t.Fatalf("variables: %v, %v", forward, back)
	}
	swap := strings.NewReplacer("eski_", "yeni_", "yeni_", "eski_", "'ileri'", "'geri'")
	slices.Sort(back)
	var swapped []string
	for _, line := range forward {
		swapped = append(swapped, swap.Replace(line))
	}
	slices.Sort(swapped)
	if !slices.Equal(swapped, back) {
		t.Errorf("veri-geri.sql's markers are not veri-ileri.sql's the other way round:\n%v\n%v", swapped, back)
	}
}
