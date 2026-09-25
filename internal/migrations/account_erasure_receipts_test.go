package migrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that keeps the proof of each finished Account erasure.
const accountErasureReceipts = uint(20260925200000)

// A receipt is one per request, with a completion time and an object of
// counts; nothing else. Down drops it.
func TestAccountErasureReceiptMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(functionSearchPath); err != nil {
		t.Fatalf("migrate to %d: %v", functionSearchPath, err)
	}
	if err := runner.Migrate(accountErasureReceipts); err != nil {
		t.Fatalf("up: %v", err)
	}

	const request = `'0b6f2a3e-8d1c-4f5e-9a7b-3c2d1e0f9a8b'`
	if _, err := database.Pool.Exec(ctx, `INSERT INTO account_erasure_receipts VALUES (`+request+`, now(), '{"recipients_deleted": 1}')`); err != nil {
		t.Fatalf("a receipt refused: %v", err)
	}
	for statement, want := range map[string]string{
		`INSERT INTO account_erasure_receipts VALUES (` + request + `, now(), '{}')`:                        "23505",
		`INSERT INTO account_erasure_receipts VALUES (gen_random_uuid(), now(), '[1]')`:                     "23514",
		`INSERT INTO account_erasure_receipts VALUES (gen_random_uuid(), NULL, '{}')`:                       "23502",
		`INSERT INTO account_erasure_receipts (request_id, completed_at) VALUES (gen_random_uuid(), now())`: "23502",
	} {
		_, err := database.Pool.Exec(ctx, statement)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != want {
			t.Errorf("%s: err = %v, want SQLSTATE %s", statement, err, want)
		}
	}

	if err := runner.Migrate(functionSearchPath); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left bool
	if err := database.Pool.QueryRow(ctx, `SELECT to_regclass('account_erasure_receipts') IS NOT NULL`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left {
		t.Fatal("down left the receipts table")
	}
}
