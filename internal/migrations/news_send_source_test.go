package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migrations of a send made from a News record (hub ticket duyuru-haber
// 06): its source on mail_tasks and mail_queue, then the two unique indexes,
// each alone in its file (CONCURRENTLY).
const (
	newsSendSource      = uint(20261025190000)
	newsOnceIndex       = uint(20261025190100)
	newsAudienceIndex   = uint(20261025190200)
	newsSendSourceAfter = newsAudienceIndex
)

// Up: the old image's send and row are written as before, NULL source; a News
// send needs its whole source; a mailbox gets one row of a record, a record
// one send per audience. Down drops it all, up brings it back.
func TestNewsSendsKeepTheirSourceAndGoOncePerMailbox(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(ownEventStatement); err != nil {
		t.Fatalf("migrate to %d: %v", ownEventStatement, err)
	}
	var templateID string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO templates (name, key, system, subject, html_content, plain_text_content, react_email_content)
		VALUES ('Haber', 'club.news', true, 'x', '<p>x</p>', 'x', '') RETURNING id::text`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}

	for _, version := range []uint{newsSendSource, newsOnceIndex, newsAudienceIndex} {
		if err := runner.Migrate(version); err != nil {
			t.Fatalf("up to %d (CREATE INDEX CONCURRENTLY outside a transaction): %v", version, err)
		}
	}

	var oldTask string
	if err := database.Pool.QueryRow(ctx, oldImageInsertTask, templateID).Scan(&oldTask); err != nil {
		t.Fatalf("the old image's send: %v", err)
	}
	var kind *string
	if err := database.Pool.QueryRow(ctx, `SELECT source_kind FROM mail_tasks WHERE id = $1`, oldTask).Scan(&kind); err != nil || kind != nil {
		t.Fatalf("the old image's send source_kind = %v (%v), want NULL", kind, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body)
		VALUES ($1, 'A', 'a@example.org', 's', 'b'), ($1, 'A', 'a@example.org', 's', 'b')`, oldTask); err != nil {
		t.Fatalf("the old image's rows (no source, the same address twice): %v", err)
	}

	newsTask := func(audience string) (string, error) {
		var id string
		err := database.Pool.QueryRow(ctx, `
			INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send, tracking_requested,
			                        source_kind, source_ref, source_version, source_snapshot, source_audience, source_counts)
			VALUES ('sub', $1, '{}', 'unset', false, 'news', 'news:yeni-donem', 3, '{"title":"Yeni dönem"}', $2, '{}')
			RETURNING id::text`, templateID, audience).Scan(&id)
		return id, err
	}
	members, err := newsTask("members")
	if err != nil {
		t.Fatalf("a News send: %v", err)
	}
	if _, err := newsTask("members"); err == nil {
		t.Error("a second send of the record to the same audience was written")
	}
	team, err := newsTask("teams:/UYELER/ARGE/WEBLAB")
	if err != nil {
		t.Fatalf("the record to another audience: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send, tracking_requested,
		                        source_kind, source_ref)
		VALUES ('sub', $1, '{}', 'unset', false, 'news', 'news:yarim')`, templateID); err == nil {
		t.Error("a News send without its version, snapshot and audience was written")
	}

	row := func(task, address string) error {
		_, err := database.Pool.Exec(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, source_ref)
			VALUES ($1, 'A', $2, 's', 'b', 'news:yeni-donem')`, task, address)
		return err
	}
	if err := row(team, "ayse@example.org"); err != nil {
		t.Fatalf("the team's row: %v", err)
	}
	if err := row(members, "Ayse@Example.org"); err == nil {
		t.Error("the record's second row to the same mailbox (another case, another send) was written")
	}
	if err := row(members, "mehmet@example.org"); err != nil {
		t.Fatalf("the record's row to another mailbox: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := runner.Steps(-1); err != nil {
			t.Fatalf("down %d: %v", i+1, err)
		}
	}
	var columns int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_name IN ('mail_tasks', 'mail_queue')
		  AND column_name IN ('source_kind', 'source_ref', 'source_version', 'source_snapshot', 'source_audience', 'source_counts')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("after down: %d source columns, want 0", columns)
	}
	if err := runner.Migrate(newsSendSourceAfter); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
