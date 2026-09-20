package migrations

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	dbmigrations "github.com/skylab-kulubu/skymail-backend/db/migrations"
)

const LegacyBaselineVersion = uint(20260919180000)

func Run(ctx context.Context, databaseURL string, baselineVersion uint) (uint, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return 0, fmt.Errorf("inspect database before migrations: %w", err)
	}
	defer pool.Close()

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
		if err := pool.QueryRow(ctx, `
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
			if err := validateLegacyBaseline(ctx, pool, baselineVersion); err != nil {
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

func validateLegacyBaseline(ctx context.Context, pool *pgxpool.Pool, version uint) error {
	if version != LegacyBaselineVersion {
		return fmt.Errorf("legacy baseline version %d is unsupported", version)
	}

	var schemaMatches bool
	if err := pool.QueryRow(ctx, `
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
