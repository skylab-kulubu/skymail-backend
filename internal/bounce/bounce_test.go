package bounce_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/bounce"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

const fqdn = "mail.example.org"

// The rows the dsn package's fixtures name.
var (
	notFoundRow = uuid.MustParse("6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b") // yok@std.example.org, 5.1.10
	edgeRow     = uuid.MustParse("0b3e6c1a-2d4f-4a7b-8c9d-e0f1a2b3c4d5") // mezun@std.example.org, 5.4.1 AS(201806281)
	delayedRow  = uuid.MustParse("2a7f0e4c-91b3-4d6e-a5f8-3c2b1a0d9e87") // uye@uzak.example.net, delayed
	fullRow     = uuid.MustParse("9c8b7a6d-5e4f-4321-8fed-cba987654321") // dolu@uzak.example.net, 5.2.2
	aliasRow    = uuid.MustParse("d4c3b2a1-0f9e-4d8c-b7a6-a5b4c3d2e1f0") // takma-ad@std.example.org via a forwarding
)

const (
	task       = "40000000-0000-4000-8000-000000000001"
	personSub  = "3f1c9d70-5b8e-4c1a-9d2e-0a1b2c3d4e5f"
	sentAt     = "2026-10-20T09:59:59.700Z"
	receivedAt = "2026-10-20T10:00:01Z"
)

func store(t *testing.T) *database.Store {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES ('`+task+`', 'service-account-keycloak-mailer', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, sent_at, recipient_sub) VALUES
		    ('`+notFoundRow.String()+`', '`+task+`', 'Yok', 'Yok@Std.Example.org', 'Giriş kodun', 'x', 'sent', '`+sentAt+`', '`+personSub+`'),
		    ('`+edgeRow.String()+`', '`+task+`', 'Mezun', 'mezun@std.example.org', 'Duyuru', 'x', 'sent', '`+sentAt+`', NULL),
		    ('`+delayedRow.String()+`', '`+task+`', 'Üye', 'uye@uzak.example.net', 'Toplantı', 'x', 'sent', '`+sentAt+`', NULL),
		    ('`+fullRow.String()+`', '`+task+`', 'Dolu', 'dolu@uzak.example.net', 'Duyuru', 'x', 'sent', '`+sentAt+`', NULL),
		    ('`+aliasRow.String()+`', '`+task+`', 'Takma', 'takma-ad@std.example.org', 'Duyuru', 'x', 'sent', '`+sentAt+`', NULL)`); err != nil {
		t.Fatal(err)
	}
	return database.NewStore(postgres.Pool)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "dsn", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func process(t *testing.T, db *database.Store, file, id string) bounce.Outcome {
	t.Helper()
	return processRaw(t, db, fixture(t, file), id, mustTime(receivedAt))
}

func processRaw(t *testing.T, db *database.Store, raw []byte, id string, received time.Time) bounce.Outcome {
	t.Helper()
	outcome, err := bounce.Process(context.Background(), db, fqdn, bounce.NDR{ID: id, ReceivedAt: received, MIME: bytes.NewReader(raw)})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return outcome
}

func mustTime(s string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return at
}

type suppression struct {
	address, source, reason string
	sub                     *string
	count                   int
	lastQueue               *uuid.UUID
}

func suppressions(t *testing.T, db *database.Store) map[string]suppression {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(),
		`SELECT address, source, reason_code, recipient_sub::text, count, last_queue_id FROM mail_suppressions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	all := map[string]suppression{}
	for rows.Next() {
		var s suppression
		if err := rows.Scan(&s.address, &s.source, &s.reason, &s.sub, &s.count, &s.lastQueue); err != nil {
			t.Fatal(err)
		}
		all[s.address] = s
	}
	return all
}

func count(t *testing.T, db *database.Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.Conn.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// One permanent failure of the row's own address suppresses it at once
// (hub spec §0 K4): its address as the row has it, lower-cased, the
// subject the row knew, the code. The bounce keeps codes only.
func TestAPermanentFailureSuppressesTheAddress(t *testing.T) {
	db := store(t)
	if got := process(t, db, "exchange-recipient-not-found.eml", "<ndr-1@outlook.example>"); got != bounce.OutcomeHard {
		t.Fatalf("outcome = %q, want %q", got, bounce.OutcomeHard)
	}
	if got := process(t, db, "exchange-edge-blocking.eml", "<ndr-2@outlook.example>"); got != bounce.OutcomeHard {
		t.Fatalf("edge blocking: outcome = %q, want %q", got, bounce.OutcomeHard)
	}
	all := suppressions(t, db)
	if len(all) != 2 {
		t.Fatalf("suppressions = %+v, want two", all)
	}
	got := all["yok@std.example.org"]
	if got.source != "dsn" || got.reason != "5.1.10" || got.count != 1 || got.sub == nil || *got.sub != personSub ||
		got.lastQueue == nil || *got.lastQueue != notFoundRow {
		t.Errorf("suppression = %+v", got)
	}
	if got := all["mezun@std.example.org"]; got.reason != "5.4.1 AS(201806281)" || got.sub != nil {
		t.Errorf("edge blocking suppression = %+v", got)
	}

	var kind, action, status, as, hash string
	var smtp int
	var matched bool
	if err := db.Conn.QueryRow(context.Background(), `
		SELECT kind, action, status, smtp_code, COALESCE(exo_as_code, ''), recipient_matched, ndr_id_sha256
		FROM mail_bounces WHERE queue_id = $1`, notFoundRow).Scan(&kind, &action, &status, &smtp, &as, &matched, &hash); err != nil {
		t.Fatal(err)
	}
	if kind != "hard" || action != "failed" || status != "5.1.10" || smtp != 550 || as != "" || !matched || len(hash) != 64 {
		t.Errorf("bounce = %s %s %s %d %q %v %s", kind, action, status, smtp, as, matched, hash)
	}
	// Nothing of the report's text or the address is kept with the bounce.
	var row string
	if err := db.Conn.QueryRow(context.Background(), `SELECT row_to_json(b)::text FROM mail_bounces b WHERE queue_id = $1`, notFoundRow).Scan(&row); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"yok", "RESOLVER", "outlook", "ndr-1"} {
		if strings.Contains(strings.ToLower(row), strings.ToLower(secret)) {
			t.Errorf("bounce %s holds %q", row, secret)
		}
	}
}

// The same report twice (the reader read it again) changes nothing; another
// report about the same address counts it again.
func TestAReportIsRecordedOnce(t *testing.T) {
	db := store(t)
	process(t, db, "exchange-recipient-not-found.eml", "<ndr-1@outlook.example>")
	if got := process(t, db, "exchange-recipient-not-found.eml", "<ndr-1@outlook.example>"); got != bounce.OutcomeDuplicate {
		t.Errorf("again: outcome = %q, want %q", got, bounce.OutcomeDuplicate)
	}
	if got := suppressions(t, db)["yok@std.example.org"].count; got != 1 {
		t.Errorf("count = %d after a repeat, want 1", got)
	}
	if got := process(t, db, "exchange-recipient-not-found.eml", "<ndr-3@outlook.example>"); got != bounce.OutcomeHard {
		t.Errorf("another report: outcome = %q, want %q", got, bounce.OutcomeHard)
	}
	if got := suppressions(t, db)["yok@std.example.org"].count; got != 2 {
		t.Errorf("count = %d after a second report, want 2", got)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_bounces`); n != 2 {
		t.Errorf("bounces = %d, want 2", n)
	}

	// Without the mailbox's id, the report's own Message-ID is the id.
	if got := process(t, db, "exchange-delayed.eml", ""); got != bounce.OutcomeDelay {
		t.Errorf("outcome = %q", got)
	}
	if got := process(t, db, "exchange-delayed.eml", ""); got != bounce.OutcomeDuplicate {
		t.Errorf("again without an id: outcome = %q, want %q", got, bounce.OutcomeDuplicate)
	}
}

// Any other failure, and a delay, is recorded and never suppresses (§4).
func TestASoftFailureOrADelayIsOnlyRecorded(t *testing.T) {
	db := store(t)
	if got := process(t, db, "remote-mailbox-full.eml", "<ndr-full@uzak.example>"); got != bounce.OutcomeSoft {
		t.Errorf("mailbox full: outcome = %q, want %q", got, bounce.OutcomeSoft)
	}
	if got := process(t, db, "exchange-delayed.eml", "<ndr-delay@outlook.example>"); got != bounce.OutcomeDelay {
		t.Errorf("delayed: outcome = %q, want %q", got, bounce.OutcomeDelay)
	}
	if all := suppressions(t, db); len(all) != 0 {
		t.Errorf("suppressions = %+v, want none", all)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_bounces WHERE (queue_id, kind, action) IN (($1, 'soft', 'failed'), ($2, 'delay', 'delayed'))`,
		fullRow, delayedRow); n != 2 {
		t.Errorf("bounces recorded = %d, want 2", n)
	}
}

// A report that names another address than the row's (a forwarding: the
// mail went to takma-ad@, the report names where it was forwarded) is
// recorded with recipient_matched false and suppresses nothing.
func TestAReportAboutAnotherAddressNeverSuppresses(t *testing.T) {
	db := store(t)
	raw := fixture(t, "exchange-several-recipients.eml")
	// The forwarding's own address is the Original-Recipient: matched.
	if got := processRaw(t, db, raw, "<ndr-alias-1@outlook.example>", mustTime(receivedAt)); got != bounce.OutcomeHard {
		t.Errorf("original recipient: outcome = %q, want %q", got, bounce.OutcomeHard)
	}
	// Without it, the report names only an address the row never had.
	other := bytes.Replace(raw, []byte("Original-Recipient: rfc822;takma-ad@std.example.org\n"), nil, 1)
	if _, err := db.Conn.Exec(context.Background(), `DELETE FROM mail_suppressions`); err != nil {
		t.Fatal(err)
	}
	if got := processRaw(t, db, other, "<ndr-alias-2@outlook.example>", mustTime(receivedAt)); got != bounce.OutcomeOtherRecipient {
		t.Errorf("another address: outcome = %q, want %q", got, bounce.OutcomeOtherRecipient)
	}
	if all := suppressions(t, db); len(all) != 0 {
		t.Errorf("suppressions = %+v, want none", all)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_bounces WHERE queue_id = $1 AND NOT recipient_matched`, aliasRow); n != 1 {
		t.Errorf("unmatched bounces = %d, want 1", n)
	}
}

// A forged or stray report suppresses nothing and records nothing: it must
// name a row that exists, went out by e-mail, was sent, before the report,
// and still has its address (not cleared by Account erasure).
func TestAReportThatMatchesNoSentRowChangesNothing(t *testing.T) {
	raw := fixture(t, "exchange-recipient-not-found.eml")
	for _, tc := range []struct {
		name     string
		setup    string
		mime     []byte
		received time.Time
		want     bounce.Outcome
	}{
		{name: "not a report", mime: fixture(t, "auto-reply.eml"), want: bounce.OutcomeNotDSN},
		{name: "a plain-text bounce", mime: fixture(t, "plain-text-bounce.eml"), want: bounce.OutcomeNotDSN},
		{name: "a mail SkyMail did not send", mime: fixture(t, "rotator-alarm.eml"), want: bounce.OutcomeUnmatched},
		{name: "another SMTP_FQDN", mime: bytes.ReplaceAll(raw, []byte("@mail.example.org>"), []byte("@mail.example.net>")), want: bounce.OutcomeUnmatched},
		{name: "no such row", mime: bytes.ReplaceAll(raw, []byte(notFoundRow.String()), []byte(uuid.NewString())), want: bounce.OutcomeUnmatched},
		{name: "a pending row", setup: `UPDATE mail_queue SET status = 'pending', sent_at = NULL WHERE id = $1`, mime: raw, want: bounce.OutcomeUnmatched},
		{name: "a failed row", setup: `UPDATE mail_queue SET status = 'failed' WHERE id = $1`, mime: raw, want: bounce.OutcomeUnmatched},
		{name: "a row sent by an image without sent_at", setup: `UPDATE mail_queue SET sent_at = NULL WHERE id = $1`, mime: raw, want: bounce.OutcomeUnmatched},
		{name: "a report from before the mail", mime: raw, received: mustTime(sentAt).Add(-time.Hour), want: bounce.OutcomeUnmatched},
		{name: "a row Account erasure cleared", setup: `UPDATE mail_queue SET recipient_email = '', recipient_sub = NULL WHERE id = $1`, mime: raw, want: bounce.OutcomeUnmatched},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := store(t)
			if tc.setup != "" {
				if _, err := db.Conn.Exec(context.Background(), tc.setup, notFoundRow); err != nil {
					t.Fatal(err)
				}
			}
			received := tc.received
			if received.IsZero() {
				received = mustTime(receivedAt)
			}
			if got := processRaw(t, db, tc.mime, "<ndr-x@outlook.example>", received); got != tc.want {
				t.Errorf("outcome = %q, want %q", got, tc.want)
			}
			if n := count(t, db, `SELECT (SELECT count(*) FROM mail_bounces) + (SELECT count(*) FROM mail_suppressions)`); n != 0 {
				t.Errorf("%d rows written, want none", n)
			}
		})
	}
}

// Exchange's own report comes within a second, and Graph gives whole
// seconds: a report a little before sent_at by the clocks still matches.
func TestAReportJustBeforeSentAtMatches(t *testing.T) {
	db := store(t)
	if got := processRaw(t, db, fixture(t, "exchange-recipient-not-found.eml"), "<ndr@outlook.example>",
		mustTime(sentAt).Truncate(time.Second)); got != bounce.OutcomeHard {
		t.Errorf("outcome = %q, want %q", got, bounce.OutcomeHard)
	}
}

func TestAReportWithoutAnIDIsNotRecorded(t *testing.T) {
	db := store(t)
	raw := bytes.Replace(fixture(t, "exchange-recipient-not-found.eml"),
		[]byte("Message-ID:\n <a1b2c3d4-0000-4000-8000-ndr000000001@AM9PR01MB0002.eurprd01.prod.outlook.com>\n"), nil, 1)
	_, err := bounce.Process(context.Background(), db, fqdn, bounce.NDR{MIME: bytes.NewReader(raw)})
	if !errors.Is(err, bounce.ErrNoID) {
		t.Errorf("err = %v, want ErrNoID", err)
	}
}

// Account erasure clears the row and deletes the suppression; a report that
// arrives after it finds the row cleared and suppresses nothing again.
func TestAReportAfterAccountErasureSuppressesNothing(t *testing.T) {
	db := store(t)
	process(t, db, "exchange-recipient-not-found.eml", "<ndr-1@outlook.example>")
	if _, err := db.EraseAccount(context.Background(), database.AccountErasure{
		RequestID: uuid.New(), Subject: personSub, Emails: []string{"yok@std.example.org"},
	}); err != nil {
		t.Fatal(err)
	}
	if all := suppressions(t, db); len(all) != 0 {
		t.Fatalf("suppressions after erasure = %+v", all)
	}
	if got := process(t, db, "exchange-recipient-not-found.eml", "<ndr-late@outlook.example>"); got != bounce.OutcomeUnmatched {
		t.Errorf("late report: outcome = %q, want %q", got, bounce.OutcomeUnmatched)
	}
	if all := suppressions(t, db); len(all) != 0 {
		t.Errorf("suppressions = %+v, want none", all)
	}
	// The bounce stays with the cleared row: codes only.
	if n := count(t, db, `SELECT count(*) FROM mail_bounces WHERE queue_id = $1`, notFoundRow); n != 1 {
		t.Errorf("bounces = %d, want 1", n)
	}
}

// A report holds the row while it records: an erasure that comes meanwhile
// waits for it, then deletes the suppression it wrote.
func TestAccountErasureWaitsForAReportBeingRecorded(t *testing.T) {
	db := store(t)
	ctx := context.Background()
	tx, err := db.Conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.WithTx(tx)
	if _, err := q.LockBounceQueueRow(ctx, notFoundRow); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SuppressQueueRowAddress(ctx, database.SuppressQueueRowAddressParams{QueueID: notFoundRow, Source: "dsn", ReasonCode: "5.1.10"}); err != nil {
		t.Fatal(err)
	}

	erased := make(chan error, 1)
	go func() {
		_, err := db.EraseAccount(ctx, database.AccountErasure{RequestID: uuid.New(), Subject: personSub, Emails: []string{"yok@std.example.org"}})
		erased <- err
	}()
	select {
	case err := <-erased:
		t.Fatalf("erasure did not wait for the report: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-erased; err != nil {
		t.Fatal(err)
	}
	if all := suppressions(t, db); len(all) != 0 {
		t.Errorf("suppressions = %+v, want none", all)
	}
}

// Bounces are kept a year: the content sweep's step deletes older ones.
func TestBouncesAreKeptAYear(t *testing.T) {
	db := store(t)
	ctx := context.Background()
	if _, err := db.Conn.Exec(ctx, `
		INSERT INTO mail_bounces (queue_id, source, kind, action, received_at) VALUES
		    ($1, 'smtp', 'hard', 'failed', now() - interval '366 days'),
		    ($1, 'smtp', 'hard', 'failed', now() - interval '400 days'),
		    ($1, 'smtp', 'hard', 'failed', now() - interval '364 days')`, fullRow); err != nil {
		t.Fatal(err)
	}
	deleted, err := db.DeleteMailBounces(ctx, bounce.RetentionDays, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d, want the two older than a year", deleted)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_bounces`); n != 1 {
		t.Errorf("left %d, want 1", n)
	}
}

// The tables refuse what they must never hold.
func TestTheTablesRefuseWhatTheyMustNotHold(t *testing.T) {
	db := store(t)
	ctx := context.Background()
	for name, sql := range map[string]string{
		"an address in capitals":         `INSERT INTO mail_suppressions (address, source, reason_code) VALUES ('Yok@example.org', 'dsn', '5.1.1')`,
		"text in the reason":             `INSERT INTO mail_suppressions (address, source, reason_code) VALUES ('yok@example.org', 'dsn', '5.1.1 user yok@example.org unknown')`,
		"an unknown source":              `INSERT INTO mail_suppressions (address, source, reason_code) VALUES ('yok@example.org', 'manual', '5.1.1')`,
		"a report without its id":        `INSERT INTO mail_bounces (queue_id, source, kind, action) VALUES ('` + fullRow.String() + `', 'dsn', 'soft', 'failed')`,
		"a refusal not matched":          `INSERT INTO mail_bounces (queue_id, source, kind, action, recipient_matched) VALUES ('` + fullRow.String() + `', 'smtp', 'hard', 'failed', false)`,
		"a status that is not a code":    `INSERT INTO mail_bounces (queue_id, source, kind, action, status) VALUES ('` + fullRow.String() + `', 'smtp', 'hard', 'failed', 'user unknown')`,
		"a delivered action":             `INSERT INTO mail_bounces (queue_id, source, kind, action) VALUES ('` + fullRow.String() + `', 'smtp', 'hard', 'delivered')`,
		"an AS token that is not digits": `INSERT INTO mail_bounces (queue_id, source, kind, action, exo_as_code) VALUES ('` + fullRow.String() + `', 'smtp', 'hard', 'failed', 'x')`,
	} {
		if _, err := db.Conn.Exec(ctx, sql); err == nil {
			t.Errorf("%s was written", name)
		}
	}
}

// A dry run (the reader's SKYMAIL_BOUNCE_READER=dry-run) says what the
// report would do and writes nothing: no bounce, no suppression. The same
// report then processed for real does it.
func TestADryRunSaysWhatAReportWouldDoAndWritesNothing(t *testing.T) {
	db := store(t)
	evaluate := func(file, id string) bounce.Outcome {
		t.Helper()
		outcome, err := bounce.Evaluate(context.Background(), db, fqdn, bounce.NDR{ID: id, ReceivedAt: mustTime(receivedAt), MIME: bytes.NewReader(fixture(t, file))})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		return outcome
	}
	for file, want := range map[string]bounce.Outcome{
		"exchange-recipient-not-found.eml": bounce.OutcomeHard,
		"exchange-delayed.eml":             bounce.OutcomeDelay,
		"remote-mailbox-full.eml":          bounce.OutcomeSoft,
		"auto-reply.eml":                   bounce.OutcomeNotDSN,
		"rotator-alarm.eml":                bounce.OutcomeUnmatched,
	} {
		if got := evaluate(file, "<dry-"+file+">"); got != want {
			t.Errorf("%s: outcome = %q, want %q", file, got, want)
		}
	}
	if n := count(t, db, `SELECT count(*) FROM mail_bounces`); n != 0 {
		t.Errorf("a dry run wrote %d bounces", n)
	}
	if n := count(t, db, `SELECT count(*) FROM mail_suppressions`); n != 0 {
		t.Errorf("a dry run wrote %d suppressions", n)
	}
	if got := process(t, db, "exchange-recipient-not-found.eml", "<dry-exchange-recipient-not-found.eml>"); got != bounce.OutcomeHard {
		t.Errorf("for real after the dry run: outcome = %q, want %q", got, bounce.OutcomeHard)
	}
	// A report recorded before is a duplicate to the dry run too.
	if got := evaluate("exchange-recipient-not-found.eml", "<dry-exchange-recipient-not-found.eml>"); got != bounce.OutcomeDuplicate {
		t.Errorf("dry run of a recorded report: outcome = %q, want %q", got, bounce.OutcomeDuplicate)
	}
}

// A report that says it is a delivery status notification but cannot be read
// is ErrUnreadable: the reader counts it and goes on, it does not try it
// again and again. Its error quotes nothing of the report.
func TestAReportThatCannotBeReadIsErrUnreadable(t *testing.T) {
	db := store(t)
	// More parts than a report has: the parser gives up.
	raw := "Message-ID: <broken@outlook.example>\r\nContent-Type: multipart/report; report-type=delivery-status; boundary=\"b\"\r\n\r\n" +
		strings.Repeat("--b\r\nContent-Type: text/plain\r\n\r\nyok@std.example.org\r\n", 30) + "--b--\r\n"
	_, err := bounce.Process(context.Background(), db, fqdn, bounce.NDR{ID: "<broken@outlook.example>", MIME: strings.NewReader(raw)})
	if !errors.Is(err, bounce.ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable", err)
	}
	if strings.Contains(err.Error(), "yok@") {
		t.Errorf("the error quotes the report: %v", err)
	}
}
