package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const (
	beforeNotificationTypes = uint(20261019120000)
	notificationTypes       = uint(20261020130100)
)

// Up makes every template the e-mail variant and lets no other channel in
// yet, on a template or a type (hub ticket iletisim-servisi 03).
func TestNotificationTypesMigrationKeepsEmailTheOnlyChannel(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(beforeNotificationTypes); err != nil {
		t.Fatalf("migrate to %d: %v", beforeNotificationTypes, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO templates (id, key, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('10000000-0000-4000-8000-000000000001', 'event.cancelled', 'İptal', 'K', '<p>x</p>', 'x', '')`); err != nil {
		t.Fatal(err)
	}
	if err := runner.Migrate(notificationTypes); err != nil {
		t.Fatalf("up: %v", err)
	}
	var channel string
	if err := database.Pool.QueryRow(ctx, `SELECT channel FROM templates WHERE id = '10000000-0000-4000-8000-000000000001'`).Scan(&channel); err != nil {
		t.Fatal(err)
	}
	if channel != "email" {
		t.Errorf("an existing template's channel = %q, want email", channel)
	}
	for _, refused := range []string{
		`UPDATE templates SET channel = 'push' WHERE id = '10000000-0000-4000-8000-000000000001'`,
		`INSERT INTO notification_types (key, default_channels) VALUES ('event.cancelled', '{inbox}')`,
		`INSERT INTO notification_types (key, default_channels) VALUES ('event.cancelled', '{}')`,
		`INSERT INTO notification_types (key, lane) VALUES ('event.cancelled', 'fast')`,
		`INSERT INTO notification_types (key, category) VALUES ('event.cancelled', 'Etkinlik')`,
	} {
		if _, err := database.Pool.Exec(ctx, refused); err == nil {
			t.Errorf("accepted: %s", refused)
		}
	}
	var lane string
	var channels []string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO notification_types (key, category) VALUES ('event.cancelled', 'events')
		RETURNING lane, default_channels`).Scan(&lane, &channels); err != nil {
		t.Fatal(err)
	}
	if lane != "bulk" || len(channels) != 1 || channels[0] != "email" {
		t.Errorf("defaults: lane %q channels %v, want bulk [email]", lane, channels)
	}
}
