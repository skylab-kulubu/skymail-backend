package mailer_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// fakeSMTP counts the connections a worker makes and hangs up on each, so a
// send fails at once and is rescheduled. Nothing reaches a real relay.
type fakeSMTP struct {
	listener net.Listener
	dials    atomic.Int32
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{listener: listener}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.dials.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })
	return s
}

func (s *fakeSMTP) config() mailer.SMTPConfig {
	return mailer.SMTPConfig{
		FromEmail: "skymail@example.com",
		Host:      "127.0.0.1",
		Port:      s.listener.Addr().(*net.TCPAddr).Port,
		User:      "skymail",
		Password:  "test",
		FQDN:      "example.com",
	}
}

// mailerLogs captures what the mailer logs. The mailer takes its logger from
// the global one when it is made, so capture before NewMailer.
type mailerLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *mailerLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// lines returns every log line as a map, the fields zerolog wrote.
func (l *mailerLogs) lines(t *testing.T) []map[string]any {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

func captureMailerLogs(t *testing.T) *mailerLogs {
	t.Helper()
	logs := &mailerLogs{}
	previous := log.Logger
	log.Logger = zerolog.New(logs).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = previous })
	return logs
}

// restoredQueue is a database as a restored dump leaves it: one row a worker
// had taken (processing) and one still waiting (pending), both due now.
func restoredQueue(t *testing.T) *database.Store {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.Pool.Exec(ctx, `
		INSERT INTO mail_tasks (id, sent_by, body_variables)
		VALUES ('40000000-0000-4000-8000-000000000001', '31ef736f-72da-4a40-8791-d523199cf9f0', '{}');
		INSERT INTO mail_queue (id, task_id, recipient_full_name, recipient_email, subject, body, status, next_attempt_at) VALUES
		    ('50000000-0000-4000-8000-000000000001', '40000000-0000-4000-8000-000000000001', 'Alıcı Bir', 'alici1@example.com',
		     'Konu', 'Gövde', 'processing', NOW() - INTERVAL '1 minute'),
		    ('50000000-0000-4000-8000-000000000002', '40000000-0000-4000-8000-000000000001', 'Alıcı İki', 'alici2@example.com',
		     'Konu', 'Gövde', 'pending', NOW() - INTERVAL '1 minute');`); err != nil {
		t.Fatal(err)
	}
	return database.NewStore(postgres.Pool)
}

type queueRow struct {
	Status   string
	Attempts int
}

func queueRows(t *testing.T, db *database.Store) map[string]queueRow {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(), `SELECT id::text, status::text, attempts FROM mail_queue`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]queueRow{}
	for rows.Next() {
		var id string
		var row queueRow
		if err := rows.Scan(&id, &row.Status, &row.Attempts); err != nil {
			t.Fatal(err)
		}
		got[id] = row
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// startMailer starts the mailer until the test ends.
func startMailer(t *testing.T, m mailer.Mailer, workers int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.Start(ctx, workers)
}

// MAIL_SENDER=paused is how a restore starts SkyMail: the queue a dump brought
// back holds mail to people erased since, and nothing may go out until core
// has replayed its erasures.
func TestPausedStartResetsProcessingRowsAndSendsNothing(t *testing.T) {
	logs := captureMailerLogs(t)
	db := restoredQueue(t)
	smtp := startFakeSMTP(t)

	m := mailer.NewMailer(db, smtp.config(), mailer.SenderPaused)
	if !m.SenderPaused() {
		t.Fatal("SenderPaused() = false for a paused mailer")
	}
	startMailer(t, m, 3)

	// The processing row has no worker to finish it. It goes back to pending,
	// or the erase endpoint would answer 202 for it forever.
	want := map[string]queueRow{
		"50000000-0000-4000-8000-000000000001": {Status: "pending"},
		"50000000-0000-4000-8000-000000000002": {Status: "pending"},
	}
	if got := queueRows(t, db); !equalRows(got, want) {
		t.Fatalf("after start: %v, want %v", got, want)
	}

	// A send wakes the dispatcher; paused, there is none to wake. The on
	// sender picks rows up within milliseconds of a wake (the next test), so
	// two seconds is ample for anything that was going to happen.
	m.Wake()
	time.Sleep(2 * time.Second)

	if n := smtp.dials.Load(); n != 0 {
		t.Fatalf("paused sender dialled SMTP %d times", n)
	}
	if got := queueRows(t, db); !equalRows(got, want) {
		t.Fatalf("after a wait: %v, want %v", got, want)
	}

	var warned bool
	for _, line := range logs.lines(t) {
		if line["component"] == "worker" || line["component"] == "dispatcher" {
			t.Errorf("paused sender started a %v: %v", line["component"], line)
		}
		if line["level"] == "warn" && line["sender"] == "paused" &&
			strings.Contains(line["message"].(string), "MAIL_SENDER") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning names MAIL_SENDER: %v", logs.lines(t))
	}
}

func TestDefaultStartSendsAsBefore(t *testing.T) {
	logs := captureMailerLogs(t)
	db := restoredQueue(t)
	smtp := startFakeSMTP(t)

	m := mailer.NewMailer(db, smtp.config(), mailer.SenderOn)
	if m.SenderPaused() {
		t.Fatal("SenderPaused() = true for the on sender")
	}
	startMailer(t, m, 1)
	m.Wake()

	// Both rows go to the relay, which hangs up: each is rescheduled after one
	// attempt, the processing one too since start put it back to pending.
	deadline := time.Now().Add(20 * time.Second)
	for {
		got := queueRows(t, db)
		if got["50000000-0000-4000-8000-000000000001"].Attempts == 1 &&
			got["50000000-0000-4000-8000-000000000002"].Attempts == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rows never attempted: %v (dials %d)", got, smtp.dials.Load())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := smtp.dials.Load(); n < 2 {
		t.Fatalf("dials = %d, want one per row", n)
	}

	var started bool
	for _, line := range logs.lines(t) {
		if line["level"] == "info" && line["sender"] == "on" && line["message"] == "Starting up" {
			started = true
		}
	}
	if !started {
		t.Fatalf("no startup line says the sender is on: %v", logs.lines(t))
	}
}

func equalRows(got, want map[string]queueRow) bool {
	if len(got) != len(want) {
		return false
	}
	for id, row := range want {
		if got[id] != row {
			return false
		}
	}
	return true
}
