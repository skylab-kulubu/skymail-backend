package migrations_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The newest migration in db/migrations. Distinct from
// migrations.LegacyBaselineVersion, which is the one older schema a running
// database may be adopted at — the two were the same number until templates
// grew keys, and conflating them hid what each test was actually asserting.
const latestMigrationVersion = uint(20260923233000)

func TestRunAppliesAllMigrationsToFreshDatabase(t *testing.T) {
	database := testpostgres.StartDatabase(t)

	version, err := migrations.Run(context.Background(), database.URL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if version != latestMigrationVersion {
		t.Fatalf("version = %d, want %d", version, latestMigrationVersion)
	}

	var columns int
	if err := database.Pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name IN ('templates', 'mailing_lists')
		  AND column_name IN ('archived_at', 'archived_by')
	`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 4 {
		t.Fatalf("archive columns = %d, want 4", columns)
	}

	version, err = migrations.Run(context.Background(), database.URL, 0)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if version != latestMigrationVersion {
		t.Fatalf("second version = %d, want %d", version, latestMigrationVersion)
	}
}

func TestRunRequiresExplicitBaselineForLegacySchema(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	if _, err := database.Pool.Exec(context.Background(), `CREATE TABLE legacy_data (id bigint PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	_, err := migrations.Run(context.Background(), database.URL, 0)
	if err == nil {
		t.Fatal("legacy database unexpectedly migrated without a baseline")
	}
	if !strings.Contains(err.Error(), "DATABASE_MIGRATIONS_BASELINE_VERSION") {
		t.Fatalf("error = %q, want actionable baseline instruction", err)
	}
}

func TestRunBaselinesExistingSchemaWithoutReplayingOldMigrations(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	applyLegacySchema(t, database)

	if _, err := database.Pool.Exec(context.Background(), `
		INSERT INTO templates (name, html_content, plain_text_content, react_email_content, subject)
		VALUES ('Korunacak', '<p>İçerik</p>', 'İçerik', '{}', 'Konu')
	`); err != nil {
		t.Fatal(err)
	}

	version, err := migrations.Run(context.Background(), database.URL, migrations.LegacyBaselineVersion)
	if err != nil {
		t.Fatal(err)
	}
	// Adopted at the baseline, then carried forward: the migrations newer than
	// the baseline still run, the ones it already has do not.
	if version != latestMigrationVersion {
		t.Fatalf("version = %d, want %d", version, latestMigrationVersion)
	}

	var templates int
	if err := database.Pool.QueryRow(context.Background(), `SELECT count(*) FROM templates`).Scan(&templates); err != nil {
		t.Fatal(err)
	}
	if templates != 1 {
		t.Fatalf("templates = %d, want preserved row", templates)
	}
}

func TestRunRejectsBaselineWhenLegacySchemaDoesNotMatch(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	if _, err := database.Pool.Exec(context.Background(), `CREATE TABLE legacy_data (id bigint PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	_, err := migrations.Run(context.Background(), database.URL, migrations.LegacyBaselineVersion)
	if err == nil {
		t.Fatal("mismatched legacy schema unexpectedly accepted")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %q, want schema mismatch", err)
	}
}

// applyLegacySchema builds the database as it stood at the verified baseline —
// migrations newer than the baseline are deliberately left out, because the
// point of adopting a legacy database is that those still have to run.
func applyLegacySchema(t *testing.T, database testpostgres.Database) {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate migration runner test")
	}
	migrationsDir := filepath.Join(filepath.Dir(filename), "..", "..", "db", "migrations")
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, file := range files {
		version := migrationVersion(t, file)
		if version > migrations.LegacyBaselineVersion {
			continue
		}

		migration, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Pool.Exec(context.Background(), string(migration)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}
}

func migrationVersion(t *testing.T, path string) uint {
	t.Helper()
	name := filepath.Base(path)
	digits := strings.SplitN(name, "_", 2)[0]
	version, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		t.Fatalf("parse migration version from %s: %v", name, err)
	}
	return uint(version)
}
