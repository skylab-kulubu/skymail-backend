package mailer_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/msauth"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

// The workers read a mail's lane from its send's template in the database:
// a login code (a System keycloak.* template) goes out of hesap@ through
// Graph; a list send's row, its lane (duyuru) off, through SMTP AUTH as
// always. Then Graph throttles a login code: the row waits the Retry-After
// Graph asked for, not the first retry's 30 seconds.
func TestWorkersSendEachRowByItsLane(t *testing.T) {
	captureMailerLogs(t)
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	db := database.NewStore(postgres.Pool)
	var template, loginTask, listTask uuid.UUID
	if err := postgres.Pool.QueryRow(ctx, `
		INSERT INTO templates (key, system, name, subject, html_content, plain_text_content, react_email_content)
		VALUES ('keycloak.login-code', true, 'Kod', 'Kodun', '<p>kod</p>', 'kod', '') RETURNING id`).Scan(&template); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, body_variables) VALUES ('keycloak', $1, '{}') RETURNING id`, template).Scan(&loginTask); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Pool.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, body_variables) VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', '{}') RETURNING id`).Scan(&listTask); err != nil {
		t.Fatal(err)
	}
	queue := func(task uuid.UUID, priority mailer.Priority) uuid.UUID {
		t.Helper()
		var id uuid.UUID
		if err := postgres.Pool.QueryRow(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, body_html, priority, next_attempt_at)
			VALUES ($1, 'Alıcı', 'alici@example.net', 'Konu', 'Gövde', '<p>Gövde</p>', $2, NOW() - INTERVAL '1 minute')
			RETURNING id`, task, int16(priority)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	login, list := queue(loginTask, mailer.PrioritySystem), queue(listTask, mailer.PriorityBulk)

	env := decidedEnv()
	env["SKYMAIL_SEND_DUYURU"] = "off"
	lanes := mustLanes(t, env)
	entra, graph, relay := startFakeEntra(t), startFakeGraph(t), startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn, mailer.WithSendLanes(lanes))
	plainSMTP(m)
	mailer.SetGraph(m, graph.server.URL+"/v1.0", graph.server.Client(),
		msauth.NewTokenSource(lanes.Credentials, msauth.ScopeGraph, msauth.WithAuthority(entra.server.URL)))
	startMailer(t, m, 2)
	m.Wake()

	waitFor(t, "both rows sent", 10*time.Second, func() bool {
		return channelRowOf(t, db, login).status == "sent" && channelRowOf(t, db, list).status == "sent"
	})
	calls := graph.taken()
	if len(calls) != 1 || calls[0].path != "/v1.0/users/hesap@bildirim.example.org/sendMail" || calls[0].clientRequestID != login.String() {
		t.Fatalf("Graph took %+v, want the login code from hesap@", calls)
	}
	if mails := relay.taken(); len(mails) != 1 || headerOf(mails[0], "Message-ID") != "<"+list.String()+"@example.com>" {
		t.Fatalf("the relay took %d mails, want the list send", len(mails))
	}

	graph.mu.Lock()
	graph.answer = func(w http.ResponseWriter) bool {
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"ApplicationThrottled","message":"throttled"}}`)
		return true
	}
	graph.mu.Unlock()
	throttled := queue(loginTask, mailer.PrioritySystem)
	m.Wake()
	var row channelRow
	waitFor(t, "the throttled row's retry", 10*time.Second, func() bool {
		row = channelRowOf(t, db, throttled)
		return row.attempts == 1
	})
	var wait time.Duration
	if err := postgres.Pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM next_attempt_at - NOW())::int * INTERVAL '1 second'
		FROM mail_queue WHERE id = $1`, throttled).Scan(&wait); err != nil {
		t.Fatal(err)
	}
	if row.status != "pending" || row.error != "graph_deferred: 429 ApplicationThrottled retry-after 5m0s" || wait < 290*time.Second || wait > 300*time.Second {
		t.Fatalf("the throttled row: %+v, next attempt in %v; want pending in about 5 minutes", row, wait)
	}
}
