package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that makes a recipient in a reserved domain (reserved_domain)
// skipped, not failed, in a send's status, and the one before it.
const (
	beforeReservedDomainSkipped = uint(20261020130100)
	reservedDomainSkipped       = uint(20261020150000)
)

// A send whose only failed recipients are reserved_domain rows reads as its
// other recipients do; one with any other failed recipient still reads
// failed; one whose every recipient was skipped reads failed (nothing went
// out), at once. Down gives back the old status, pinned search_path and all.
func TestAReservedDomainRecipientDoesNotFailItsSend(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(beforeReservedDomainSkipped); err != nil {
		t.Fatalf("migrate to %d: %v", beforeReservedDomainSkipped, err)
	}
	const reserved = "reserved_domain: not sent, the address is in a domain reserved by RFC 2606/6761"
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES
		    ('70000000-0000-4000-8000-000000000001', 'sub', '{}'),
		    ('70000000-0000-4000-8000-000000000002', 'sub', '{}'),
		    ('70000000-0000-4000-8000-000000000003', 'sub', '{}'),
		    ('70000000-0000-4000-8000-000000000004', 'sub', '{}');
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error) VALUES
		    ('70000000-0000-4000-8000-000000000001', 'A', 'a@yildizskylab.com', 'K', 'G', 'sent', NULL),
		    ('70000000-0000-4000-8000-000000000001', 'S', 'seed@example.com', '', '', 'failed', $1),
		    ('70000000-0000-4000-8000-000000000002', 'A', 'a@yildizskylab.com', 'K', 'G', 'pending', NULL),
		    ('70000000-0000-4000-8000-000000000002', 'S', 'seed@example.com', '', '', 'failed', $1),
		    ('70000000-0000-4000-8000-000000000003', 'S', 'seed@example.com', '', '', 'failed', $1),
		    ('70000000-0000-4000-8000-000000000003', 'B', 'b@yildizskylab.com', 'K', 'G', 'failed', 'smtp_rejected: RCPT TO 550 5.1.1'),
		    ('70000000-0000-4000-8000-000000000003', 'C', 'c@yildizskylab.com', 'K', 'G', 'sent', NULL),
		    ('70000000-0000-4000-8000-000000000004', 'S', 'seed@example.com', '', '', 'failed', $1)`, reserved); err != nil {
		t.Fatal(err)
	}
	statuses := func() map[string]string {
		t.Helper()
		got := map[string]string{}
		for _, task := range []string{"1", "2", "3", "4"} {
			var status string
			if err := database.Pool.QueryRow(ctx, `SELECT mail_task_status($1)`,
				"70000000-0000-4000-8000-00000000000"+task).Scan(&status); err != nil {
				t.Fatal(err)
			}
			got[task] = status
		}
		return got
	}
	check := func(when string, want map[string]string) {
		t.Helper()
		got := statuses()
		for task, status := range want {
			if got[task] != status {
				t.Errorf("%s: send %s = %q, want %q", when, task, got[task], status)
			}
		}
	}
	pinned := func(when string) {
		t.Helper()
		var pins bool
		if err := database.Pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM unnest(p.proconfig) AS setting WHERE setting = 'search_path=pg_catalog, public')
			FROM pg_proc p WHERE p.oid = 'mail_task_status(uuid)'::regprocedure`).Scan(&pins); err != nil {
			t.Fatal(err)
		}
		if !pins {
			t.Errorf("%s: mail_task_status() lost its pinned search_path", when)
		}
	}
	old := map[string]string{"1": "failed", "2": "failed", "3": "failed", "4": "failed"}
	check("before", old)

	if err := runner.Migrate(reservedDomainSkipped); err != nil {
		t.Fatalf("up: %v", err)
	}
	check("after up", map[string]string{"1": "sent", "2": "sending", "3": "failed", "4": "failed"})
	pinned("after up")

	if err := runner.Migrate(beforeReservedDomainSkipped); err != nil {
		t.Fatalf("down: %v", err)
	}
	check("after down", old)
	pinned("after down")
}
