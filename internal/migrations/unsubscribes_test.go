package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The migration that adds the unsubscribe record beside the suppression list.
const unsubscribes = uint(20261021150000)

// What an image from before it asks of the suppression list, as its sqlc
// generated it (query.sql at f897c98..fb21db8): GetSuppressedAddresses when a
// send is queued, FlagSuppressionsExempt for account mail, the operator
// API's ListSuppressions, CountSuppressions and DeleteSuppression. None of it
// names mail_unsubscribes.
const (
	oldImageSuppressed = `
		SELECT address, source, reason_code
		FROM mail_suppressions
		WHERE address = ANY ($1::text[])`
	oldImageFlagExempt = `
		UPDATE mail_suppressions
		SET exempt_flagged_at = now()
		WHERE address = ANY ($1::text[])`
	oldImageList = `
		SELECT id, address, recipient_sub, source, reason_code, first_seen, last_seen, count, last_queue_id, exempt_flagged_at
		FROM mail_suppressions
		WHERE $1::text IS NULL
		   OR strpos(address, lower($1::text)) > 0
		ORDER BY last_seen DESC, address
		LIMIT $3::int OFFSET $2::int`
	oldImageCount = `
		SELECT count(*)
		FROM mail_suppressions
		WHERE $1::text IS NULL
		   OR strpos(address, lower($1::text)) > 0`
	oldImageDelete = `
		DELETE
		FROM mail_suppressions
		WHERE id = $1
		RETURNING source, reason_code, count, first_seen`
)

// Up adds the two tables and nothing else. With an unsubscribe recorded, an
// image from before reads the suppression list exactly as it did: the
// unsubscribed address is not suppressed for it (its Forms confirmations and
// login codes keep going), its list and count do not show it, and its
// DELETE of the unsubscribe's id removes nothing. Down drops both tables;
// up again brings them back.
func TestAnImageFromBeforeNeverSeesAnUnsubscribe(t *testing.T) {
	database := testpostgres.StartDatabase(t)
	ctx := context.Background()
	runner := migrationRunner(t, database.URL)
	if err := runner.Migrate(listUnsubscribe); err != nil {
		t.Fatalf("migrate to %d: %v", listUnsubscribe, err)
	}
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_suppressions (address, source, reason_code) VALUES ('mehmet@example.com', 'dsn', '5.1.10')`); err != nil {
		t.Fatal(err)
	}
	if err := runner.Migrate(unsubscribes); err != nil {
		t.Fatalf("up: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(ctx, `
		INSERT INTO mail_unsubscribes (address, stream, source) VALUES ('ayse@example.com', 'duyuru', 'link')
		RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("an unsubscribe: %v", err)
	}
	// Mehmet is both bounced and unsubscribed: the old image sees the bounce.
	if _, err := database.Pool.Exec(ctx, `
		INSERT INTO mail_unsubscribes (address, stream, source) VALUES ('mehmet@example.com', 'duyuru', 'link')`); err != nil {
		t.Fatal(err)
	}

	addresses := []string{"ayse@example.com", "mehmet@example.com"}
	rows, err := database.Pool.Query(ctx, oldImageSuppressed, addresses)
	if err != nil {
		t.Fatalf("the old image's GetSuppressedAddresses: %v", err)
	}
	var suppressed []string
	for rows.Next() {
		var address, source, code string
		if err := rows.Scan(&address, &source, &code); err != nil {
			t.Fatal(err)
		}
		suppressed = append(suppressed, address)
	}
	rows.Close()
	if strings.Join(suppressed, ",") != "mehmet@example.com" {
		t.Errorf("the old image suppresses %v, want Mehmet's bounce only", suppressed)
	}
	if tag, err := database.Pool.Exec(ctx, oldImageFlagExempt, addresses); err != nil || tag.RowsAffected() != 1 {
		t.Errorf("the old image's FlagSuppressionsExempt: %v rows, %v; want Mehmet's bounce only", tag.RowsAffected(), err)
	}

	rows, err = database.Pool.Query(ctx, oldImageList, nil, 0, 100)
	if err != nil {
		t.Fatalf("the old image's ListSuppressions: %v", err)
	}
	listed := 0
	for rows.Next() {
		listed++
	}
	rows.Close()
	var counted int
	if err := database.Pool.QueryRow(ctx, oldImageCount, nil).Scan(&counted); err != nil {
		t.Fatalf("the old image's CountSuppressions: %v", err)
	}
	if listed != 1 || counted != 1 {
		t.Errorf("the old image lists %d and counts %d, want the bounce alone", listed, counted)
	}
	if tag, err := database.Pool.Exec(ctx, oldImageDelete, id); err != nil || tag.RowsAffected() != 0 {
		t.Errorf("the old image's DELETE of the unsubscribe's id: %d rows, %v; want nothing removed", tag.RowsAffected(), err)
	}
	var left int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_unsubscribes`).Scan(&left); err != nil || left != 2 {
		t.Errorf("unsubscribes after the old image's calls = %d (%v), want both", left, err)
	}

	for name, sql := range map[string]string{
		"a second record of one address": `INSERT INTO mail_unsubscribes (address, stream, source) VALUES ('ayse@example.com', 'duyuru', 'link')`,
		"an address not lower-case":      `INSERT INTO mail_unsubscribes (address, stream, source) VALUES ('Zeynep@example.com', 'duyuru', 'link')`,
		"the islem stream":               `INSERT INTO mail_unsubscribes (address, stream, source) VALUES ('zeynep@example.com', 'islem', 'link')`,
		"another source":                 `INSERT INTO mail_unsubscribes (address, stream, source) VALUES ('zeynep@example.com', 'duyuru', 'operator')`,
		"an operator without a reason": `INSERT INTO mail_unsubscribe_reactivations (actor, actor_sub, stream, source, unsubscribed_at)
			VALUES ('operator', '31ef736f-72da-4a40-8791-d523199cf9f0', 'duyuru', 'link', now())`,
		"the person with a subject": `INSERT INTO mail_unsubscribe_reactivations (actor, actor_sub, stream, source, unsubscribed_at)
			VALUES ('recipient', '31ef736f-72da-4a40-8791-d523199cf9f0', 'duyuru', 'link', now())`,
	} {
		if _, err := database.Pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s was taken", name)
		}
	}

	tables := func() int {
		t.Helper()
		var n int
		if err := database.Pool.QueryRow(ctx, `
			SELECT count(*) FROM information_schema.tables
			WHERE table_name IN ('mail_unsubscribes', 'mail_unsubscribe_reactivations')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := runner.Migrate(listUnsubscribe); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := tables(); n != 0 {
		t.Errorf("after down: %d tables", n)
	}
	var bounces int
	if err := database.Pool.QueryRow(ctx, `SELECT count(*) FROM mail_suppressions`).Scan(&bounces); err != nil || bounces != 1 {
		t.Errorf("after down: %d bounces (%v), want Mehmet's", bounces, err)
	}
	if _, err := migrations.Run(ctx, database.URL, 0); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := tables(); n != 2 {
		t.Errorf("after up again: %d tables", n)
	}
}
