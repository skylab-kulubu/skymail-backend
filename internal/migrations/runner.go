package migrations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	dbmigrations "github.com/skylab-kulubu/skymail-backend/db/migrations"
)

const LegacyBaselineVersion = uint(20260919180000)

// LockName names the session advisory lock (hashtextextended(LockName, 0))
// a process holds from before it reads the migration version until it is
// through. golang-migrate's own lock covers only applying; without this one a
// task starting while another migrates reads the version golang-migrate has
// marked dirty for the migration in progress, or an empty database the other
// is filling, and refuses to start.
const LockName = "skymail-backend migrations"

const (
	// LockWait bounds the wait for another process's migrations. Past it
	// Run fails and the task restarts (a migration that long needs a
	// longer health check start period, docs/health-and-shutdown.md).
	LockWait = 5 * time.Minute
	// lockPoll is how often a waiting process asks for the lock again.
	lockPoll = 500 * time.Millisecond
)

// Run applies the embedded migrations to databaseURL and returns the version
// it is at. It holds LockName throughout, waiting up to LockWait (or until
// ctx ends) for a process that holds it; once it holds the lock it is not
// cut off by ctx, so a migration is never left half done by a stop signal.
func Run(ctx context.Context, databaseURL string, baselineVersion uint) (uint, error) {
	// pgxpool's settings mean nothing to a single connection, nor to
	// golang-migrate's lib/pq, which would send them to PostgreSQL as
	// unknown parameters and fail.
	databaseURL = withoutPoolSettings(databaseURL)
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return 0, fmt.Errorf("inspect database before migrations: %w", err)
	}
	// Closing the connection releases the lock.
	defer func() { _ = conn.Close(context.Background()) }()
	if err := lock(ctx, conn); err != nil {
		return 0, err
	}
	ctx = context.WithoutCancel(ctx)

	source, err := iofs.New(dbmigrations.Files, ".")
	if err != nil {
		return 0, fmt.Errorf("open embedded migrations: %w", err)
	}
	runner, err := migrate.NewWithSourceInstance("iofs", source, databaseURL)
	if err != nil {
		return 0, fmt.Errorf("initialize database migrations: %w", err)
	}
	defer func() {
		_, _ = runner.Close()
	}()

	currentVersion, dirty, versionErr := runner.Version()
	if versionErr != nil && !errors.Is(versionErr, migrate.ErrNilVersion) {
		return 0, fmt.Errorf("read database migration version: %w", versionErr)
	}
	if dirty {
		return 0, fmt.Errorf("database migration version %d is dirty; manual recovery is required", currentVersion)
	}
	if errors.Is(versionErr, migrate.ErrNilVersion) {
		var userTableCount int
		if err := conn.QueryRow(ctx, `
			SELECT count(*)
			FROM information_schema.tables
			WHERE table_schema = 'public'
			  AND table_type = 'BASE TABLE'
			  AND table_name <> 'schema_migrations'
		`).Scan(&userTableCount); err != nil {
			return 0, fmt.Errorf("inspect existing database schema: %w", err)
		}

		if userTableCount > 0 && baselineVersion == 0 {
			return 0, errors.New("database contains an unversioned schema; set DATABASE_MIGRATIONS_BASELINE_VERSION after verifying it")
		}
		if userTableCount == 0 && baselineVersion != 0 {
			return 0, errors.New("DATABASE_MIGRATIONS_BASELINE_VERSION cannot be used with an empty database")
		}
		if baselineVersion != 0 {
			if err := validateLegacyBaseline(ctx, conn, baselineVersion); err != nil {
				return 0, err
			}
			if !embeddedVersionExists(baselineVersion) {
				return 0, fmt.Errorf("baseline migration %d is not embedded in this build", baselineVersion)
			}
			if err := runner.Force(int(baselineVersion)); err != nil {
				return 0, fmt.Errorf("record database migration baseline %d: %w", baselineVersion, err)
			}
		}
	}

	if err := runner.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, fmt.Errorf("apply database migrations: %w", err)
	}

	currentVersion, dirty, err = runner.Version()
	if err != nil {
		return 0, fmt.Errorf("read applied database migration version: %w", err)
	}
	if dirty {
		return 0, fmt.Errorf("database migration version %d is dirty after apply", currentVersion)
	}
	return currentVersion, nil
}

func embeddedVersionExists(version uint) bool {
	prefix := strconv.FormatUint(uint64(version), 10) + "_"
	entries, err := fs.ReadDir(dbmigrations.Files, ".")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".up.sql") {
			return true
		}
	}
	return false
}

// lock takes LockName on conn, waiting for the process that holds it until
// ctx ends or LockWait has passed.
func lock(ctx context.Context, conn *pgx.Conn) error {
	wait, cancel := context.WithTimeout(ctx, LockWait)
	defer cancel()
	for {
		var locked bool
		if err := conn.QueryRow(wait, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, LockName).Scan(&locked); err != nil {
			return fmt.Errorf("take the migration lock: %w", err)
		}
		if locked {
			return nil
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("wait for the migration lock (another process is migrating): %w", wait.Err())
		case <-time.After(lockPoll):
		}
	}
}

// withoutPoolSettings drops pgxpool's pool_* settings from a URL-form
// DATABASE_URL. Anything else, a keyword/value string among them, is
// returned as it is.
func withoutPoolSettings(databaseURL string) string {
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme == "" || parsed.RawQuery == "" {
		return databaseURL
	}
	query := parsed.Query()
	changed := false
	for key := range query {
		if strings.HasPrefix(key, "pool_") {
			query.Del(key)
			changed = true
		}
	}
	if !changed {
		return databaseURL
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func validateLegacyBaseline(ctx context.Context, conn *pgx.Conn, version uint) error {
	if version != LegacyBaselineVersion {
		return fmt.Errorf("legacy baseline version %d is unsupported", version)
	}

	var schemaMatches bool
	if err := conn.QueryRow(ctx, `
		WITH expected_tables(name) AS (
			VALUES
				('templates'),
				('mailing_lists'),
				('recipients'),
				('mailing_list_recipients'),
				('mail_tasks'),
				('mail_queue')
		),
		expected_columns(table_name, column_name) AS (
			VALUES
				('templates', 'subject'),
				('templates', 'archived_at'),
				('templates', 'archived_by'),
				('mailing_lists', 'archived_at'),
				('mailing_lists', 'archived_by'),
				('mail_tasks', 'body_variables'),
				('mail_queue', 'status')
		),
		expected_indexes(name) AS (
			VALUES
				('idx_mail_queue_task_id'),
				('idx_mail_queue_active_jobs'),
				('idx_templates_current_created_at'),
				('idx_templates_archived_at'),
				('idx_mailing_lists_current_created_at'),
				('idx_mailing_lists_archived_at')
		)
		SELECT
			NOT EXISTS (
				SELECT 1 FROM expected_tables expected
				WHERE NOT EXISTS (
					SELECT 1 FROM information_schema.tables actual
					WHERE actual.table_schema = 'public'
					  AND actual.table_name = expected.name
				)
			)
			AND NOT EXISTS (
				SELECT 1 FROM expected_columns expected
				WHERE NOT EXISTS (
					SELECT 1 FROM information_schema.columns actual
					WHERE actual.table_schema = 'public'
					  AND actual.table_name = expected.table_name
					  AND actual.column_name = expected.column_name
				)
			)
			AND NOT EXISTS (
				SELECT 1 FROM expected_indexes expected
				WHERE NOT EXISTS (
					SELECT 1 FROM pg_indexes actual
					WHERE actual.schemaname = 'public'
					  AND actual.indexname = expected.name
				)
			)
			AND NOT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = 'applications'
			)
			AND NOT EXISTS (
				SELECT 1 FROM information_schema.table_constraints
				WHERE constraint_schema = 'public'
				  AND table_name = 'mail_tasks'
				  AND constraint_name = 'mail_tasks_mail_list_id_fkey'
			)
	`).Scan(&schemaMatches); err != nil {
		return fmt.Errorf("validate legacy schema for baseline %d: %w", version, err)
	}
	if !schemaMatches {
		return fmt.Errorf("existing database schema does not match verified baseline %d", version)
	}
	return nil
}
