package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skynotify-backend/internal/database"
	"github.com/skylab-kulubu/skynotify-backend/internal/handlers"
	"github.com/skylab-kulubu/skynotify-backend/internal/middlewares"
	"github.com/skylab-kulubu/skynotify-backend/internal/requests"
	"github.com/skylab-kulubu/skynotify-backend/pkg/validator"
)

// A service's Idempotency-Key on POST /v1/mail_tasks/single (hub tickets
// forms-posta-core 11, horizontal-scale 28): the same key and the same mail
// queue one send, and the retry gets the first answer again; the same key
// with another mail is 422; without the header nothing changes.

type singlePost struct {
	status   int
	replayed string
	raw      []byte
	created  struct {
		ID             uuid.UUID `json:"id"`
		Tracking       *bool     `json:"tracking"`
		EraseAfterSend bool      `json:"erase_after_send"`
	}
	error struct {
		Code string `json:"code"`
	}
}

func postSingle(t *testing.T, app *fiber.App, sub string, service bool, key string, send map[string]any) singlePost {
	t.Helper()
	body, _ := json.Marshal(send)
	request := httptest.NewRequest(http.MethodPost, "/v1/mail_tasks/single", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+bearer(t, sub, service, "skynotify:access", "skynotify:mails:send"))
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set(handlers.IdempotencyKeyHeader, key)
	}
	response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var p singlePost
	p.status = response.StatusCode
	p.replayed = response.Header.Get("Idempotent-Replayed")
	p.raw, _ = io.ReadAll(response.Body)
	if p.status == fiber.StatusCreated {
		if err := json.Unmarshal(p.raw, &p.created); err != nil {
			t.Fatalf("answer %s: %v", p.raw, err)
		}
	} else {
		_ = json.Unmarshal(p.raw, &p.error)
	}
	return p
}

func (w *approvalWorld) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.store.Conn.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestASingleMailWithAServicesIdempotencyKeyIsQueuedOnce(t *testing.T) {
	app, w := singleSendWorld(t)
	core := uuid.NewString()
	sends := func() int { return w.count(`SELECT count(*) FROM mail_tasks`) }
	rows := func() int { return w.count(`SELECT count(*) FROM mail_queue`) }

	t.Run("the same key and mail: one send, the first answer again", func(t *testing.T) {
		before, beforeRows := sends(), rows()
		send := w.singleSendTo(elif.email, "Elif", false)
		first := postSingle(t, app, core, true, "core:forms.guest-mail:1:v1", send)
		if first.status != fiber.StatusCreated || first.replayed != "" {
			t.Fatalf("first = %d %s, replayed %q", first.status, first.raw, first.replayed)
		}
		// The same mail again.
		again := w.singleSendTo(elif.email, "Elif", false)
		retry := postSingle(t, app, core, true, "core:forms.guest-mail:1:v1", again)
		if retry.status != fiber.StatusCreated || retry.replayed != "true" {
			t.Fatalf("retry = %d %s, replayed %q", retry.status, retry.raw, retry.replayed)
		}
		if string(retry.raw) != string(first.raw) {
			t.Errorf("retry answered %s, want the first answer %s", retry.raw, first.raw)
		}
		if got := sends() - before; got != 1 {
			t.Errorf("%d sends queued, want 1", got)
		}
		if got := rows() - beforeRows; got != 1 {
			t.Errorf("%d queue rows, want 1", got)
		}
		if n := w.count(`SELECT count(*) FROM mail_task_idempotency_keys WHERE task_id = $1 AND sent_by = $2`, first.created.ID, core); n != 1 {
			t.Errorf("%d keys name the send, want 1", n)
		}
	})

	t.Run("the same key with another mail: 422, nothing queued", func(t *testing.T) {
		send := w.singleSendTo(elif.email, "Elif", false)
		if p := postSingle(t, app, core, true, "core:forms.guest-mail:2:v1", send); p.status != fiber.StatusCreated {
			t.Fatalf("first = %d %s", p.status, p.raw)
		}
		before := sends()
		other := w.singleSendTo(baska.email, "Başka", false)
		p := postSingle(t, app, core, true, "core:forms.guest-mail:2:v1", other)
		if p.status != fiber.StatusUnprocessableEntity || p.error.Code != "mail_task.idempotency_key_reused" {
			t.Errorf("another mail = %d %s, want 422 mail_task.idempotency_key_reused", p.status, p.raw)
		}
		if got := sends() - before; got != 0 {
			t.Errorf("%d sends queued", got)
		}
	})

	t.Run("a key is the service's own", func(t *testing.T) {
		before := sends()
		send := w.singleSendTo(elif.email, "Elif", false)
		a := postSingle(t, app, core, true, "shared-key", send)
		b := postSingle(t, app, uuid.NewString(), true, "shared-key", send)
		if a.status != fiber.StatusCreated || b.status != fiber.StatusCreated || b.replayed != "" || a.created.ID == b.created.ID {
			t.Errorf("two services' same key = %d %s, %d %s (replayed %q); want two sends", a.status, a.raw, b.status, b.raw, b.replayed)
		}
		if got := sends() - before; got != 2 {
			t.Errorf("%d sends queued, want 2", got)
		}
	})

	t.Run("without the header: as before, every request a send", func(t *testing.T) {
		before := sends()
		send := w.singleSendTo(elif.email, "Elif", false)
		for range 2 {
			if p := postSingle(t, app, core, true, "", send); p.status != fiber.StatusCreated || p.replayed != "" {
				t.Fatalf("no key = %d %s, replayed %q", p.status, p.raw, p.replayed)
			}
		}
		if got := sends() - before; got != 2 {
			t.Errorf("%d sends queued, want 2", got)
		}
	})

	t.Run("refused keys queue nothing", func(t *testing.T) {
		before := sends()
		send := w.singleSendTo(elif.email, "Elif", false)
		for _, c := range []struct {
			name    string
			service bool
			key     string
			code    string
		}{
			{"a person's token", false, "panel-1", "mail_task.idempotency_key_not_allowed"},
			{"a space", true, "two words", "mail_task.idempotency_key_invalid"},
			{"too long", true, strings.Repeat("k", 256), "mail_task.idempotency_key_invalid"},
			{"not ASCII", true, "anahtar-ğ", "mail_task.idempotency_key_invalid"},
		} {
			p := postSingle(t, app, uuid.NewString(), c.service, c.key, send)
			if p.status != fiber.StatusBadRequest || p.error.Code != c.code {
				t.Errorf("%s = %d %s, want 400 %s", c.name, p.status, p.raw, c.code)
			}
		}
		if got := sends() - before; got != 0 {
			t.Errorf("%d sends queued", got)
		}
		if n := w.count(`SELECT count(*) FROM mail_task_idempotency_keys WHERE idempotency_key = 'panel-1'`); n != 0 {
			t.Errorf("a person's key was kept")
		}
	})

	t.Run("a refused mail keeps no key: the retry is tried again", func(t *testing.T) {
		send := w.singleSendTo(elif.email, "Elif", false)
		delete(send, "template_id")
		send["template_key"] = "no.such-template"
		if p := postSingle(t, app, core, true, "core:refused:1", send); p.status == fiber.StatusCreated {
			t.Fatalf("an unknown template = %d %s", p.status, p.raw)
		}
		if n := w.count(`SELECT count(*) FROM mail_task_idempotency_keys WHERE idempotency_key = 'core:refused:1'`); n != 0 {
			t.Errorf("a refused mail kept its key")
		}
	})

	t.Run("past 72 hours the key is swept and the mail can go again", func(t *testing.T) {
		send := w.singleSendTo(elif.email, "Elif", false)
		first := postSingle(t, app, core, true, "core:old:1", send)
		if first.status != fiber.StatusCreated {
			t.Fatalf("first = %d %s", first.status, first.raw)
		}
		if _, err := w.store.Conn.Exec(context.Background(), `
			UPDATE mail_task_idempotency_keys SET created_at = now() - make_interval(hours => $1 + 1)
			WHERE idempotency_key = 'core:old:1'`, database.IdempotencyKeyRetentionHours); err != nil {
			t.Fatal(err)
		}
		if _, err := w.store.Conn.Exec(context.Background(), `
			UPDATE mail_task_idempotency_keys SET created_at = now() - make_interval(hours => $1 - 1)
			WHERE idempotency_key = 'core:forms.guest-mail:1:v1'`, database.IdempotencyKeyRetentionHours); err != nil {
			t.Fatal(err)
		}
		deleted, err := w.store.DeleteMailTaskIdempotencyKeys(context.Background(), 1)
		if err != nil || deleted != 1 {
			t.Fatalf("swept %d keys (%v), want the one past 72 hours", deleted, err)
		}
		again := postSingle(t, app, core, true, "core:old:1", send)
		if again.status != fiber.StatusCreated || again.replayed != "" || again.created.ID == first.created.ID {
			t.Errorf("after the sweep = %d %s (replayed %q), want a new send", again.status, again.raw, again.replayed)
		}
		if n := w.count(`SELECT count(*) FROM mail_task_idempotency_keys WHERE idempotency_key = 'core:forms.guest-mail:1:v1'`); n != 1 {
			t.Errorf("a key younger than 72 hours was swept")
		}
	})
}

// With system tracking on the answer says whether the recipient is tracked,
// and the retry gets that answer; erase_after_send is replayed as it was.
// Retries that arrive together queue one send between them.
func TestATrackedOrErasedSingleMailIsReplayedAsAnswered(t *testing.T) {
	w, _ := systemTrackedWorld(t)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skynotify", claimsUserinfo(t), middlewares.AudienceOff)
	registerMailTaskRoutes(protectedAPI(app, auth, nil), auth, handlers.NewMailHandler(w.store, w.mail, w.directory))
	core := uuid.NewString()

	send := w.singleSendTo(elif.email, "Elif", false)
	first := postSingle(t, app, core, true, "core:tracked:1", send)
	if first.status != fiber.StatusCreated || first.created.Tracking == nil || !*first.created.Tracking {
		t.Fatalf("first = %d %s, want tracked", first.status, first.raw)
	}
	retry := postSingle(t, app, core, true, "core:tracked:1", send)
	if retry.replayed != "true" || string(retry.raw) != string(first.raw) {
		t.Errorf("retry = %d %s (replayed %q), want %s", retry.status, retry.raw, retry.replayed, first.raw)
	}

	erased := w.singleSendTo(elif.email, "Elif", false)
	erased["erase_after_send"] = true
	first = postSingle(t, app, core, true, "core:deleted:1", erased)
	retry = postSingle(t, app, core, true, "core:deleted:1", erased)
	if first.status != fiber.StatusCreated || !first.created.EraseAfterSend || *first.created.Tracking ||
		retry.replayed != "true" || string(retry.raw) != string(first.raw) {
		t.Errorf("erase_after_send = %s then %s (replayed %q)", first.raw, retry.raw, retry.replayed)
	}

	before := w.count(`SELECT count(*) FROM mail_tasks`)
	const callers = 4
	answers := make([]singlePost, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			answers[i] = postSingle(t, app, core, true, "core:together:1", send)
		}()
	}
	close(start)
	wg.Wait()
	replayed := 0
	for i, a := range answers {
		if a.status != fiber.StatusCreated || a.created.ID != answers[0].created.ID {
			t.Errorf("caller %d = %d %s, want the one send %s", i, a.status, a.raw, answers[0].created.ID)
		}
		if a.replayed == "true" {
			replayed++
		}
	}
	if replayed != callers-1 {
		t.Errorf("%d answers replayed, want %d", replayed, callers-1)
	}
	if got := w.count(`SELECT count(*) FROM mail_tasks`) - before; got != 1 {
		t.Errorf("%d sends queued by %d callers, want 1", got, callers)
	}
}

// requestDigest is the digest SendSingle keeps of send: its JSON as the
// request type reads it.
func requestDigest(t *testing.T, send map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(send)
	var params requests.SendSingleMail
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(params)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// A request that finds its key taken by a transaction not yet committed
// waits for it in the key's INSERT, past the replay that looked first: the
// test holds that transaction open itself, sees the request blocked on its
// lock, then ends it. Committed with the same request: the request's own send
// is rolled back and it answers the held one. Committed with another request:
// 422. Rolled back: the request queues its own send.
func TestASingleMailWaitsForTheTransactionHoldingItsKey(t *testing.T) {
	app, w := singleSendWorld(t)
	ctx := context.Background()
	core := uuid.NewString()
	send := w.singleSendTo(elif.email, "Elif", false)
	digest := requestDigest(t, send)

	for _, c := range []struct {
		name     string
		digest   string
		commit   bool
		status   int
		replayed string
		code     string
		sends    int
	}{
		// sends: the held one when committed, the request's own when not.
		{"committed, the same mail", digest, true, fiber.StatusCreated, "true", "", 1},
		{"committed, another mail", strings.Repeat("0", 64), true, fiber.StatusUnprocessableEntity, "", "mail_task.idempotency_key_reused", 1},
		{"rolled back", digest, false, fiber.StatusCreated, "", "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			key := "core:held:" + uuid.NewString()
			before := w.count(`SELECT count(*) FROM mail_tasks`)
			held, err := w.store.Conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Rollback(ctx) }()
			var heldTask uuid.UUID
			if err := held.QueryRow(ctx, `INSERT INTO mail_tasks (sent_by, body_variables) VALUES ($1, '{}') RETURNING id`, core).Scan(&heldTask); err != nil {
				t.Fatal(err)
			}
			heldAnswer := `{"id":"` + heldTask.String() + `","tracking":false,"purpose":null,"category":null}`
			if _, err := held.Exec(ctx, `
				INSERT INTO mail_task_idempotency_keys (sent_by, idempotency_key, request_sha256, task_id, result)
				VALUES ($1, $2, $3, $4, $5)`, core, key, c.digest, heldTask, heldAnswer); err != nil {
				t.Fatal(err)
			}

			answered := make(chan singlePost, 1)
			go func() { answered <- postSingle(t, app, core, true, key, send) }()

			// The request is past its replay and waits on the held row's lock.
			deadline := time.Now().Add(20 * time.Second)
			for w.count(`SELECT count(*) FROM pg_locks WHERE NOT granted`) == 0 {
				select {
				case p := <-answered:
					t.Fatalf("the request answered %d %s without waiting for the held key", p.status, p.raw)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("the request never waited on the held key")
				}
				time.Sleep(20 * time.Millisecond)
			}

			if c.commit {
				err = held.Commit(ctx)
			} else {
				err = held.Rollback(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			p := <-answered
			if p.status != c.status || p.replayed != c.replayed || p.error.Code != c.code {
				t.Fatalf("answer = %d %s (replayed %q), want %d %q %q", p.status, p.raw, p.replayed, c.status, c.replayed, c.code)
			}
			if c.commit && c.status == fiber.StatusCreated && p.created.ID != heldTask {
				t.Errorf("answered send %s, want the held one %s", p.created.ID, heldTask)
			}
			if !c.commit && p.created.ID == heldTask {
				t.Errorf("answered the rolled-back send")
			}
			if got := w.count(`SELECT count(*) FROM mail_tasks`) - before; got != c.sends {
				t.Errorf("%d sends exist after it, want %d", got, c.sends)
			}
			if n := w.count(`SELECT count(*) FROM mail_queue WHERE task_id = $1`, heldTask); n != 0 {
				t.Errorf("the held send has %d queue rows", n)
			}
		})
	}
}
