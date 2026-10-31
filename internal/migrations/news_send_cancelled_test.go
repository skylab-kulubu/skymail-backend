package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migrations that stop a cancelled News send from holding the record
// back (review duyuru-haber B3): its cancelled rows give up their source_ref,
// and one send per record and audience counts only the sends not cancelled.
const (
	cancelledNewsRows      = uint(20261031120000)
	newsAudienceOpenIndex  = uint(20261031120100)
	dropNewsAudienceIndex  = uint(20261031120200)
	cancelledNewsSendAfter = dropNewsAudienceIndex
)

// Up: a row an image from before cancelled gives up its source_ref, a sent
// one keeps it; the record goes again to the audience of its cancelled send,
// once. Down puts the old index back once the sends allow it; up again.
func TestACancelledNewsSendNoLongerHoldsTheRecordBack(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(bulkTaskControls); err != nil {
		t.Fatalf("migrate to %d: %v", bulkTaskControls, err)
	}
	var templateID string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO templates (name, key, system, subject, html_content, plain_text_content, react_email_content)
		VALUES ('Haber', 'club.news', true, 'x', '<p>x</p>', 'x', '') RETURNING id::text`).Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	newsTask := func() (string, error) {
		var id string
		err := database.Pool.QueryRow(ctx, `
			INSERT INTO mail_tasks (sent_by, template_id, body_variables, content_sensitive_at_send, tracking_requested,
			                        source_kind, source_ref, source_version, source_snapshot, source_audience, source_counts)
			VALUES ('sub', $1, '{}', 'unset', false, 'news', 'news:yeni-donem', 3, '{"title":"Yeni dönem"}', 'members', '{}')
			RETURNING id::text`, templateID).Scan(&id)
		return id, err
	}
	cancelled, err := newsTask()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, error, source_ref) VALUES
		    ($1, 'A', 'ayse@example.org', 's', 'b', 'sent', NULL, 'news:yeni-donem'),
		    ($1, 'M', 'mehmet@example.org', 's', 'b', 'failed', 'cancelled', 'news:yeni-donem')`, cancelled); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(ctx, `UPDATE mail_tasks SET cancelled_at = now(), cancelled_by = 'sub' WHERE id = $1`, cancelled); err != nil {
		t.Fatal(err)
	}

	for _, version := range []uint{cancelledNewsRows, newsAudienceOpenIndex, dropNewsAudienceIndex} {
		if err := runner.Migrate(version); err != nil {
			t.Fatalf("up to %d: %v", version, err)
		}
	}
	var sentRef, cancelledRef *string
	if err := database.Pool.QueryRow(ctx, `
		SELECT (SELECT source_ref FROM mail_queue WHERE recipient_email = 'ayse@example.org'),
		       (SELECT source_ref FROM mail_queue WHERE recipient_email = 'mehmet@example.org')`).Scan(&sentRef, &cancelledRef); err != nil {
		t.Fatal(err)
	}
	if sentRef == nil || cancelledRef != nil {
		t.Fatalf("source_ref: sent row %v, cancelled row %v; want kept and NULL", sentRef, cancelledRef)
	}
	again, err := newsTask()
	if err != nil {
		t.Fatalf("the record again to the audience of its cancelled send: %v", err)
	}
	if _, err := newsTask(); err == nil {
		t.Error("a third send of the record to the same audience was written")
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, source_ref)
		VALUES ($1, 'M', 'mehmet@example.org', 's', 'b', 'news:yeni-donem')`, again); err != nil {
		t.Fatalf("the cancelled recipient's row of the send again: %v", err)
	}

	// Down needs one send per record and audience again.
	if _, err := database.Pool.Exec(ctx, `DELETE FROM mail_tasks WHERE id = $1`, again); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := runner.Steps(-1); err != nil {
			t.Fatalf("down %d: %v", i+1, err)
		}
	}
	var indexes int
	if err := database.Pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE indexname = 'idx_mail_tasks_news_audience')
		     - count(*) FILTER (WHERE indexname = 'idx_mail_tasks_news_audience_open')
		FROM pg_indexes WHERE tablename = 'mail_tasks'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("after down: the old index and not the new one, %d (%v)", indexes, err)
	}
	if err := runner.Migrate(cancelledNewsSendAfter); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
