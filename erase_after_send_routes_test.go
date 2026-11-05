package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// POST /v1/mail_tasks/single with erase_after_send (core's account deletion
// notices, 20261031230000): a service's mail that keeps no record of its
// recipient. It is never tracked, even while SkyMail tracks system mail, and
// it goes in the system lane; a person's send cannot ask for it.
func TestASendThatKeepsNoRecordIsAServicesAndNeverTracked(t *testing.T) {
	w, _ := systemTrackedWorld(t)
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skymail", claimsUserinfo(t), middlewares.AudienceOff)
	registerMailTaskRoutes(protectedAPI(app, auth, nil), auth, handlers.NewMailHandler(w.store, w.mail, w.directory))
	post := func(service bool) (int, []byte) {
		t.Helper()
		send := w.singleSendTo(elif.email, "Elif", false)
		send["erase_after_send"] = true
		body, _ := json.Marshal(send)
		request := httptest.NewRequest(http.MethodPost, "/v1/mail_tasks/single", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+bearer(t, uuid.NewString(), service, "skymail:access", "skymail:mails:send"))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, raw
	}

	asks := w.directory.accountAsks
	status, raw := post(true)
	var created struct {
		ID             uuid.UUID `json:"id"`
		Tracking       *bool     `json:"tracking"`
		EraseAfterSend *bool     `json:"erase_after_send"`
	}
	if status != fiber.StatusCreated || json.Unmarshal(raw, &created) != nil || created.Tracking == nil || created.EraseAfterSend == nil {
		t.Fatalf("service: POST /v1/mail_tasks/single = %d %s", status, raw)
	}
	if *created.Tracking || !*created.EraseAfterSend {
		t.Errorf("answer tracking %v, erase_after_send %v; want untracked and kept no record", *created.Tracking, *created.EraseAfterSend)
	}
	requested, row := w.onlyRow(created.ID)
	if requested || row.token != nil || row.sub != "" || w.laneOf(created.ID) != mailer.PrioritySystem {
		t.Errorf("tracking_requested %v, row %+v, lane %d; want untracked in the system lane", requested, row, w.laneOf(created.ID))
	}
	if n := w.linkCount(created.ID); n != 0 {
		t.Errorf("%d tracked links registered", n)
	}
	if asked := w.directory.accountAsks - asks; asked != 0 {
		t.Errorf("Keycloak asked %d times about the recipient, want none", asked)
	}
	var flagged bool
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT erase_after_send FROM mail_tasks WHERE id = $1`, created.ID).Scan(&flagged); err != nil {
		t.Fatal(err)
	}
	if !flagged {
		t.Error("the send is not marked erase_after_send")
	}

	var sends int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_tasks`).Scan(&sends); err != nil {
		t.Fatal(err)
	}
	status, raw = post(false)
	var refusal struct {
		Code string `json:"code"`
	}
	if status != fiber.StatusBadRequest || json.Unmarshal(raw, &refusal) != nil || refusal.Code != "mail_task.erase_after_send_not_allowed" {
		t.Errorf("person: POST /v1/mail_tasks/single = %d %s, want 400 mail_task.erase_after_send_not_allowed", status, raw)
	}
	var after int
	if err := w.store.Conn.QueryRow(context.Background(), `SELECT count(*) FROM mail_tasks`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != sends {
		t.Errorf("a person's refused send queued %d sends", after-sends)
	}
}
