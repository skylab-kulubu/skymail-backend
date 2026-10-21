package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/unsubscribe"
)

// unsubscribePanel is suppressionApp (three bounces) with Selin out of the
// duyuru stream through her link, and Mehmet, already bounced, out of it
// too.
func unsubscribePanel(t *testing.T, roles ...string) (*fiber.App, *database.Store, string) {
	t.Helper()
	app, store := suppressionApp(t, roles...)
	var selin string
	if err := store.Conn.QueryRow(context.Background(), `
		WITH rows AS (
			INSERT INTO mail_unsubscribes (address, stream, source, created_at) VALUES
			    ('selin@uzak.example.net', 'duyuru', 'link', now() + interval '1 minute'),
			    ('mehmet@uzak.example.net', 'duyuru', 'link', now() - interval '1 day')
			RETURNING id, address)
		SELECT id::text FROM rows WHERE address = 'selin@uzak.example.net'`).Scan(&selin); err != nil {
		t.Fatal(err)
	}
	return app, store, selin
}

func callJSON(t *testing.T, app *fiber.App, method, target, body string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(raw)
}

// The list holds both records, each with its reason and stream; reason
// keeps one of them.
func TestTheListShowsBouncesAndUnsubscribesTogether(t *testing.T) {
	app, _, selin := unsubscribePanel(t, "skymail:access", handlers.SuppressionsManageRole)
	response, body := call(t, app, fiber.MethodGet, "/v1/suppressions?_start=0&_end=100")
	if response.StatusCode != fiber.StatusOK || response.Header.Get("X-Total-Count") != "5" {
		t.Fatalf("GET = %d, total %q: %s", response.StatusCode, response.Header.Get("X-Total-Count"), body)
	}
	var rows []handlers.Suppression
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	if rows[0].ID.String() != selin || rows[0].Reason != "unsubscribed" || rows[0].Stream != "duyuru" || rows[0].Source != "link" ||
		rows[0].ReasonCode != "" || rows[0].Count != 1 {
		t.Errorf("newest row = %+v, want Selin's unsubscribe", rows[0])
	}
	reasons := map[string]int{}
	for _, row := range rows {
		reasons[row.Reason+" "+row.Stream]++
	}
	if reasons["bounce all"] != 3 || reasons["unsubscribed duyuru"] != 2 {
		t.Errorf("rows by reason = %v", reasons)
	}

	response, body = call(t, app, fiber.MethodGet, "/v1/suppressions?reason=unsubscribed&q=MEHMET")
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Total-Count") != "1" || len(rows) != 1 || rows[0].Reason != "unsubscribed" {
		t.Errorf("reason=unsubscribed&q=MEHMET = %s (total %s)", body, response.Header.Get("X-Total-Count"))
	}
	response, _ = call(t, app, fiber.MethodGet, "/v1/suppressions?reason=bounce")
	if response.Header.Get("X-Total-Count") != "3" {
		t.Errorf("reason=bounce total %s, want 3", response.Header.Get("X-Total-Count"))
	}
	if response, _ := call(t, app, fiber.MethodGet, "/v1/suppressions?reason=other"); response.StatusCode != fiber.StatusBadRequest {
		t.Errorf("reason=other = %d, want 400", response.StatusCode)
	}

	reactivator, _, _ := unsubscribePanel(t, "skymail:access", handlers.UnsubscribesReactivateRole)
	if response, _ := call(t, reactivator, fiber.MethodGet, "/v1/suppressions"); response.StatusCode != fiber.StatusOK {
		t.Errorf("GET with the reactivate role = %d, want 200", response.StatusCode)
	}
}

// DELETE removes a bounce only: an unsubscribe is 409 and stays.
func TestAnUnsubscribeIsNotDeleted(t *testing.T) {
	app, store, selin := unsubscribePanel(t, "skymail:access", handlers.SuppressionsManageRole)
	status, body := callJSON(t, app, fiber.MethodDelete, "/v1/suppressions/"+selin, "")
	if status != fiber.StatusConflict || !strings.Contains(body, "suppression.use_reactivate") {
		t.Errorf("DELETE an unsubscribe = %d %s, want 409 suppression.use_reactivate", status, body)
	}
	if n := countRows(t, store, `SELECT count(*) FROM mail_unsubscribes`); n != 2 {
		t.Errorf("unsubscribes = %d, want both", n)
	}
	if n := countRows(t, store, `SELECT count(*) FROM mail_suppression_removals`); n != 0 {
		t.Errorf("removals = %d, want none", n)
	}
}

func countRows(t *testing.T, store *database.Store, sql string) int {
	t.Helper()
	var n int
	if err := store.Conn.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Reversing an unsubscribe needs skymail:unsubscribes:reactivate and a
// reason without an address; it is recorded with who and why, never the
// address, and the reversals are listed with the same role.
func TestReversingAnUnsubscribeNeedsItsRoleAndAReason(t *testing.T) {
	manager, _, selin := unsubscribePanel(t, "skymail:access", handlers.SuppressionsManageRole)
	const reason = `{"reason": "Kişi bilgi kutusuna yazıp yeniden abone olmak istedi."}`
	if status, _ := callJSON(t, manager, fiber.MethodPost, "/v1/suppressions/"+selin+"/reactivate", reason); status != fiber.StatusForbidden {
		t.Errorf("reactivate with the manage role only = %d, want 403", status)
	}
	if status, _ := callJSON(t, manager, fiber.MethodGet, "/v1/suppressions/reactivations", ""); status != fiber.StatusForbidden {
		t.Errorf("reactivations with the manage role only = %d, want 403", status)
	}

	app, store, selin := unsubscribePanel(t, "skymail:access", handlers.UnsubscribesReactivateRole)
	path := "/v1/suppressions/" + selin + "/reactivate"
	for name, c := range map[string]struct {
		body, code string
	}{
		"no body":             {"", "suppression.reason_required"},
		"no reason":           {`{}`, "suppression.reason_required"},
		"a short reason":      {`{"reason": "istedi"}`, "suppression.reason_required"},
		"spaces only":         {`{"reason": "              "}`, "suppression.reason_required"},
		"a line break":        {`{"reason": "Kişi istedi,\nyeniden aç."}`, "suppression.reason_required"},
		"a long reason":       {`{"reason": "` + strings.Repeat("a", 501) + `"}`, "suppression.reason_required"},
		"the address":         {`{"reason": "selin@uzak.example.net yeniden istedi"}`, "suppression.reason_has_address"},
		"another address":     {`{"reason": "Kişi x@y.z adresinden yazdı."}`, "suppression.reason_has_address"},
		"an @ in the address": {`{"reason": "Kişi SELIN@uzak adresinden yazdı."}`, "suppression.reason_has_address"},
	} {
		status, body := callJSON(t, app, fiber.MethodPost, path, c.body)
		if status != fiber.StatusBadRequest || !strings.Contains(body, c.code) {
			t.Errorf("%s = %d %s, want 400 %s", name, status, body, c.code)
		}
	}
	var bounce string
	if err := store.Conn.QueryRow(context.Background(), `SELECT id::text FROM mail_suppressions WHERE address = 'ayse@uzak.example.net'`).Scan(&bounce); err != nil {
		t.Fatal(err)
	}
	if status, body := callJSON(t, app, fiber.MethodPost, "/v1/suppressions/"+bounce+"/reactivate", reason); status != fiber.StatusConflict || !strings.Contains(body, "suppression.use_delete") {
		t.Errorf("reactivate a bounce = %d %s, want 409 suppression.use_delete", status, body)
	}
	if status, _ := callJSON(t, app, fiber.MethodPost, "/v1/suppressions/70000000-0000-4000-8000-000000000001/reactivate", reason); status != fiber.StatusNotFound {
		t.Errorf("reactivate an unknown id = %d, want 404", status)
	}
	if n := countRows(t, store, `SELECT count(*) FROM mail_unsubscribe_reactivations`); n != 0 {
		t.Fatalf("refused reversals recorded %d rows", n)
	}

	if status, body := callJSON(t, app, fiber.MethodPost, path, reason); status != fiber.StatusNoContent {
		t.Fatalf("reactivate = %d %s", status, body)
	}
	if n := countRows(t, store, `SELECT count(*) FROM mail_unsubscribes WHERE address = 'selin@uzak.example.net'`); n != 0 {
		t.Errorf("Selin's unsubscribe left %d rows", n)
	}
	var record string
	if err := store.Conn.QueryRow(context.Background(), `SELECT row_to_json(r)::text FROM mail_unsubscribe_reactivations r`).Scan(&record); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"actor":"operator"`, suppressionOperator, "yeniden abone olmak istedi", `"stream":"duyuru"`, `"source":"link"`} {
		if !strings.Contains(record, want) {
			t.Errorf("reversal record %s lacks %s", record, want)
		}
	}
	if strings.Contains(strings.ToLower(record), "selin") {
		t.Errorf("reversal record %s names the address", record)
	}
	if status, _ := callJSON(t, app, fiber.MethodPost, path, reason); status != fiber.StatusNotFound {
		t.Errorf("a second reactivate = %d, want 404", status)
	}

	response, body := call(t, app, fiber.MethodGet, "/v1/suppressions/reactivations")
	var reversals []handlers.Reactivation
	if err := json.Unmarshal([]byte(body), &reversals); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK || response.Header.Get("X-Total-Count") != "1" || len(reversals) != 1 ||
		reversals[0].Actor != "operator" || reversals[0].ActorSub == nil || *reversals[0].ActorSub != suppressionOperator {
		t.Errorf("reactivations = %d %s", response.StatusCode, body)
	}
}

// Over the database: GET writes nothing; one-click writes one record however
// often it comes; the page's geri removes it and records the person's own
// reversal, without the address.
func TestTheUnsubscribeLinkOverTheDatabase(t *testing.T) {
	_, store := suppressionApp(t)
	ring, err := unsubscribe.NewRing(1, map[byte][]byte{1: bytes.Repeat([]byte{1}, unsubscribe.KeySize)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := unsubscribe.Config{On: true, BaseURL: "https://api.yildizskylab.com/api/skymail", AccountURL: unsubscribe.DefaultAccountURL, Ring: ring}
	app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
	handlers.NewUnsubscribePublicHandler(store, cfg).Register(app)
	link, ok := cfg.Link("Selin@Uzak.Example.net")
	if !ok {
		t.Fatal("no link")
	}
	path := strings.TrimPrefix(link, cfg.BaseURL)
	post := func(values url.Values) int {
		t.Helper()
		request := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader(values.Encode()))
		request.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationForm)
		response, err := app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode
	}

	if response, _ := call(t, app, fiber.MethodGet, path); response.StatusCode != fiber.StatusOK {
		t.Fatalf("GET = %d", response.StatusCode)
	}
	if n := countRows(t, store, `SELECT count(*) FROM mail_unsubscribes`); n != 0 {
		t.Errorf("GET wrote %d rows", n)
	}
	for i := 0; i < 2; i++ {
		if status := post(url.Values{"List-Unsubscribe": {"One-Click"}}); status != fiber.StatusOK {
			t.Fatalf("one-click %d = %d", i+1, status)
		}
	}
	if n := countRows(t, store, `SELECT count(*) FROM mail_unsubscribes WHERE address = 'selin@uzak.example.net' AND stream = 'duyuru' AND source = 'link'`); n != 1 {
		t.Errorf("records = %d, want one", n)
	}
	for i := 0; i < 2; i++ {
		if status := post(url.Values{"eylem": {"geri"}}); status != fiber.StatusOK {
			t.Fatalf("geri %d = %d", i+1, status)
		}
	}
	if n := countRows(t, store, `SELECT count(*) FROM mail_unsubscribes`); n != 0 {
		t.Errorf("after geri: %d records", n)
	}
	var record string
	if err := store.Conn.QueryRow(context.Background(), `SELECT row_to_json(r)::text FROM mail_unsubscribe_reactivations r`).Scan(&record); err != nil {
		t.Fatalf("one reversal: %v", err)
	}
	if !strings.Contains(record, `"actor":"recipient"`) || !strings.Contains(record, `"actor_sub":null`) || strings.Contains(strings.ToLower(record), "selin") {
		t.Errorf("reversal record = %s", record)
	}
}
