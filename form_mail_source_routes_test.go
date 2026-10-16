package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	migrationfiles "github.com/skylab-kulubu/skymail-backend/db/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

func formSourcePerson(key string) (approvalPerson, bool) {
	if key == "source-sender" {
		return approvalPerson{sub: "88888888-8888-4888-8888-888888888888", name: "Source Sender", email: "sender@example.invalid", roles: []string{"skymail:access", "skymail:mails:send", "skymail:templates:read"}}, true
	}
	p, ok := approvalPeople[key]
	return p, ok
}

// These tests use the real stores, transactions, renderer and bulk queue;
// no dispatcher is started and no SMTP connection is made. Authentication
// stands in for the already-tested userinfo/service guard.
func newFormSourceWorld(t *testing.T) *approvalWorld {
	t.Helper()
	w := newApprovalWorldWithPool(t, 2)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	api := app.Group("/v1")
	api.Use(func(c fiber.Ctx) error {
		p, ok := formSourcePerson(c.Get("X-Test-User"))
		if !ok {
			return fiber.ErrForbidden
		}
		c.Locals("user_id", p.sub)
		c.Locals("user_name", p.name)
		c.Locals("user_email", p.email)
		c.Locals("roles", p.roles)
		return c.Next()
	})
	h := handlers.NewMailHandler(w.store, w.mail, w.directory, handlers.WithFormSourceLimit(150), handlers.WithFormSourceClock(w.now))
	auth := middlewares.NewAuthMiddleware("skymail", "http://keycloak.invalid", middlewares.AudienceOff)
	api.Post("/form_mail_sources", h.CreateFormSource)
	api.Get("/form_mail_sources/:id", h.GetFormSource)
	registerMailTaskRoutes(api, auth, h)
	registerMailApprovalRoutes(api, auth, w.handler)
	w.app = app
	return w
}

func sourceRequest(owner string, n int) requests.CreateFormMailSource {
	people := make([]requests.MailApprovalRecipient, n)
	for i := range people {
		people[i] = requests.MailApprovalRecipient{Email: fmt.Sprintf("source-%d@example.invalid", i), FullName: fmt.Sprintf("Source Recipient %d", i)}
	}
	person, _ := formSourcePerson(owner)
	return requests.CreateFormMailSource{OwnerSub: uuid.MustParse(person.sub), FormID: uuid.New(), FormTitle: "Source form", Recipients: people, ResponseCount: n}
}

func prepareSource(t *testing.T, w *approvalWorld, owner string, n int) handlers.FormMailSource {
	t.Helper()
	var source handlers.FormMailSource
	status, _ := w.call("yusuf", http.MethodPost, "/v1/form_mail_sources", sourceRequest(owner, n), &source)
	if status != 201 {
		t.Fatalf("prepare status=%d", status)
	}
	return source
}

func sourceSend(w *approvalWorld, id uuid.UUID) map[string]any {
	return map[string]any{"form_source_id": id, "template_id": w.freeBasic.ID, "body_variables": map[string]any{"Subject": "Source mail", "BodyHtml": "<p>Source content</p>"}}
}

func sourceApproval(t *testing.T, w *approvalWorld, owner string, id uuid.UUID) approvalAnswer {
	t.Helper()
	var out approvalAnswer
	status, _ := w.call(owner, http.MethodPost, "/v1/mail_approvals", sourceSend(w, id), &out)
	if status != 201 {
		t.Fatalf("source approval status=%d", status)
	}
	return out
}

func TestFormSourceIntegrationOwnerBodyAndIndependentLimit(t *testing.T) {
	w := newFormSourceWorld(t)
	source := prepareSource(t, w, "okur", 101)
	if source.RecipientCount != 101 || source.FormID == source.ID || source.OwnerSub != okur.sub {
		t.Fatal("descriptor is not the prepared owner/form snapshot")
	}
	path := "/v1/form_mail_sources/" + source.ID.String()
	if status, raw := w.call("okur", http.MethodGet, path, nil, nil); status != 200 || strings.Contains(string(raw), "@") {
		t.Fatalf("owner descriptor status=%d", status)
	}
	if status, _ := w.call("yusuf", http.MethodGet, path, nil, nil); status != 404 {
		t.Fatalf("other owner read=%d", status)
	}
	if status, _ := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/form-source", sourceSend(w, source.ID), nil); status != 404 {
		t.Fatalf("other owner consume=%d", status)
	}
	if status, _ := w.call("okur", http.MethodPost, "/v1/mail_tasks/form-source", sourceSend(w, source.ID), nil); status != 403 {
		t.Fatalf("no direct-send permission=%d", status)
	}
	get := httptest.NewRequest(http.MethodGet, path, nil)
	get.Header.Set("X-Test-User", "okur")
	reply, err := w.app.Test(get)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("source descriptor can be cached")
	}
	reply.Body.Close()

	if status, _ := w.call("yusuf", http.MethodPost, "/v1/form_mail_sources", sourceRequest("okur", 151), nil); status != 400 {
		t.Fatalf("source limit status=%d", status)
	}
	spoof := sourceSend(w, source.ID)
	spoof["recipients"] = []any{}
	if status, _ := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/form-source", spoof, nil); status != 400 {
		t.Fatalf("recipient override=%d", status)
	}
	large := sourceRequest("okur", 1)
	large.Recipients[0].FullName = strings.Repeat("n", 4<<20)
	raw, _ := json.Marshal(large)
	req := httptest.NewRequest(http.MethodPost, "/v1/form_mail_sources", strings.NewReader(string(raw)))
	req.Header.Set("X-Test-User", "yusuf")
	req.Header.Set("Content-Type", "application/json")
	response, err := w.app.Test(req)
	if err != nil {
		if err.Error() != "body size exceeds the given limit" {
			t.Fatal("unexpected body-limit transport error")
		}
	} else {
		defer response.Body.Close()
		if response.StatusCode != 413 {
			t.Fatalf("request body bound=%d", response.StatusCode)
		}
	}
}

func TestFormSourceIntegrationDirectSnapshotAndExactRetryAfterTTL(t *testing.T) {
	w := newFormSourceWorld(t)
	source := prepareSource(t, w, "source-sender", 101)
	request := sourceSend(w, source.ID)
	var first handlers.CreatedMailTask
	if status, _ := w.call("source-sender", http.MethodPost, "/v1/mail_tasks/form-source", request, &first); status != 201 {
		t.Fatalf("direct status=%d", status)
	}
	if len(w.queuedRows(first.ID)) != 101 {
		t.Fatal("source was not one task with 101 recipients")
	}
	var bulk bool
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT mail_task_bulk($1)`, first.ID).Scan(&bulk); err != nil || !bulk {
		t.Fatal("direct source did not use bulk content policy")
	}
	w.advance(time.Hour)
	var retry handlers.CreatedMailTask
	if status, _ := w.call("source-sender", http.MethodPost, "/v1/mail_tasks/form-source", request, &retry); status != 200 || retry.ID != first.ID {
		t.Fatalf("exact retry status=%d", status)
	}
	request["tracking"] = true
	if status, _ := w.call("source-sender", http.MethodPost, "/v1/mail_tasks/form-source", request, nil); status != 409 {
		t.Fatalf("different consume=%d", status)
	}
	var detail struct {
		Audience struct {
			Kind         string
			FormID       *uuid.UUID `json:"form_id"`
			FormSourceID *uuid.UUID `json:"form_source_id"`
		}
	}
	if status, _ := w.call("baska", http.MethodGet, "/v1/mail_tasks/"+first.ID.String(), nil, &detail); status != 200 || detail.Audience.Kind != "form" || detail.Audience.FormID == nil || *detail.Audience.FormID != source.FormID {
		t.Fatalf("source readback status=%d", status)
	}
}

func TestFormSourceIntegrationApprovalPinsSnapshotAndResubmitKeepsSource(t *testing.T) {
	w := newFormSourceWorld(t)
	source := prepareSource(t, w, "okur", 101)
	approval := sourceApproval(t, w, "okur", source.ID)
	var manualRows int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_approval_recipients WHERE approval_id=$1`, approval.ID).Scan(&manualRows); err != nil || manualRows != 0 {
		t.Fatal("source changed the manual recipient table")
	}
	w.advance(time.Hour)
	if status, _, _ := w.act("fatih", approval.ID, "reject", map[string]any{"reason": "Revise content"}); status != 200 {
		t.Fatalf("reject=%d", status)
	}
	request := sourceSend(w, source.ID)
	request["body_variables"] = map[string]any{"Subject": "Revised source mail", "BodyHtml": "<p>Revised</p>"}
	if status, _ := w.call("okur", http.MethodPost, "/v1/mail_approvals/"+approval.ID.String()+"/resubmit", request, nil); status != 200 {
		t.Fatalf("same source after preparation TTL=%d", status)
	}
	newSource := prepareSource(t, w, "okur", 102)
	request["form_source_id"] = newSource.ID
	if status, _ := w.call("okur", http.MethodPost, "/v1/mail_approvals/"+approval.ID.String()+"/resubmit", request, nil); status != 409 {
		t.Fatalf("silent refresh=%d", status)
	}
	status, approved, _ := w.act("fatih", approval.ID, "approve", nil)
	if status != 200 || len(approved.TaskIDs) != 1 {
		t.Fatalf("approve status=%d tasks=%d", status, len(approved.TaskIDs))
	}
	if len(w.queuedRows(approved.TaskIDs[0])) != 101 {
		t.Fatal("approval used a refreshed audience")
	}
}

func TestFormSourceIntegrationRollbackAndConcurrentConsume(t *testing.T) {
	w := newFormSourceWorld(t)
	source := prepareSource(t, w, "yusuf", 101)
	request := sourceSend(w, source.ID)
	if _, err := w.store.Conn.Exec(context.Background(), `CREATE FUNCTION fail_source_queue() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected source queue failure'; END $$; CREATE TRIGGER fail_source_queue BEFORE INSERT ON mail_queue FOR EACH ROW EXECUTE FUNCTION fail_source_queue()`); err != nil {
		t.Fatal(err)
	}
	if status, _ := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/form-source", request, nil); status != 500 {
		t.Fatalf("injected failure=%d", status)
	}
	var consumed *time.Time
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT consumed_at FROM mail_form_sources WHERE id=$1`, source.ID).Scan(&consumed); err != nil || consumed != nil {
		t.Fatal("rollback spent source")
	}
	if _, err := w.store.Conn.Exec(context.Background(), `DROP TRIGGER fail_source_queue ON mail_queue; DROP FUNCTION fail_source_queue()`); err != nil {
		t.Fatal(err)
	}
	w.mail.gate = make(chan struct{})
	w.mail.entered = make(chan struct{}, 1)
	done := make(chan int, 1)
	go func() {
		status, _ := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/form-source", request, nil)
		done <- status
	}()
	select {
	case <-w.mail.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("direct queue not entered")
	}
	status, _ := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/form-source", request, nil)
	close(w.mail.gate)
	if status != 409 {
		t.Fatalf("competing consume=%d", status)
	}
	if status := <-done; status != 201 {
		t.Fatalf("winning consume=%d", status)
	}
	if len(w.mail.of(w.freeBasic.ID)) != 1 {
		t.Fatal("concurrent source produced duplicate sends")
	}
}

func TestFormSourceIntegrationErasureInvalidationAndSendRace(t *testing.T) {
	w := newFormSourceWorld(t)
	payload := sourceRequest("okur", 101)
	payload.FormTitle = "source-0@example.invalid"
	var source handlers.FormMailSource
	if status, _ := w.call("yusuf", http.MethodPost, "/v1/form_mail_sources", payload, &source); status != 201 {
		t.Fatalf("prepare erasure source=%d", status)
	}
	approval := sourceApproval(t, w, "okur", source.ID)
	ctx := context.Background()
	erasure := database.AccountErasure{RequestID: uuid.New(), Subject: uuid.NewString(), Emails: []string{"source-0@example.invalid"}}
	if _, err := w.store.EraseAccount(ctx, erasure); !errors.Is(err, database.ErrAccountErasureInProgress) {
		t.Fatal("personal notification did not retain the existing in-flight erasure policy")
	}
	var invalidated *time.Time
	var owner string
	var kept int
	if err := w.store.Conn.QueryRow(ctx, `SELECT invalidated_at,owner_sub,(SELECT count(*) FROM mail_form_source_recipients r WHERE r.source_id=s.id) FROM mail_form_sources s WHERE s.id=$1`, source.ID).Scan(&invalidated, &owner, &kept); err != nil || invalidated == nil || owner != okur.sub || kept != 101 {
		t.Fatal("in-progress erasure did not commit invalidation while preserving names until delivery")
	}
	if status, _, _ := w.act("fatih", approval.ID, "approve", nil); status != 409 {
		t.Fatalf("in-progress invalidated source=%d", status)
	}
	// Mark only this fixture's approval-notification tasks delivered. No
	// source-recipient task is created, sent or deleted by this transition.
	var notificationIDs []uuid.UUID
	if err := w.store.Conn.QueryRow(ctx, `SELECT array_agg(id) FROM mail_tasks WHERE template_id=$1`, w.requested.ID).Scan(&notificationIDs); err != nil || len(notificationIDs) != 2 {
		t.Fatal("fixture notification targets were not exact")
	}
	if _, err := w.store.Conn.Exec(ctx, `UPDATE mail_queue SET status='sent' WHERE task_id=ANY($1::uuid[])`, notificationIDs); err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.EraseAccount(ctx, erasure); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := w.act("fatih", approval.ID, "approve", nil); status != 409 {
		t.Fatalf("invalidated approval=%d", status)
	}
	var title string
	if err := w.store.Conn.QueryRow(ctx, `SELECT form_title FROM mail_form_sources WHERE id=$1`, source.ID).Scan(&title); err != nil || title != database.ErasedNote {
		t.Fatal("personal source title survived erasure")
	}
	var personalRows int
	if err := w.store.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_form_source_recipients WHERE email='source-0@example.invalid'`).Scan(&personalRows); err != nil || personalRows != 0 {
		t.Fatal("source recipient survived erasure")
	}
	second := prepareSource(t, w, "yusuf", 2)
	w.mail.gate = make(chan struct{})
	w.mail.entered = make(chan struct{}, 1)
	sent := make(chan int, 1)
	go func() {
		status, _ := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/form-source", sourceSend(w, second.ID), nil)
		sent <- status
	}()
	select {
	case <-w.mail.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("send did not enter queue")
	}
	erased := make(chan error, 1)
	gateOpened := false
	defer func() {
		if !gateOpened {
			close(w.mail.gate)
		}
	}()
	// A dedicated, named connection lets this fixture observe this erasure's
	// real source-row lock wait, instead of guessing from goroutine scheduling.
	eraseConfig := w.store.Conn.Config()
	eraseConfig.MaxConns = 1
	eraseName := "forms-source-erasure-" + uuid.NewString()
	eraseConfig.ConnConfig.RuntimeParams["application_name"] = eraseName
	erasePool, err := pgxpool.NewWithConfig(ctx, eraseConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(erasePool.Close)
	var erasePID int32
	if err := erasePool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&erasePID); err != nil {
		t.Fatal(err)
	}
	eraseCtx, cancelErase := context.WithTimeout(ctx, 10*time.Second)
	defer cancelErase()
	eraseStore := database.NewStore(erasePool)
	go func() {
		_, err := eraseStore.EraseAccount(eraseCtx, database.AccountErasure{RequestID: uuid.New(), Subject: uuid.NewString(), Emails: []string{"source-0@example.invalid"}})
		erased <- err
	}()
	waitForFormSourceErasureLock(t, w, erasePID, eraseName, erased)
	close(w.mail.gate)
	gateOpened = true
	if status := <-sent; status != 201 {
		t.Fatalf("race send=%d", status)
	}
	select {
	case err := <-erased:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("erasure lock order stalled")
	}
	if err := w.store.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_queue WHERE recipient_email='source-0@example.invalid' AND status IN ('pending','processing')`).Scan(&personalRows); err != nil || personalRows != 0 {
		t.Fatal("erased recipient was requeued")
	}
	// The reverse order also fails closed: a completed erasure invalidates a
	// prepared source, so a later sender cannot create any task or queue rows.
	third := prepareSource(t, w, "yusuf", 2)
	if _, err := w.store.EraseAccount(ctx, database.AccountErasure{RequestID: uuid.New(), Subject: uuid.NewString(), Emails: []string{"source-0@example.invalid"}}); err != nil {
		t.Fatal(err)
	}
	if status, _ := w.call("yusuf", http.MethodPost, "/v1/mail_tasks/form-source", sourceSend(w, third.ID), nil); status != 409 {
		t.Fatalf("erasure-first source send=%d", status)
	}
	var futureTasks int
	if err := w.store.Conn.QueryRow(ctx, `SELECT count(*) FROM mail_tasks WHERE form_source_id=$1`, third.ID).Scan(&futureTasks); err != nil || futureTasks != 0 {
		t.Fatal("invalidated source created a future task")
	}
}

func waitForFormSourceErasureLock(t *testing.T, w *approvalWorld, pid int32, name string, finished <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		err := w.store.Conn.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM pg_stat_activity waiting
 JOIN pg_stat_activity sending ON sending.pid=ANY(pg_blocking_pids(waiting.pid))
 WHERE waiting.pid=$1 AND waiting.application_name=$2
  AND waiting.datname=current_database() AND waiting.wait_event_type='Lock'
  AND waiting.query LIKE 'SELECT fs.id FROM mail_form_sources fs%'
  AND sending.datname=current_database() AND sending.state='idle in transaction'
  AND EXISTS (SELECT 1 FROM pg_locks locks WHERE locks.pid=sending.pid
    AND locks.relation='mail_form_sources'::regclass AND locks.granted AND locks.mode='RowShareLock')
)`, pid, name).Scan(&blocked)
		if err != nil {
			t.Fatal("could not observe the fixture's source-row lock wait")
		}
		if blocked {
			t.Log("erasure source-row lock wait acknowledged before releasing the sender")
			return
		}
		select {
		case <-finished:
			t.Fatal("erasure finished before its source-row lock wait was acknowledged")
		case <-ctx.Done():
			t.Fatal("erasure did not reach the sender's source-row lock")
		case <-tick.C:
		}
	}
}

func TestFormSourceIntegrationLegacyManualConstraintAndExpiry(t *testing.T) {
	w := newFormSourceWorld(t)
	source := prepareSource(t, w, "okur", 101)
	down, err := migrationfiles.Files.ReadFile("20261017120000_add_form_mail_sources.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := w.store.Conn.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, downErr := tx.Exec(context.Background(), string(down))
	tx.Rollback(context.Background())
	if downErr == nil {
		t.Fatal("down migration silently removed a source snapshot")
	}

	w.advance(time.Hour)
	if _, err := w.handler.ExpireDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, _ := w.call("okur", http.MethodGet, "/v1/form_mail_sources/"+source.ID.String(), nil, nil); status != 404 {
		t.Fatalf("expired unconsumed source=%d", status)
	}
	manual := map[string]any{"template_id": w.freeBasic.ID, "recipients": []map[string]any{{"email": "manual@example.invalid"}}, "body_variables": map[string]any{"Subject": "Manual", "BodyHtml": "<p>Manual</p>"}}
	var approval approvalAnswer
	if status, raw := w.call("okur", http.MethodPost, "/v1/mail_approvals", manual, &approval); status != 201 || strings.Contains(string(raw), "form_source_id") {
		t.Fatalf("legacy approval=%d", status)
	}
	_, err = w.store.Conn.Exec(context.Background(), `INSERT INTO mail_approval_recipients(approval_id,position,email,full_name) VALUES($1,101,'outside@example.invalid','')`, approval.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatal("manual database position cap no longer holds")
	}
	request := sourceSend(w, uuid.New())
	data, _ := json.Marshal(request)
	if strings.Contains(string(data), "recipients") {
		t.Fatal("source request carries browser addresses")
	}
}
