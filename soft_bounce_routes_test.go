package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
)

// softBounce records one report about a new mail to address: kind, its codes,
// how many days ago, and whether it named the mail's own address. It returns
// the mail, for a second report about it.
func softBounce(t *testing.T, store *database.Store, address, kind, status string, smtp int, as string, daysAgo int, matched bool) string {
	t.Helper()
	ctx := context.Background()
	var row string
	if err := store.Conn.QueryRow(ctx, `
		INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, sent_at)
		VALUES ($1, 'Kişi', $2, 'K', 'G', 'sent', now() - make_interval(days => $3::int))
		RETURNING id`, suppressionTask, address, daysAgo+1).Scan(&row); err != nil {
		t.Fatal(err)
	}
	report(t, store, row, kind, status, smtp, as, daysAgo, matched)
	return row
}

func report(t *testing.T, store *database.Store, row, kind, status string, smtp int, as string, daysAgo int, matched bool) {
	t.Helper()
	action := "failed"
	if kind == "delay" {
		action = "delayed"
	}
	if _, err := store.Conn.Exec(context.Background(), `
		INSERT INTO mail_bounces (queue_id, source, kind, action, status, smtp_code, exo_as_code, recipient_matched, ndr_id_sha256, received_at)
		VALUES ($1, 'dsn', $2, $3, NULLIF($4, ''), NULLIF($5::int, 0)::smallint, NULLIF($6, ''), $7,
		        encode(sha256(gen_random_uuid()::text::bytea), 'hex'), now() - make_interval(days => $8::int))`,
		row, kind, action, status, smtp, as, matched, daysAgo); err != nil {
		t.Fatal(err)
	}
}

// GET /v1/suppressions/soft-bounces (hub ticket skymail-bounce-isleme 13):
// the addresses with 3 or more softly refused mails in the last 30 days, not
// suppressed, newest first, with the last one's codes and no text.
func TestTheAddressesWithRepeatedSoftBouncesAreListed(t *testing.T) {
	app, store := suppressionApp(t, handlers.SuppressionsManageRole)

	// Can: three mails refused softly, one of them reported twice; the
	// address written in two cases. The last one is a full mailbox.
	first := softBounce(t, store, "can@uzak.example.net", "soft", "5.7.1", 550, "", 20, true)
	report(t, store, first, "soft", "5.7.1", 550, "", 19, true)
	softBounce(t, store, "CAN@uzak.example.net", "soft", "5.7.1", 550, "", 10, true)
	softBounce(t, store, "can@uzak.example.net", "soft", "5.2.2", 552, "", 1, true)
	// Deniz: four, earlier; the last one's code with Exchange's token.
	for _, days := range []int{25, 15, 8} {
		softBounce(t, store, "deniz@uzak.example.net", "soft", "5.2.2", 552, "", days, true)
	}
	softBounce(t, store, "deniz@uzak.example.net", "soft", "5.4.1", 550, "12345", 5, true)
	// Not listed: two only; one of three past 30 days; one of three that
	// named another address; three delays; Ayşe, suppressed already; mail
	// Account erasure cleared.
	for _, days := range []int{3, 2} {
		softBounce(t, store, "ebru@uzak.example.net", "soft", "5.2.2", 552, "", days, true)
	}
	for _, days := range []int{31, 4, 2} {
		softBounce(t, store, "filiz@uzak.example.net", "soft", "5.2.2", 552, "", days, true)
	}
	for i, days := range []int{6, 4, 2} {
		softBounce(t, store, "gul@uzak.example.net", "soft", "5.2.2", 552, "", days, i > 0)
	}
	for _, days := range []int{6, 4, 2} {
		softBounce(t, store, "hakan@uzak.example.net", "delay", "4.4.7", 0, "", days, true)
		softBounce(t, store, "ayse@uzak.example.net", "soft", "5.2.2", 552, "", days, true)
		softBounce(t, store, "", "soft", "5.2.2", 552, "", days, true)
	}

	response, body := call(t, app, http.MethodGet, "/v1/suppressions/soft-bounces")
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Total-Count") != "2" {
		t.Fatalf("GET = %d, total %q: %s", response.StatusCode, response.Header.Get("X-Total-Count"), body)
	}
	var page []handlers.SoftBounceAddress
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 {
		t.Fatalf("listed %s, want Can and Deniz", body)
	}
	if page[0].Address != "can@uzak.example.net" || page[0].Count != 3 || page[0].LastCode != "5.2.2" {
		t.Errorf("first = %+v, want Can: 3 mails, last 5.2.2", page[0])
	}
	if page[1].Address != "deniz@uzak.example.net" || page[1].Count != 4 || page[1].LastCode != "5.4.1 AS(12345)" {
		t.Errorf("second = %+v, want Deniz: 4 mails, last 5.4.1 AS(12345)", page[1])
	}
	if !page[0].LastBouncedAt.After(page[1].LastBouncedAt) {
		t.Errorf("Can's last bounce %v is not after Deniz's %v", page[0].LastBouncedAt, page[1].LastBouncedAt)
	}
	var keys []map[string]any
	_ = json.Unmarshal([]byte(body), &keys)
	for key := range keys[0] {
		switch key {
		case "address", "count", "last_bounced_at", "last_code":
		default:
			t.Errorf("the answer carries %q", key)
		}
	}

	response, body = call(t, app, http.MethodGet, "/v1/suppressions/soft-bounces?q=DENIZ")
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Total-Count") != "1" || len(page) != 1 || page[0].Address != "deniz@uzak.example.net" {
		t.Errorf("search = %s (total %s)", body, response.Header.Get("X-Total-Count"))
	}
	response, body = call(t, app, http.MethodGet, "/v1/suppressions/soft-bounces?_start=1&_end=2")
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Total-Count") != "2" || len(page) != 1 || page[0].Address != "deniz@uzak.example.net" {
		t.Errorf("second page = %s (total %s)", body, response.Header.Get("X-Total-Count"))
	}
	if response, _ := call(t, app, http.MethodGet, "/v1/suppressions/soft-bounces?q="+strings.Repeat("a", 255)); response.StatusCode != http.StatusBadRequest {
		t.Errorf("a search past 254 characters = %d, want 400", response.StatusCode)
	}
}

// Only skymail:suppressions:manage reads it.
func TestTheSoftBounceListNeedsItsRole(t *testing.T) {
	app, _ := suppressionApp(t, "skymail:access", handlers.UnsubscribesReactivateRole, "skymail:mails:read")
	if response, _ := call(t, app, http.MethodGet, "/v1/suppressions/soft-bounces"); response.StatusCode != http.StatusForbidden {
		t.Errorf("without skymail:suppressions:manage: status = %d, want 403", response.StatusCode)
	}
}
