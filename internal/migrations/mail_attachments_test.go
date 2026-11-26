package migrations_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/skynotify-backend/internal/testpostgres"
)

const mailAttachments = uint(20261122150000)

// Mail attachments (ADR 0082): a file per queue row and position, a safe name
// and a size that matches the file; the file goes when the row's outcome is
// written (sent or failed, not a row taken or put back), the record with the
// row; down drops them and leaves the rows.
func TestMailAttachmentsMigrationGoesUpAndDown(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(mailTaskIdempotencyKeys); err != nil {
		t.Fatalf("migrate to %d: %v", mailTaskIdempotencyKeys, err)
	}
	if err := runner.Migrate(mailAttachments); err != nil {
		t.Fatalf("up: %v", err)
	}
	var task string
	if err := database.Pool.QueryRow(ctx, `INSERT INTO mail_tasks (sent_by, body_variables) VALUES ('core', '{}') RETURNING id`).Scan(&task); err != nil {
		t.Fatal(err)
	}
	row := func() string {
		var id string
		if err := database.Pool.QueryRow(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body)
			VALUES ($1, 'Ayşe Kaya', 'ayse@example.com', 'Biletin hazır', 'Ekte') RETURNING id`, task).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	attach := func(queue string, position int, filename string, size int, content string) error {
		_, err := database.Pool.Exec(ctx, `
			INSERT INTO mail_attachments (queue_id, position, filename, content_type, size_bytes, content)
			VALUES ($1, $2, $3, 'application/pdf', $4, $5::bytea)`, queue, position, filename, size, content)
		return err
	}
	kept := func(queue string) (records, files int) {
		if err := database.Pool.QueryRow(ctx, `SELECT count(*), count(content) FROM mail_attachments WHERE queue_id = $1`, queue).Scan(&records, &files); err != nil {
			t.Fatal(err)
		}
		return records, files
	}
	setStatus := func(queue, status string) {
		if _, err := database.Pool.Exec(ctx, `UPDATE mail_queue SET status = $2::mail_queue_status WHERE id = $1`, queue, status); err != nil {
			t.Fatal(err)
		}
	}

	sent, failed, gone := row(), row(), row()
	for _, queue := range []string{sent, failed, gone} {
		if err := attach(queue, 1, "bilet-gecekodu-2026.pdf", 8, "%PDF-1.7"); err != nil {
			t.Fatalf("an attachment: %v", err)
		}
	}
	for _, c := range []struct {
		name     string
		position int
		filename string
		size     int
	}{
		{"the same position again", 1, "bilet.pdf", 8},
		{"a name with a path", 2, "../bilet.pdf", 8},
		{"a name with a space", 2, "bilet 1.pdf", 8},
		{"a name starting with a dot", 2, ".bilet.pdf", 8},
		{"a size that is not the file's", 2, "bilet.pdf", 9},
		{"position 0", 0, "bilet.pdf", 8},
	} {
		if err := attach(sent, c.position, c.filename, c.size, "%PDF-1.7"); err == nil {
			t.Errorf("%s was written", c.name)
		}
	}

	// Taken and put back: still to go out, the file stays.
	setStatus(sent, "processing")
	setStatus(sent, "pending")
	if records, files := kept(sent); records != 1 || files != 1 {
		t.Fatalf("a row taken and put back keeps %d records, %d files; want the file", records, files)
	}
	setStatus(sent, "processing")
	setStatus(sent, "sent")
	setStatus(failed, "failed")
	for name, queue := range map[string]string{"sent": sent, "failed": failed} {
		if records, files := kept(queue); records != 1 || files != 0 {
			t.Errorf("%s: %d records, %d files; want the record without its file", name, records, files)
		}
	}
	if _, err := database.Pool.Exec(ctx, `DELETE FROM mail_queue WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	if records, _ := kept(gone); records != 0 {
		t.Errorf("a deleted row's attachment stayed")
	}

	if err := runner.Migrate(mailTaskIdempotencyKeys); err != nil {
		t.Fatalf("down: %v", err)
	}
	var table, rows int
	if err := database.Pool.QueryRow(ctx, `SELECT count(to_regclass('mail_attachments')), (SELECT count(*) FROM mail_queue)`).Scan(&table, &rows); err != nil {
		t.Fatal(err)
	}
	if table != 0 || rows != 2 {
		t.Errorf("down: table %d, rows %d; want no table and the two rows", table, rows)
	}
	setStatus(failed, "pending")
	setStatus(failed, "sent")
}
