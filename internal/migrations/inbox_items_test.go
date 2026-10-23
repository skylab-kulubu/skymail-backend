package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const (
	beforeInboxItems = uint(20261020150000)
	inboxItems       = uint(20261021120000)
)

// Up lets inbox into a type's channels and nothing else new, keeps an item at
// most 90 days and one item per person per notification; down takes inbox
// back out without deleting a type a notification names (hub ticket
// iletisim-servisi 05).
func TestInboxItemsMigrationAddsTheInboxChannelAndTakesItBackOut(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(beforeInboxItems); err != nil {
		t.Fatalf("migrate to %d: %v", beforeInboxItems, err)
	}
	if _, err := database.Pool.Exec(ctx, `INSERT INTO notification_types (key) VALUES ('core.certificate')`); err != nil {
		t.Fatal(err)
	}
	if err := runner.Migrate(inboxItems); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO notification_types (key, default_channels) VALUES ('event.cancelled', '{email,inbox}'), ('team.joined', '{inbox}');
		INSERT INTO notification_variants (type, channel, title) VALUES ('team.joined', 'inbox', 'Ekibe katıldın');
		INSERT INTO notifications (id, type, sent_by, idempotency_key, request_sha256)
		VALUES ('80000000-0000-4000-8000-000000000001', 'team.joined', 'core', 'k1', 'x');
		INSERT INTO inbox_items (user_id, notification_id, notification_type, title, expires_at)
		VALUES ('1a000000-0000-4000-8000-000000000001', '80000000-0000-4000-8000-000000000001', 'team.joined', 'Ekibe katıldın', now() + interval '90 days')`); err != nil {
		t.Fatalf("inbox rows: %v", err)
	}
	for _, refused := range []string{
		`INSERT INTO notification_types (key, default_channels) VALUES ('event.push', '{push}')`,
		`INSERT INTO notification_variants (type, channel, title) VALUES ('team.joined', 'push', 'x')`,
		`INSERT INTO notification_variants (type, channel, title) VALUES ('event.cancelled', 'inbox', '  ')`,
		// Longer than 90 days.
		`INSERT INTO inbox_items (user_id, notification_id, notification_type, title, expires_at)
		 VALUES ('1a000000-0000-4000-8000-000000000002', '80000000-0000-4000-8000-000000000001', 'team.joined', 'x', now() + interval '91 days')`,
		// A second item of one notification for one person.
		`INSERT INTO inbox_items (user_id, notification_id, notification_type, title, expires_at)
		 VALUES ('1a000000-0000-4000-8000-000000000001', '80000000-0000-4000-8000-000000000001', 'team.joined', 'x', now())`,
	} {
		if _, err := database.Pool.Exec(ctx, refused); err == nil {
			t.Errorf("accepted: %s", refused)
		}
	}

	if err := runner.Migrate(beforeInboxItems); err != nil {
		t.Fatalf("down: %v", err)
	}
	rows, err := database.Pool.Query(ctx, `SELECT key, default_channels::text, archived_at IS NOT NULL FROM notification_types ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var key, channels string
		var archived bool
		if err := rows.Scan(&key, &channels, &archived); err != nil {
			t.Fatal(err)
		}
		if archived {
			channels += " archived"
		}
		got[key] = channels
	}
	rows.Close()
	want := map[string]string{"core.certificate": "{email}", "event.cancelled": "{email}", "team.joined": "{email} archived"}
	for key, channels := range want {
		if got[key] != channels {
			t.Errorf("after down %s = %q, want %q", key, got[key], channels)
		}
	}
	var tables int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('inbox_items', 'notification_variants')`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Errorf("tables left after down = %d", tables)
	}
}
