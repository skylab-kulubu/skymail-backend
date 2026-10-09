package migrations_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

type runResult struct {
	version uint
	err     error
}

func runInBackground(ctx context.Context, url string) <-chan runResult {
	done := make(chan runResult, 1)
	go func() {
		version, err := migrations.Run(ctx, url, 0)
		done <- runResult{version, err}
	}()
	return done
}

// holdMigrationLock is another process in the middle of its migrations: it
// holds the migration lock on a connection of its own until released.
func holdMigrationLock(t *testing.T, url string) (release func()) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, migrations.LockName).Scan(&locked); err != nil || !locked {
		t.Fatalf("take the migration lock: locked=%v err=%v", locked, err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = conn.Close(ctx) }) }
	t.Cleanup(release)
	return release
}

// A task starting while another applies a migration (golang-migrate marks
// the version dirty until it is through) waits for it instead of taking the
// dirty mark for a failed migration and exiting, then finds nothing to do.
func TestRunWaitsForTheMigrationInProgress(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatal(err)
	}
	release := holdMigrationLock(t, database.URL)
	if _, err := database.Pool.Exec(ctx, `UPDATE schema_migrations SET dirty = true`); err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	done := runInBackground(waitCtx, database.URL)
	select {
	case got := <-done:
		t.Fatalf("Run returned (%d, %v) while another process held the migration lock", got.version, got.err)
	case <-time.After(1500 * time.Millisecond):
	}

	// The other process's migration goes through.
	if _, err := database.Pool.Exec(ctx, `UPDATE schema_migrations SET dirty = false`); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case got := <-done:
		if got.err != nil || got.version != latestMigrationVersion {
			t.Fatalf("Run after the other migration = (%d, %v), want (%d, nil)", got.version, got.err, latestMigrationVersion)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not go on once the lock was free")
	}
}

// A migration that really failed (its process gone, the version still
// dirty) still stops startup, as before: it needs a person.
func TestRunStillRefusesADirtyVersionNobodyIsMigrating(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE schema_migrations SET dirty = true`); err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Run(ctx, database.URL, 0); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("Run on a dirty version = %v, want the dirty refusal", err)
	}
}

// The wait for the lock ends with the context (the stop signal, or the
// bound): Run returns an error instead of waiting on.
func TestRunStopsWaitingForTheLockWithItsContext(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	holdMigrationLock(t, database.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := migrations.Run(ctx, database.URL, 0)
	if err == nil || !strings.Contains(err.Error(), "migration lock") {
		t.Fatalf("Run with the lock held elsewhere = %v, want a lock wait error", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Run waited %s past a 700ms context", elapsed)
	}
}

// Tasks starting together on a fresh database (a scale-up, a start-first
// deploy of two replicas) all come up on the latest version.
func TestTasksMigratingAtOnceAllStart(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var runs []<-chan runResult
	for range 3 {
		runs = append(runs, runInBackground(ctx, database.URL))
	}
	for i, done := range runs {
		got := <-done
		if got.err != nil || got.version != latestMigrationVersion {
			t.Errorf("task %d: (%d, %v), want (%d, nil)", i, got.version, got.err, latestMigrationVersion)
		}
	}
}

// DATABASE_URL may carry pgxpool's settings (pool_max_conns, ...), which
// only the pool understands; the migrations' own connections leave them out
// instead of handing them to PostgreSQL as unknown parameters.
func TestRunAcceptsPoolSettingsInTheURL(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	url := database.URL + "&pool_max_conns=5&pool_max_conn_lifetime=1h"
	version, err := migrations.Run(context.Background(), url, 0)
	if err != nil || version != latestMigrationVersion {
		t.Fatalf("Run with pool settings = (%d, %v), want (%d, nil)", version, err, latestMigrationVersion)
	}
}
