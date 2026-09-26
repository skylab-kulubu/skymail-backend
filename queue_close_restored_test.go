package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// skymail-backend queue-close-restored: the step of a restore from backup
// that closes the queue the dump brought back without sending it
// (docs/data-lifecycle.md, Backup and restore; account erasure ticket 16).

// restoreCutoff is the restore instant the tests pass as --before.
const restoreCutoff = "2026-09-20T12:00:00Z"

func queueCloseEnv(databaseURL, sender string) func(string) string {
	return func(key string) string {
		switch key {
		case "DATABASE_URL":
			return databaseURL
		case "MAIL_SENDER":
			return sender
		}
		return ""
	}
}

func runQueueClose(env func(string) string, args ...string) (int, string) {
	var out bytes.Buffer
	code := runQueueCloseRestored(args, env, &out)
	return code, out.String()
}

func TestQueueCloseRestoredRejectsAMissingOrInvalidBefore(t *testing.T) {
	// Nothing listens here: a command that got past its arguments would fail
	// to connect and exit 1, not 2.
	unreachable := "postgres://skymail:secret@127.0.0.1:1/skymail?sslmode=disable&connect_timeout=1"
	for _, tc := range []struct {
		name string
		args []string
		says string
	}{
		{"missing", nil, "--before is required"},
		{"empty", []string{"--before", ""}, "--before is required"},
		{"without a value", []string{"--before"}, "flag needs an argument"},
		{"without an offset", []string{"--before", "2026-09-20T12:00:00"}, "is not RFC3339"},
		{"a date only", []string{"--before", "2026-09-20"}, "is not RFC3339"},
		{"a space for the T", []string{"--before", "2026-09-20 12:00:00Z"}, "is not RFC3339"},
		{"words", []string{"--before", "yesterday"}, "is not RFC3339"},
		{"in the future", []string{"--before", "2999-01-01T00:00:00Z"}, "is in the future"},
		{"a stray argument", []string{"--before", restoreCutoff, "now"}, `unexpected argument "now"`},
		{"an unknown flag", []string{"--before", restoreCutoff, "--force"}, "flag provided but not defined"},
	} {
		for _, apply := range []bool{false, true} {
			args := tc.args
			name := tc.name
			if apply {
				args = append([]string{"--apply"}, tc.args...)
				name += " with --apply"
			}
			t.Run(name, func(t *testing.T) {
				code, out := runQueueClose(queueCloseEnv(unreachable, "paused"), args...)
				if code != 2 {
					t.Fatalf("exit = %d, want 2:\n%s", code, out)
				}
				if !strings.Contains(out, tc.says) {
					t.Errorf("output does not say %q:\n%s", tc.says, out)
				}
				if strings.Contains(out, "secret") || strings.Contains(out, "postgres://") {
					t.Errorf("output shows the database URL:\n%s", out)
				}
			})
		}
	}
}

func TestRestoreInstantReadsEveryRFC3339Offset(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	want := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{restoreCutoff, "2026-09-20T15:00:00+03:00", "2026-09-20T12:00:00.000Z", " " + restoreCutoff + " "} {
		got, err := parseRestoreInstant(raw, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseRestoreInstant(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := parseRestoreInstant(now.Format(time.RFC3339), now); err != nil {
		t.Errorf("now itself is refused: %v", err)
	}
}

// restoredQueueFixture is a queue as a dump brings it back, cut at
// restoreCutoff, with mail queued since beside it:
//   - a list send: a row pending, one processing and one sent before the cut;
//   - a single send pending before it, and a row with no created_at;
//   - a send queued since: a row at the cut exactly, one after it;
//   - an older send that failed for good, with an error of its own.
//
// Ayşe Kaya is the recipient throughout: the command must never print her.
const restoredQueueFixture = `
INSERT INTO mail_tasks (id, sent_by, body_variables) VALUES
    ('41000000-0000-4000-8000-000000000001', '8d4f2c1e-7a6b-4c5d-9e8f-000000000001', '{}'),
    ('41000000-0000-4000-8000-000000000002', '8d4f2c1e-7a6b-4c5d-9e8f-000000000001', '{}'),
    ('41000000-0000-4000-8000-000000000003', '8d4f2c1e-7a6b-4c5d-9e8f-000000000001', '{}'),
    ('41000000-0000-4000-8000-000000000004', '8d4f2c1e-7a6b-4c5d-9e8f-000000000001', '{}');
INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status, error,
                        attempts, created_at) VALUES
    ('51000000-0000-4000-8000-0000000000a1', '41000000-0000-4000-8000-000000000001', 'Ayşe Kaya', 'ayse@example.com',
     'Bahar şenliği', 'Merhaba Ayşe Kaya', '<p>Merhaba Ayşe Kaya</p>', 'pending', NULL, 0, '2026-09-19T08:00:00Z'),
    ('51000000-0000-4000-8000-0000000000a2', '41000000-0000-4000-8000-000000000001', 'Ayşe Kaya', 'ayse@kaya.example',
     'Bahar şenliği', 'Merhaba Ayşe Kaya', '<p>Merhaba Ayşe Kaya</p>', 'processing', 'timeout', 1, '2026-09-19T20:00:00Z'),
    ('51000000-0000-4000-8000-0000000000a3', '41000000-0000-4000-8000-000000000001', 'Ayşe Kaya', 'ayse@okul.example',
     'Bahar şenliği', 'Merhaba Ayşe Kaya', '<p>Merhaba Ayşe Kaya</p>', 'sent', NULL, 1, '2026-09-19T08:00:00Z'),
    ('51000000-0000-4000-8000-0000000000b1', '41000000-0000-4000-8000-000000000002', 'Ayşe Kaya', 'ayse@example.com',
     'Parola sıfırlama', 'Bağlantın', '<p>Bağlantın</p>', 'pending', NULL, 0, '2026-09-18T07:00:00Z'),
    ('51000000-0000-4000-8000-0000000000b2', '41000000-0000-4000-8000-000000000002', 'Ayşe Kaya', 'ayse@example.com',
     'Parola sıfırlama', 'Bağlantın', '<p>Bağlantın</p>', 'pending', NULL, 0, NULL),
    ('51000000-0000-4000-8000-0000000000c1', '41000000-0000-4000-8000-000000000003', 'Ayşe Kaya', 'ayse@example.com',
     'Doğrulama', 'Bağlantın', '<p>Bağlantın</p>', 'pending', NULL, 0, '` + restoreCutoff + `'),
    ('51000000-0000-4000-8000-0000000000c2', '41000000-0000-4000-8000-000000000003', 'Ayşe Kaya', 'ayse@kaya.example',
     'Doğrulama', 'Bağlantın', '<p>Bağlantın</p>', 'pending', NULL, 0, '2026-09-21T09:00:00Z'),
    ('51000000-0000-4000-8000-0000000000d1', '41000000-0000-4000-8000-000000000004', 'Ayşe Kaya', 'ayse@example.com',
     'Eski', 'Merhaba', '<p>Merhaba</p>', 'failed', '550 unknown user', 4, '2026-09-17T10:00:00Z');`

var (
	restoredListSend   = uuid.MustParse("41000000-0000-4000-8000-000000000001")
	restoredSingleSend = uuid.MustParse("41000000-0000-4000-8000-000000000002")
	sendQueuedSince    = uuid.MustParse("41000000-0000-4000-8000-000000000003")
	sendFailedBefore   = uuid.MustParse("41000000-0000-4000-8000-000000000004")
)

func startRestoredQueue(t *testing.T) testpostgres.Database {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, restoredQueueFixture); err != nil {
		t.Fatal(err)
	}
	return postgres
}

// queueSnapshot is every queue row and send, whole, in one string.
func queueSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT COALESCE(string_agg(to_jsonb(q)::text, E'\n' ORDER BY q.id), '') FROM mail_queue q) || E'\n' ||
		       (SELECT COALESCE(string_agg(to_jsonb(t)::text, E'\n' ORDER BY t.id), '') FROM mail_tasks t)`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

type queueRowState struct {
	status   string
	error    string
	attempts int
}

func queueRowStates(t *testing.T, pool *pgxpool.Pool) map[string]queueRowState {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT right(id::text, 2), status::text, COALESCE(error, ''), attempts FROM mail_queue`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	states := map[string]queueRowState{}
	for rows.Next() {
		var id string
		var state queueRowState
		if err := rows.Scan(&id, &state.status, &state.error, &state.attempts); err != nil {
			t.Fatal(err)
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return states
}

// assertPrintsNoPerson checks the output holds counts and times, never a
// recipient, an address or a mail.
func assertPrintsNoPerson(t *testing.T, out string) {
	t.Helper()
	lower := strings.ToLower(out)
	for _, secret := range []string{"ayşe", "kaya", "@", "example", "merhaba", "bağlantın", "şenliği", "sıfırlama"} {
		if strings.Contains(lower, secret) {
			t.Errorf("output shows %q:\n%s", secret, out)
		}
	}
}

const restoredQueueCounts = `before: 2026-09-20T12:00:00Z
pending: 3
processing: 1
tasks: 2
oldest created_at: 2026-09-18T07:00:00Z
newest created_at: 2026-09-19T20:00:00Z
without created_at, counted in: 1
queued at or after --before, left alone: 2
`

func TestQueueCloseRestoredDryRunCountsAndChangesNothing(t *testing.T) {
	postgres := startRestoredQueue(t)
	before := queueSnapshot(t, postgres.Pool)

	code, out := runQueueClose(queueCloseEnv(postgres.URL, "paused"), "--before", restoreCutoff)
	if code != 0 {
		t.Fatalf("exit = %d:\n%s", code, out)
	}
	want := "queue-close-restored: dry run, nothing changed. Add --apply to close these rows.\n" + restoredQueueCounts
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
	assertPrintsNoPerson(t, out)

	// Not paused, a dry run still counts, and says --apply would refuse.
	for sender, says := range map[string]string{
		"":       "note: MAIL_SENDER is not set, so the sender is on; --apply refuses until MAIL_SENDER=paused.\n",
		"on":     `note: MAIL_SENDER is "on"; --apply refuses until MAIL_SENDER=paused.` + "\n",
		"PAUSED": `note: MAIL_SENDER is "PAUSED", neither on nor paused; --apply refuses until MAIL_SENDER=paused.` + "\n",
	} {
		code, out := runQueueClose(queueCloseEnv(postgres.URL, sender), "--before", restoreCutoff)
		if code != 0 || out != want+says {
			t.Errorf("MAIL_SENDER=%q: exit %d, output:\n%s\nwant:\n%s", sender, code, out, want+says)
		}
	}

	if after := queueSnapshot(t, postgres.Pool); after != before {
		t.Fatalf("a dry run changed the queue:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestQueueCloseRestoredApplyRefusesUnlessTheSenderIsPaused(t *testing.T) {
	postgres := startRestoredQueue(t)
	before := queueSnapshot(t, postgres.Pool)

	for sender, says := range map[string]string{
		"":       "MAIL_SENDER is not set, so the sender is on",
		"on":     `MAIL_SENDER is "on"`,
		"PAUSED": `MAIL_SENDER is "PAUSED", neither on nor paused`,
		"off":    `MAIL_SENDER is "off", neither on nor paused`,
	} {
		code, out := runQueueClose(queueCloseEnv(postgres.URL, sender), "--before", restoreCutoff, "--apply")
		if code != 1 {
			t.Errorf("MAIL_SENDER=%q: exit = %d, want 1:\n%s", sender, code, out)
		}
		want := "queue-close-restored: --apply refused: " + says +
			". Set MAIL_SENDER=paused on SkyMail and deploy first; a dry run works either way.\n"
		if out != want {
			t.Errorf("MAIL_SENDER=%q: output:\n%s\nwant:\n%s", sender, out, want)
		}
	}

	if after := queueSnapshot(t, postgres.Pool); after != before {
		t.Fatalf("a refused --apply changed the queue:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestQueueCloseRestoredApplyClosesOnlyWhatWasQueuedBeforeTheRestore(t *testing.T) {
	ctx := context.Background()
	postgres := startRestoredQueue(t)
	store := database.NewStore(postgres.Pool)
	env := queueCloseEnv(postgres.URL, "paused")

	// The instant in any offset: Istanbul's here.
	code, out := runQueueClose(env, "--before", "2026-09-20T15:00:00+03:00", "--apply")
	if code != 0 {
		t.Fatalf("exit = %d:\n%s", code, out)
	}
	want := `queue-close-restored: closed without sending, error "restore: gönderilmedi".` + "\n" + restoredQueueCounts
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
	assertPrintsNoPerson(t, out)

	closed := queueRowState{status: "failed", error: restoreNotSentError}
	processingClosed := queueRowState{status: "failed", error: restoreNotSentError, attempts: 1}
	wantStates := map[string]queueRowState{
		"a1": closed,
		"a2": processingClosed,
		"a3": {status: "sent", attempts: 1},
		"b1": closed,
		"b2": closed,
		"c1": {status: "pending"},
		"c2": {status: "pending"},
		"d1": {status: "failed", error: "550 unknown user", attempts: 4},
	}
	got := queueRowStates(t, postgres.Pool)
	for id, want := range wantStates {
		if got[id] != want {
			t.Errorf("row %s = %+v, want %+v", id, got[id], want)
		}
	}
	if len(got) != len(wantStates) {
		t.Errorf("rows = %v", got)
	}

	// What the screens read: the send list, a send's page and the home screen.
	sends, err := store.ListMailTaskSends(ctx, database.ListMailTaskSendsParams{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	type sendState struct {
		status                          string
		pending, processing, sent, fail int64
	}
	gotSends := map[uuid.UUID]sendState{}
	for _, s := range sends {
		gotSends[s.ID] = sendState{s.Status, s.Pending, s.Processing, s.Sent, s.Failed}
	}
	for id, want := range map[uuid.UUID]sendState{
		restoredListSend:   {status: "failed", sent: 1, fail: 2},
		restoredSingleSend: {status: "failed", fail: 2},
		sendQueuedSince:    {status: "sending", pending: 2},
		sendFailedBefore:   {status: "failed", fail: 1},
	} {
		if gotSends[id] != want {
			t.Errorf("send %s = %+v, want %+v", id, gotSends[id], want)
		}
	}
	summary, err := store.CountMailTasksByStatus(ctx)
	if err != nil || summary != (database.CountMailTasksByStatusRow{Failed: 3, Sending: 1}) {
		t.Errorf("summary = %+v %v", summary, err)
	}
	failed, err := store.GetMailQueueItemsByTaskId(ctx, database.GetMailQueueItemsByTaskIdParams{
		TaskID: restoredListSend,
		Limit:  10,
		Status: database.NullMailQueueStatus{MailQueueStatus: database.MailQueueStatusFailed, Valid: true},
	})
	if err != nil || len(failed) != 2 {
		t.Fatalf("failed recipients = %v %v", failed, err)
	}
	for _, row := range failed {
		if row.Error == nil || *row.Error != restoreNotSentError || row.RecipientEmail == "" {
			t.Errorf("failed recipient %s: error %v, address kept %t", row.ID, row.Error, row.RecipientEmail != "")
		}
	}

	// Once is enough: a second run finds nothing, and leaves what came since.
	settled := queueSnapshot(t, postgres.Pool)
	nothing := `before: 2026-09-20T12:00:00Z
pending: 0
processing: 0
tasks: 0
oldest created_at: -
newest created_at: -
queued at or after --before, left alone: 2
`
	code, out = runQueueClose(env, "--before", restoreCutoff, "--apply")
	if code != 0 || out != `queue-close-restored: closed without sending, error "restore: gönderilmedi".`+"\n"+nothing {
		t.Errorf("second --apply: exit %d:\n%s", code, out)
	}
	code, out = runQueueClose(env, "--before", restoreCutoff)
	if code != 0 || out != "queue-close-restored: dry run, nothing changed. Add --apply to close these rows.\n"+nothing {
		t.Errorf("dry run after --apply: exit %d:\n%s", code, out)
	}
	if after := queueSnapshot(t, postgres.Pool); after != settled {
		t.Fatalf("a second run changed the queue:\nbefore:\n%s\nafter:\n%s", settled, after)
	}
}

// Core replays a completed erasure with emails: [] (account erasure spec §8).
// Paused, the replay waits (202) on another person's queued mail that names the
// erased person, since only a sent or failed mail's body is cleared. Once the
// restored queue is closed, that mail is failed: the replay clears its body by
// name and finishes with 200 (case A). A third person's closed mail, which
// holds nothing of the erased person, is left as it was and can be sent again
// by a new send, the one way SkyMail sends a failed mail again (case B).
func TestErasureReplayFinishesOnceTheRestoredQueueIsClosed(t *testing.T) {
	captureLogs(t)
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	writer, reader := startAccessRedis(t)
	route := newErasureRoute(t, store, accessgate.NewRedisGate(reader, time.Second))

	// Deniz Yılmaz, since erased, wrote a template version: his subject keeps
	// his full name, which is how a replay without addresses finds it. The
	// dump holds three queued mails: Ayşe's notice naming Deniz, Mehmet's
	// greeting, and one to Deniz himself.
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO templates (id, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('10000000-0000-4000-8000-000000000001', 'Duyuru', 'Duyuru', '<p>Merhaba {{.FullName}}</p>',
		        'Merhaba {{.FullName}}', '');
		INSERT INTO template_versions (id, template_id, seq, name, subject, html_source, main_mode, html_content,
		                               plain_text_content, author_kind, author_sub, author_name, created_at, published_at)
		VALUES ('11000000-0000-4000-8000-000000000001', '10000000-0000-4000-8000-000000000001', 1, 'Duyuru', 'Duyuru',
		        '<p>Merhaba {{.FullName}}</p>', 'html', '<p>Merhaba {{.FullName}}</p>', 'Merhaba {{.FullName}}', 'operator',
		        '`+erasureSubject+`', 'Deniz Yılmaz', '2026-09-01T10:00:00Z', '2026-09-01T10:00:00Z');
		UPDATE templates SET published_version_id = '11000000-0000-4000-8000-000000000001'
		WHERE id = '10000000-0000-4000-8000-000000000001';
		INSERT INTO mail_tasks (id, sent_by, template_id, body_variables) VALUES
		    ('42000000-0000-4000-8000-00000000000a', '8d4f2c1e-7a6b-4c5d-9e8f-000000000001', '10000000-0000-4000-8000-000000000001', '{}'),
		    ('42000000-0000-4000-8000-00000000000b', '8d4f2c1e-7a6b-4c5d-9e8f-000000000001', '10000000-0000-4000-8000-000000000001', '{}'),
		    ('42000000-0000-4000-8000-00000000000d', '8d4f2c1e-7a6b-4c5d-9e8f-000000000001', '10000000-0000-4000-8000-000000000001', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, body_html, status,
		                        created_at) VALUES
		    ('52000000-0000-4000-8000-0000000000a1', '42000000-0000-4000-8000-00000000000a', 'Ayşe Kaya', 'ayse@example.com',
		     'Mail onayı', 'Deniz Yılmaz bir gönderim sundu', '<p>Deniz Yılmaz bir gönderim sundu</p>', 'pending',
		     '2026-09-19T08:00:00Z'),
		    ('52000000-0000-4000-8000-0000000000b1', '42000000-0000-4000-8000-00000000000b', 'Mehmet Demir', 'mehmet@example.com',
		     'Duyuru', 'Merhaba Mehmet Demir', '<p>Merhaba Mehmet Demir</p>', 'processing', '2026-09-19T09:00:00Z'),
		    ('52000000-0000-4000-8000-0000000000d1', '42000000-0000-4000-8000-00000000000d', 'Deniz Yılmaz', '`+erasurePersonal+`',
		     'Duyuru', 'Merhaba Deniz Yılmaz', '<p>Merhaba Deniz Yılmaz</p>', 'pending', '2026-09-19T10:00:00Z');`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Set(ctx, accessgate.MarkerKey(erasureSubject), accessgate.MarkerValue, 0).Err(); err != nil {
		t.Fatal(err)
	}
	requestID := uuid.New()
	replay := func() erasureAnswer {
		return route.put(t, "/internal/v1/account-erasures/"+requestID.String(), route.keycloak.token(t, route.keycloak.claims()),
			fiber.MIMEApplicationJSON, erasureCommandBody(requestID, erasureSubject), nil)
	}

	// Paused and not yet closed: Ayşe's queued notice keeps the replay waiting.
	if answer := replay(); answer.status != fiber.StatusAccepted {
		t.Fatalf("replay before closing = %d %s, want 202", answer.status, answer.body)
	}

	if code, out := runQueueClose(queueCloseEnv(postgres.URL, "paused"), "--before", restoreCutoff, "--apply"); code != 0 {
		t.Fatalf("--apply: exit %d:\n%s", code, out)
	}
	mehmetClosed := mailRow(t, postgres.Pool, "52000000-0000-4000-8000-0000000000b1")

	// Case A: the replay clears the notice by name and finishes.
	answer := replay()
	if answer.status != fiber.StatusOK {
		t.Fatalf("replay after closing = %d %s, want 200", answer.status, answer.body)
	}
	var body struct {
		Status string           `json:"status"`
		Counts map[string]int64 `json:"counts"`
	}
	if err := json.Unmarshal(answer.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "completed" || body.Counts[database.AccountErasureBodiesCleared] != 2 {
		t.Fatalf("receipt = %s, want completed with Ayşe's and Deniz's closed mail cleared", answer.body)
	}
	ayse := mailRow(t, postgres.Pool, "52000000-0000-4000-8000-0000000000a1")
	if ayse != (storedMail{"Ayşe Kaya", "ayse@example.com", "", "", "", "failed", restoreNotSentError}) {
		t.Errorf("Ayşe's notice after the replay = %+v", ayse)
	}
	// Deniz's own mail was never sent, and what it said of him is gone. The
	// replay has no address to find the row itself by (spec §8).
	deniz := mailRow(t, postgres.Pool, "52000000-0000-4000-8000-0000000000d1")
	if deniz.status != "failed" || deniz.subject != "" || deniz.body != "" || deniz.html != "" {
		t.Errorf("Deniz's own mail after the replay = %+v", deniz)
	}

	// Case B: Mehmet's closed mail is as the close left it.
	mehmet := mailRow(t, postgres.Pool, "52000000-0000-4000-8000-0000000000b1")
	if mehmet != mehmetClosed ||
		mehmet != (storedMail{"Mehmet Demir", "mehmet@example.com", "Duyuru", "Merhaba Mehmet Demir", "<p>Merhaba Mehmet Demir</p>", "failed", restoreNotSentError}) {
		t.Fatalf("Mehmet's mail after the replay = %+v, closed %+v", mehmet, mehmetClosed)
	}
	// Sending it again is a new send: from the send's failed recipients, with
	// its template and values, to the same person (POST /v1/mail_tasks/single).
	task, err := store.GetMailTaskById(ctx, uuid.MustParse("42000000-0000-4000-8000-00000000000b"))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := store.GetMailQueueItemsByTaskId(ctx, database.GetMailQueueItemsByTaskIdParams{
		TaskID: task.ID,
		Limit:  10,
		Status: database.NullMailQueueStatus{MailQueueStatus: database.MailQueueStatusFailed, Valid: true},
	})
	if err != nil || len(failed) != 1 {
		t.Fatalf("failed recipients = %v %v", failed, err)
	}
	smtp, dials := countingSMTP(t)
	paused := mailer.NewMailer(store, smtp, mailer.SenderPaused)
	again, err := paused.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
		RecipientFullName: failed[0].RecipientFullName,
		RecipientEmail:    failed[0].RecipientEmail,
		TemplateID:        task.TemplateID,
		SentBy:            "8d4f2c1e-7a6b-4c5d-9e8f-000000000001",
		BodyVariables:     task.BodyVariables,
	})
	if err != nil {
		t.Fatal(err)
	}
	var queued storedMail
	if err := postgres.Pool.QueryRow(ctx, `
		SELECT recipient_full_name, recipient_email, subject, body, COALESCE(body_html, ''), status::text, COALESCE(error, '')
		FROM mail_queue WHERE task_id = $1`, again).Scan(&queued.name, &queued.email, &queued.subject, &queued.body,
		&queued.html, &queued.status, &queued.error); err != nil {
		t.Fatal(err)
	}
	if queued != (storedMail{"Mehmet Demir", "mehmet@example.com", "Duyuru", "Merhaba Mehmet Demir", "<p>Merhaba Mehmet Demir</p>", "pending", ""}) {
		t.Errorf("the new send's row = %+v", queued)
	}
	if n := dials.Load(); n != 0 {
		t.Fatalf("paused sender dialled SMTP %d times", n)
	}
}

type storedMail struct {
	name, email, subject, body, html, status, error string
}

func mailRow(t *testing.T, pool *pgxpool.Pool, id string) storedMail {
	t.Helper()
	var row storedMail
	if err := pool.QueryRow(context.Background(), `
		SELECT recipient_full_name, recipient_email, subject, body, COALESCE(body_html, ''), status::text, COALESCE(error, '')
		FROM mail_queue WHERE id = $1`, id).Scan(&row.name, &row.email, &row.subject, &row.body, &row.html, &row.status,
		&row.error); err != nil {
		t.Fatal(err)
	}
	return row
}
