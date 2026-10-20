package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/accessgate"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
)

// The in-app inbox (hub ticket iletisim-servisi 05) through the chain
// production registers: POST /v1/notifications writes the items, a person
// reads and marks their own through /v1/me/inbox with their own token.

// inboxTypes are a type that goes by e-mail and to the inbox (optional) and
// one that goes only to the inbox (required).
const inboxTypes = `
INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, content_sensitive)
VALUES ('event.reminder', 'Hatırlatma', '{{.EventName}} yarın', '<p>{{.EventName}}</p>', '{{.EventName}}', '', false);
INSERT INTO notification_types (key, category, required, default_channels, lane)
VALUES ('event.reminder', 'events', false, '{email,inbox}', 'bulk'),
       ('team.joined', NULL, true, '{inbox}', 'system');
INSERT INTO notification_variants (type, channel, title, summary, link)
VALUES ('event.reminder', 'inbox', '{{.EventName}} yarın', 'Saat {{.Time}}', '/events/{{.EventID}}'),
       ('team.joined', 'inbox', 'Ekibe katıldın: {{.Team}}', '', '');`

func newInboxWorld(t *testing.T) *notificationWorld {
	t.Helper()
	w := newNotificationWorld(t)
	if _, err := w.store.Conn.Exec(context.Background(), inboxTypes); err != nil {
		t.Fatal(err)
	}
	return w
}

// personToken is a person's token as Keycloak issues it to Account center or
// sky-app: no client_id, aud as given (skymail when the client's audience
// mapper adds it), no role on SkyMail.
func personToken(t *testing.T, sub uuid.UUID, aud ...string) string {
	t.Helper()
	claims := jwt.MapClaims{"sub": sub.String(), "azp": "account-center", "aud": aud}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("fixture-only"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func reminder(key string, recipients ...map[string]any) map[string]any {
	return map[string]any{
		"type":       "event.reminder",
		"recipients": recipients,
		"variables":  map[string]any{"EventName": "Kodlama Gecesi", "Time": "19.00", "EventID": key},
	}
}

func (w *notificationWorld) send(key string, body map[string]any) handlers.NotificationResult {
	w.t.Helper()
	s := w.post(bearer(w.t, coreService, true, coreRoles...), key, body)
	if s.status != fiber.StatusCreated {
		w.t.Fatalf("POST /notifications %s: %d %s", key, s.status, s.raw)
	}
	return s.result
}

func (w *notificationWorld) call(token, method, path string) (int, []byte) {
	w.t.Helper()
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := w.app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		w.t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, raw
}

func (w *notificationWorld) inbox(person uuid.UUID, query string) handlers.InboxPage {
	w.t.Helper()
	status, raw := w.call(personToken(w.t, person, "skymail"), http.MethodGet, "/v1/me/inbox"+query)
	var page handlers.InboxPage
	if status != fiber.StatusOK || json.Unmarshal(raw, &page) != nil {
		w.t.Fatalf("GET /v1/me/inbox%s: %d %s", query, status, raw)
	}
	return page
}

func (w *notificationWorld) counts(person uuid.UUID) handlers.InboxCounts {
	w.t.Helper()
	status, raw := w.call(personToken(w.t, person, "skymail"), http.MethodGet, "/v1/me/inbox/counts")
	var counts handlers.InboxCounts
	if status != fiber.StatusOK || json.Unmarshal(raw, &counts) != nil {
		w.t.Fatalf("counts: %d %s", status, raw)
	}
	return counts
}

func (w *notificationWorld) mark(person uuid.UUID, item uuid.UUID, mark string) (int, handlers.InboxItem) {
	w.t.Helper()
	status, raw := w.call(personToken(w.t, person, "skymail"), http.MethodPost, "/v1/me/inbox/"+item.String()+"/"+mark)
	var got handlers.InboxItem
	if status == fiber.StatusOK {
		if err := json.Unmarshal(raw, &got); err != nil {
			w.t.Fatal(err)
		}
	}
	return status, got
}

// The inbox does not look at preferences (S4): a person whose preferences
// close e-mail still gets the item; e-mail goes only where allowed.
func TestTheInboxIgnoresPreferencesAndEmailDoesNot(t *testing.T) {
	w := newInboxWorld(t)
	result := w.send("reminder-1", reminder("evt-9",
		recipient(personAyse, "ayse@example.com", []string{}...),
		recipient(personMehmet, "", "email")))

	email, inbox := result.Channels["email"], result.Channels["inbox"]
	if email.Queued != 1 || email.Skipped["not_allowed"] != 1 {
		t.Fatalf("email %+v", email)
	}
	if inbox.Queued != 2 || inbox.Failed != 0 || len(inbox.Skipped) != 0 || inbox.TaskID != nil {
		t.Fatalf("inbox %+v", inbox)
	}
	if rows := w.rows(*email.TaskID); len(rows) != 1 || rows["mehmet@example.com"].sub == nil {
		t.Fatalf("e-mail rows %+v", rows)
	}
	for _, person := range []uuid.UUID{personAyse, personMehmet} {
		page := w.inbox(person, "")
		if len(page.Items) != 1 || page.NextCursor != nil {
			t.Fatalf("%s inbox %+v", person, page)
		}
		item := page.Items[0]
		if item.Type != "event.reminder" || item.Title != "Kodlama Gecesi yarın" || item.Summary != "Saat 19.00" ||
			item.Link == nil || *item.Link != "/events/evt-9" || item.SeenAt != nil || item.ReadAt != nil {
			t.Errorf("item %+v", item)
		}
	}
	// Kept 90 days.
	if n := w.count(`SELECT count(*) FROM inbox_items WHERE notification_id = $1 AND expires_at = created_at + interval '90 days'`, result.ID); n != 2 {
		t.Errorf("items kept 90 days = %d, want 2", n)
	}

	// A required type that goes only to the inbox, with no link.
	result = w.send("joined-1", map[string]any{"type": "team.joined", "recipients": []map[string]any{recipient(personZeynep, "")},
		"variables": map[string]any{"Team": "SKY SEC"}})
	if _, ok := result.Channels["email"]; ok || result.Channels["inbox"].Queued != 1 {
		t.Fatalf("inbox only: %+v", result)
	}
	if page := w.inbox(personZeynep, ""); len(page.Items) != 1 || page.Items[0].Title != "Ekibe katıldın: SKY SEC" || page.Items[0].Link != nil {
		t.Fatalf("zeynep %+v", page)
	}
	if n := w.people.asked; n != 1 {
		t.Errorf("Keycloak asked %d times, want 1 (Mehmet's address only)", n)
	}
}

// A person sees and marks only their own items; anyone else's is 404, the
// same as one that does not exist.
func TestAPersonSeesAndMarksOnlyTheirOwnItems(t *testing.T) {
	w := newInboxWorld(t)
	w.send("reminder-1", reminder("evt-1", recipient(personAyse, "ayse@example.com", "email"), recipient(personMehmet, "mehmet@example.com", "email")))
	ayse, mehmet := w.inbox(personAyse, ""), w.inbox(personMehmet, "")
	if len(ayse.Items) != 1 || len(mehmet.Items) != 1 || ayse.Items[0].ID == mehmet.Items[0].ID {
		t.Fatalf("ayse %+v mehmet %+v", ayse, mehmet)
	}
	theirs := mehmet.Items[0].ID
	for _, mark := range []string{"seen", "read", "unread", "archive"} {
		if status, _ := w.mark(personAyse, theirs, mark); status != fiber.StatusNotFound {
			t.Errorf("%s on another's item: %d, want 404", mark, status)
		}
		if status, _ := w.mark(personAyse, uuid.New(), mark); status != fiber.StatusNotFound {
			t.Errorf("%s on no item: %d, want 404", mark, status)
		}
	}
	if status, _ := w.call(personToken(t, personAyse, "skymail"), http.MethodPost, "/v1/me/inbox/not-a-uuid/read"); status != fiber.StatusNotFound {
		t.Errorf("read of a malformed id: %d, want 404", status)
	}
	if after := w.inbox(personMehmet, ""); !reflect.DeepEqual(after, mehmet) {
		t.Errorf("Mehmet's item changed: %+v", after)
	}
}

// counts follows the seen, read, unread and archive marks; read is seen too;
// read-all and seen-all change only the caller's items.
func TestCountsFollowTheMarksAndBulkMarksTouchOnlyTheCaller(t *testing.T) {
	w := newInboxWorld(t)
	for _, key := range []string{"r-1", "r-2", "r-3"} {
		w.send(key, reminder(key, recipient(personAyse, "ayse@example.com", "email"), recipient(personMehmet, "mehmet@example.com", "email")))
	}
	items := w.inbox(personAyse, "").Items
	if len(items) != 3 {
		t.Fatalf("items %+v", items)
	}
	check := func(step string, unseen, unread int64) {
		t.Helper()
		if got := w.counts(personAyse); got != (handlers.InboxCounts{Unseen: unseen, Unread: unread}) {
			t.Errorf("%s: counts %+v, want unseen %d unread %d", step, got, unseen, unread)
		}
	}
	check("start", 3, 3)
	if status, item := w.mark(personAyse, items[0].ID, "seen"); status != fiber.StatusOK || item.SeenAt == nil || item.ReadAt != nil {
		t.Fatalf("seen: %d %+v", status, item)
	}
	check("seen", 2, 3)
	status, read := w.mark(personAyse, items[1].ID, "read")
	if status != fiber.StatusOK || read.SeenAt == nil || read.ReadAt == nil {
		t.Fatalf("read: %d %+v", status, read)
	}
	check("read", 1, 2)
	// Marking again keeps the first time.
	if _, again := w.mark(personAyse, items[1].ID, "read"); !again.ReadAt.Equal(*read.ReadAt) {
		t.Errorf("read again moved read_at: %v -> %v", read.ReadAt, again.ReadAt)
	}
	if status, item := w.mark(personAyse, items[1].ID, "unread"); status != fiber.StatusOK || item.ReadAt != nil || item.SeenAt == nil {
		t.Fatalf("unread: %d %+v", status, item)
	}
	check("unread", 1, 3)
	if status, item := w.mark(personAyse, items[2].ID, "archive"); status != fiber.StatusOK || item.ArchivedAt == nil {
		t.Fatalf("archive: %d %+v", status, item)
	}
	check("archive", 0, 2)
	if got := len(w.inbox(personAyse, "").Items); got != 2 {
		t.Errorf("default list after archive: %d items, want 2", got)
	}
	if only := w.inbox(personAyse, "?archived=only").Items; len(only) != 1 || only[0].ID != items[2].ID {
		t.Errorf("archived=only: %+v", only)
	}
	if got := len(w.inbox(personAyse, "?archived=include").Items); got != 3 {
		t.Errorf("archived=include: %d items, want 3", got)
	}

	mehmetBefore := w.inbox(personMehmet, "?archived=include")
	token := personToken(t, personAyse, "skymail")
	if status, raw := w.call(token, http.MethodPost, "/v1/me/inbox/read-all"); status != fiber.StatusOK || string(raw) != `{"updated":2}` {
		t.Fatalf("read-all: %d %s", status, raw)
	}
	check("read-all", 0, 0)
	if status, raw := w.call(token, http.MethodPost, "/v1/me/inbox/seen-all"); status != fiber.StatusOK || string(raw) != `{"updated":0}` {
		t.Fatalf("seen-all: %d %s", status, raw)
	}
	if after := w.inbox(personMehmet, "?archived=include"); !reflect.DeepEqual(after, mehmetBefore) {
		t.Errorf("read-all changed Mehmet's items: %+v", after)
	}
	if got := w.counts(personMehmet); got != (handlers.InboxCounts{Unseen: 3, Unread: 3}) {
		t.Errorf("Mehmet's counts %+v", got)
	}
	// The archived item kept its read mark: read-all leaves archived ones out.
	if n := w.count(`SELECT count(*) FROM inbox_items WHERE id = $1 AND read_at IS NULL`, items[2].ID); n != 1 {
		t.Errorf("read-all marked an archived item")
	}

	// seen-all on Mehmet's inbox.
	if status, raw := w.call(personToken(t, personMehmet, "skymail"), http.MethodPost, "/v1/me/inbox/seen-all"); status != fiber.StatusOK || string(raw) != `{"updated":3}` {
		t.Fatalf("seen-all: %d %s", status, raw)
	}
	if got := w.counts(personMehmet); got != (handlers.InboxCounts{Unseen: 0, Unread: 3}) {
		t.Errorf("Mehmet's counts after seen-all %+v", got)
	}
}

// Pages come newest first; the cursor goes on from the last item, items
// written in the same instant in id order, and no item is served twice or
// left out.
func TestTheInboxPagesNewestFirstWithAStableCursor(t *testing.T) {
	w := newInboxWorld(t)
	// Six items for Ayşe and one for Mehmet in between, each from its own
	// notification; four of Ayşe's share an instant.
	if _, err := w.store.Conn.Exec(context.Background(), `
		WITH n AS (INSERT INTO notifications (type, sent_by, idempotency_key, request_sha256)
		           SELECT 'team.joined', 'core', g::text, 'x' FROM generate_series(1, 7) AS g
		           RETURNING id, idempotency_key::int AS g)
		INSERT INTO inbox_items (user_id, notification_id, notification_type, title, created_at, expires_at)
		SELECT CASE WHEN g = 6 THEN $2::uuid ELSE $1::uuid END, id, 'team.joined', 'öğe ' || g, at, at + interval '90 days'
		FROM (SELECT id, g, CASE WHEN g <= 4 THEN now() - interval '1 hour' ELSE now() - g * interval '1 minute' END AS at FROM n) s`,
		personAyse, personMehmet); err != nil {
		t.Fatal(err)
	}
	rows, err := w.store.Conn.Query(context.Background(),
		`SELECT id FROM inbox_items WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, personAyse)
	if err != nil {
		t.Fatal(err)
	}
	var want []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		_ = rows.Scan(&id)
		want = append(want, id)
	}
	rows.Close()
	if len(want) != 6 {
		t.Fatalf("fixture: %d items", len(want))
	}

	var got []uuid.UUID
	query := "?limit=4"
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatal("too many pages")
		}
		page := w.inbox(personAyse, query)
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		if page.NextCursor == nil {
			break
		}
		query = "?limit=4&cursor=" + *page.NextCursor
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged %v\nwant  %v", got, want)
	}
	if page := w.inbox(personAyse, "?limit=6"); len(page.Items) != 6 || page.NextCursor != nil {
		t.Errorf("one full page: %d items, cursor %v", len(page.Items), page.NextCursor)
	}

	token := personToken(t, personAyse, "skymail")
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=x", "?archived=all", "?cursor=bm90LWEtY3Vyc29y", "?cursor=%%%"} {
		if status, raw := w.call(token, http.MethodGet, "/v1/me/inbox"+query); status != fiber.StatusBadRequest {
			t.Errorf("GET %s: %d %s, want 400", query, status, raw)
		}
	}
}

// Only a person's own token issued for SkyMail opens the inbox; it needs no
// role, so /v1's skymail:access chain must not run for it.
func TestOnlyAPersonsOwnTokenForSkyMailOpensTheInbox(t *testing.T) {
	w := newInboxWorld(t)
	for _, c := range []struct {
		name   string
		token  string
		status int
		code   string
	}{
		{"a person's token for SkyMail, no role", personToken(t, personAyse, "skymail"), fiber.StatusOK, ""},
		{"a person's token for SkyMail and others", personToken(t, personAyse, "account", "skymail"), fiber.StatusOK, ""},
		{"a person's token for another service", personToken(t, personAyse, "account"), fiber.StatusUnauthorized, "server.unauthorized"},
		{"a person's token with no aud", personToken(t, personAyse), fiber.StatusUnauthorized, "server.unauthorized"},
		{"a service's token", serviceToken(t, "skymail"), fiber.StatusForbidden, "auth.person_required"},
		{"no token", "", fiber.StatusUnauthorized, "server.unauthorized"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, path := range []string{"/v1/me/inbox", "/v1/me/inbox/counts"} {
				status, raw := w.call(c.token, http.MethodGet, path)
				var body struct {
					Code string `json:"code"`
				}
				_ = json.Unmarshal(raw, &body)
				if status != c.status || (c.code != "" && body.Code != c.code) {
					t.Errorf("GET %s: %d %s, want %d %s", path, status, raw, c.status, c.code)
				}
			}
		})
	}
}

func serviceToken(t *testing.T, aud ...string) string {
	t.Helper()
	claims := jwt.MapClaims{"sub": coreService, "azp": "core", "client_id": "core", "aud": aud,
		"test_roles": []string{"skymail:access", handlers.NotificationsSendRole}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("fixture-only"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// recordingInbox is an InboxHandler that only says it ran.
type recordingInbox struct{ order *[]string }

func (r recordingInbox) ran(c fiber.Ctx) error {
	*r.order = append(*r.order, "handler")
	return c.SendStatus(fiber.StatusNoContent)
}
func (r recordingInbox) List(c fiber.Ctx) error    { return r.ran(c) }
func (r recordingInbox) Counts(c fiber.Ctx) error  { return r.ran(c) }
func (r recordingInbox) Seen(c fiber.Ctx) error    { return r.ran(c) }
func (r recordingInbox) Read(c fiber.Ctx) error    { return r.ran(c) }
func (r recordingInbox) Unread(c fiber.Ctx) error  { return r.ran(c) }
func (r recordingInbox) Archive(c fiber.Ctx) error { return r.ran(c) }
func (r recordingInbox) ReadAll(c fiber.Ctx) error { return r.ran(c) }
func (r recordingInbox) SeenAll(c fiber.Ctx) error { return r.ran(c) }

// Every inbox route runs the person check, then the account access gate,
// then its handler, and never /v1's Authenticate or permission check, though
// protectedAPI is registered after it as in main.
func TestInboxRoutesRunThePersonCheckAndTheGateOnly(t *testing.T) {
	t.Parallel()
	routes := []struct{ method, path string }{
		{fiber.MethodGet, "/v1/me/inbox"}, {fiber.MethodGet, "/v1/me/inbox/counts"},
		{fiber.MethodPost, "/v1/me/inbox/read-all"}, {fiber.MethodPost, "/v1/me/inbox/seen-all"},
		{fiber.MethodPost, "/v1/me/inbox/" + uuid.NewString() + "/seen"}, {fiber.MethodPost, "/v1/me/inbox/" + uuid.NewString() + "/read"},
		{fiber.MethodPost, "/v1/me/inbox/" + uuid.NewString() + "/unread"}, {fiber.MethodPost, "/v1/me/inbox/" + uuid.NewString() + "/archive"},
	}
	for _, decision := range []accessgate.Decision{accessgate.Allowed, accessgate.Blocked, accessgate.Unavailable} {
		for _, route := range routes {
			order := []string{}
			auth := &routeTestAuth{subject: personAyse.String(), order: &order}
			gate := &routeTestGate{decision: decision, order: &order}
			app := fiber.New(fiber.Config{ErrorHandler: errorHandler})
			registerInboxRoutes(app, auth, gate, recordingInbox{order: &order})
			api := protectedAPI(app, auth, gate)
			api.Get("/me/inbox", func(c fiber.Ctx) error { return c.SendStatus(fiber.StatusTeapot) })

			response, err := app.Test(httptest.NewRequest(route.method, route.path, nil))
			if err != nil {
				t.Fatal(err)
			}
			want, wantOrder := fiber.StatusNoContent, []string{"person", "gate", "handler"}
			switch decision {
			case accessgate.Blocked:
				want, wantOrder = fiber.StatusUnauthorized, []string{"person", "gate"}
			case accessgate.Unavailable:
				want, wantOrder = fiber.StatusServiceUnavailable, []string{"person", "gate"}
			}
			if response.StatusCode != want || !reflect.DeepEqual(order, wantOrder) || auth.permissionCalls != 0 {
				t.Errorf("%s %s %s: status %d order %v permission %d, want %d %v", decision, route.method, route.path,
					response.StatusCode, order, auth.permissionCalls, want, wantOrder)
			}
		}
	}
}

// An item past its expiry is not served, counted or marked; the sweep
// deletes it. expires_at makes an item go sooner than 90 days, never later.
func TestExpiredItemsAreNotServedAndTheSweepDeletesThem(t *testing.T) {
	w := newInboxWorld(t)
	ctx := context.Background()
	now := time.Now()
	soon := reminder("evt-soon", recipient(personAyse, "ayse@example.com", "email"))
	soon["expires_at"] = now.Add(time.Hour).UTC().Format(time.RFC3339)
	w.send("soon", soon)
	late := reminder("evt-late", recipient(personAyse, "ayse@example.com", "email"))
	late["expires_at"] = now.Add(200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	lateResult := w.send("late", late)
	w.send("plain", reminder("evt-plain", recipient(personAyse, "ayse@example.com", "email")))
	if n := w.count(`SELECT count(*) FROM inbox_items WHERE notification_id = $1 AND expires_at = created_at + interval '90 days'`, lateResult.ID); n != 1 {
		t.Errorf("expires_at past 90 days was not cut to 90 days")
	}
	past := reminder("evt-past", recipient(personAyse, "ayse@example.com", "email"))
	past["expires_at"] = now.Add(-time.Minute).UTC().Format(time.RFC3339)
	if s := w.post(bearer(t, coreService, true, coreRoles...), "past", past); s.status != fiber.StatusBadRequest || s.error.Code != "notification.expires_at_past" {
		t.Errorf("expires_at in the past: %d %s", s.status, s.raw)
	}

	// One item's time has come: as if written 91 days ago.
	plain := w.inbox(personAyse, "?archived=include").Items
	if len(plain) != 3 {
		t.Fatalf("items %+v", plain)
	}
	var old uuid.UUID
	for _, item := range plain {
		if *item.Link == "/events/evt-plain" {
			old = item.ID
		}
	}
	if _, err := w.store.Conn.Exec(ctx, `UPDATE inbox_items SET created_at = now() - interval '91 days', expires_at = now() - interval '1 day' WHERE id = $1`, old); err != nil {
		t.Fatal(err)
	}
	if got := len(w.inbox(personAyse, "?archived=include").Items); got != 2 {
		t.Errorf("an expired item is served: %d items", got)
	}
	if got := w.counts(personAyse); got.Unseen != 2 || got.Unread != 2 {
		t.Errorf("an expired item is counted: %+v", got)
	}
	if status, _ := w.mark(personAyse, old, "read"); status != fiber.StatusNotFound {
		t.Errorf("an expired item was marked: %d", status)
	}

	// The sweep, on a test clock: today the expired one; in two hours the
	// soon one too; in 91 days everything.
	for _, step := range []struct {
		at   time.Time
		want int64
	}{{now, 1}, {now.Add(2 * time.Hour), 1}, {now.Add(89 * 24 * time.Hour), 0}, {now.Add(91 * 24 * time.Hour), 1}} {
		deleted, err := w.store.DeleteExpiredInbox(ctx, step.at, 1)
		if err != nil || deleted != step.want {
			t.Errorf("sweep at %s: deleted %d %v, want %d", step.at.Sub(now).Round(time.Hour), deleted, err, step.want)
		}
	}
	if n := w.count(`SELECT count(*) FROM inbox_items`); n != 0 {
		t.Errorf("items left after 91 days: %d", n)
	}
}

// A text that does not render writes no item and counts every recipient
// failed on the inbox; the e-mail still goes.
func TestAnInboxTextThatDoesNotRenderWritesNoItem(t *testing.T) {
	w := newInboxWorld(t)
	for key, variables := range map[string]map[string]any{
		"missing": {"EventName": "Kodlama Gecesi", "Time": "19.00"},
		"spaces":  {"EventName": "Kodlama Gecesi", "Time": "19.00", "EventID": "a b"},
		"host":    {"EventName": "Kodlama Gecesi", "Time": "19.00", "EventID": "/../x\\y"},
		"long":    {"EventName": strings.Repeat("ş", 201), "Time": "19.00", "EventID": "e1"},
	} {
		body := reminder("", recipient(personAyse, "ayse@example.com", "email"), recipient(personMehmet, "mehmet@example.com", []string{}...))
		body["variables"] = variables
		result := w.send(key, body)
		if inbox := result.Channels["inbox"]; inbox.Queued != 0 || inbox.Failed != 2 {
			t.Errorf("%s: inbox %+v", key, inbox)
		}
		if email := result.Channels["email"]; email.Queued+email.Failed != 1 {
			t.Errorf("%s: email %+v", key, email)
		}
	}
	if n := w.count(`SELECT count(*) FROM inbox_items`); n != 0 {
		t.Errorf("items written: %d", n)
	}
}

// A retry with the Idempotency-Key writes no second item.
func TestTheSameIdempotencyKeyWritesTheInboxOnce(t *testing.T) {
	w := newInboxWorld(t)
	body := reminder("evt-1", recipient(personAyse, "ayse@example.com", "email"))
	first := w.send("once", body)
	s := w.post(bearer(t, coreService, true, coreRoles...), "once", body)
	if s.status != fiber.StatusCreated || s.replayed != "true" || !reflect.DeepEqual(s.result, first) {
		t.Fatalf("replay: %d %s", s.status, s.raw)
	}
	if n := w.count(`SELECT count(*) FROM inbox_items`); n != 1 {
		t.Errorf("items = %d, want 1", n)
	}
}

// The seed states a type's inbox text with the type: there exactly when the
// type goes to the inbox, parsing, its link an in-app path.
func TestTheSeedStatesATypesInboxText(t *testing.T) {
	w := newInboxWorld(t)
	seed := bearer(t, formsBot, false, "skymail:access", "skymail:templates:write", "skymail:templates:read")
	put := func(body map[string]any) (int, handlers.NotificationType, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(http.MethodPut, "/v1/notification_types/team.left", bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+seed)
		request.Header.Set("Content-Type", "application/json")
		response, err := w.app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(response.Body)
		var got handlers.NotificationType
		_ = json.Unmarshal(out, &got)
		return response.StatusCode, got, string(out)
	}
	inbox := map[string]any{"title": "{{.Team}} ekibinden ayrıldın", "summary": "", "link": "/teams/{{.TeamID}}"}
	status, got, raw := put(map[string]any{"required": true, "default_channels": []string{"email", "inbox"}, "lane": "system", "inbox": inbox})
	if status != fiber.StatusOK || got.Inbox == nil || got.Inbox.Title != "{{.Team}} ekibinden ayrıldın" || got.Inbox.Link != "/teams/{{.TeamID}}" {
		t.Fatalf("PUT: %d %s", status, raw)
	}
	status, raw2 := w.call(seed, http.MethodGet, "/v1/notification_types/team.left")
	if status != fiber.StatusOK || !strings.Contains(string(raw2), `"inbox":{"title":"{{.Team}} ekibinden ayrıldın"`) {
		t.Fatalf("GET: %d %s", status, raw2)
	}
	status, raw2 = w.call(seed, http.MethodGet, "/v1/notification_types")
	if status != fiber.StatusOK || !strings.Contains(string(raw2), `"key":"event.reminder"`) || !strings.Contains(string(raw2), `"link":"/events/{{.EventID}}"`) {
		t.Fatalf("GET list: %d %s", status, raw2)
	}

	for _, c := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"default_channels": []string{"inbox"}, "lane": "bulk"}, "notification_type.inbox_variant_required"},
		{map[string]any{"default_channels": []string{"email"}, "lane": "bulk", "inbox": inbox}, "notification_type.inbox_variant_unused"},
		{map[string]any{"default_channels": []string{"inbox"}, "lane": "bulk", "inbox": map[string]any{"title": "x", "link": "https://evil.example/{{.X}}"}}, "notification_type.inbox_variant_invalid"},
		{map[string]any{"default_channels": []string{"inbox"}, "lane": "bulk", "inbox": map[string]any{"title": "x", "link": "//evil.example"}}, "notification_type.inbox_variant_invalid"},
		{map[string]any{"default_channels": []string{"inbox"}, "lane": "bulk", "inbox": map[string]any{"title": "x", "link": "{{.Link}}"}}, "notification_type.inbox_variant_invalid"},
		{map[string]any{"default_channels": []string{"inbox"}, "lane": "bulk", "inbox": map[string]any{"title": "{{.Team"}}, "notification_type.inbox_variant_invalid"},
		{map[string]any{"default_channels": []string{"inbox"}, "lane": "bulk", "inbox": map[string]any{"title": ""}}, "validation.error"},
		{map[string]any{"default_channels": []string{"inbox", "inbox"}, "lane": "bulk", "inbox": inbox}, "validation.error"},
		{map[string]any{"default_channels": []string{"push"}, "lane": "bulk"}, "validation.error"},
	} {
		if status, _, raw := put(c.body); status != fiber.StatusBadRequest || !strings.Contains(raw, `"code":"`+c.code+`"`) {
			t.Errorf("PUT %v: %d %s, want 400 %s", c.body, status, raw, c.code)
		}
	}

	// An https address on the club's domain (Account center on the web) is
	// a link too; the variables fill its path, never its host.
	web := map[string]any{"title": "{{.Team}} ekibinden ayrıldın", "link": "https://my.yildizskylab.com/ekipler/{{.TeamID}}"}
	if status, got, raw := put(map[string]any{"required": true, "default_channels": []string{"inbox"}, "lane": "system", "inbox": web}); status != fiber.StatusOK || got.Inbox == nil || got.Inbox.Link != web["link"] {
		t.Fatalf("PUT with an absolute link: %d %s", status, raw)
	}
	for _, link := range []string{"https://evil.example/", "https://yildizskylab.com.evil.example/", "https://{{.Host}}/x", "http://my.yildizskylab.com/", "https://user@my.yildizskylab.com/"} {
		body := map[string]any{"required": true, "default_channels": []string{"inbox"}, "lane": "system", "inbox": map[string]any{"title": "x", "link": link}}
		if status, _, raw := put(body); status != fiber.StatusBadRequest || !strings.Contains(raw, `"code":"notification_type.inbox_variant_invalid"`) {
			t.Errorf("PUT link %q: %d %s, want 400", link, status, raw)
		}
	}
	result := w.send("left-1", map[string]any{"type": "team.left", "recipients": []map[string]any{recipient(personAyse, "")},
		"variables": map[string]any{"Team": "SKY SEC", "TeamID": "sky-sec"}})
	if result.Channels["inbox"].Queued != 1 {
		t.Fatalf("send: %+v", result)
	}
	if items := w.inbox(personAyse, "").Items; len(items) != 1 || items[0].Link == nil || *items[0].Link != "https://my.yildizskylab.com/ekipler/sky-sec" {
		t.Fatalf("item %+v", items)
	}
	// A variable that would carry the link off the host does not render.
	result = w.send("left-2", map[string]any{"type": "team.left", "recipients": []map[string]any{recipient(personAyse, "")},
		"variables": map[string]any{"Team": "SKY SEC", "TeamID": "x y"}})
	if inbox := result.Channels["inbox"]; inbox.Queued != 0 || inbox.Failed != 1 {
		t.Errorf("a link with a space: %+v", inbox)
	}

	// Without inbox the text goes with the channel.
	status, got, raw = put(map[string]any{"required": true, "default_channels": []string{"email"}, "lane": "system"})
	if status != fiber.StatusOK || got.Inbox != nil {
		t.Fatalf("PUT without inbox: %d %s", status, raw)
	}
	if n := w.count(`SELECT count(*) FROM notification_variants WHERE type = 'team.left'`); n != 0 {
		t.Errorf("inbox text left: %d", n)
	}
}
