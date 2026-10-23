package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/handlers"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/middlewares"
	"github.com/skylab-kulubu/skymail-backend/internal/migrations"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
	"github.com/skylab-kulubu/skymail-backend/pkg/validator"
)

// POST /v1/notifications through the chain production registers: userinfo
// (a stub that answers the bearer's own claims), skymail:access, the role,
// the service account check, the handler, the real database and queue. No
// dispatcher is started.

const (
	coreService = "0c0c0c0c-0000-4000-8000-000000000001"
	formsBot    = "0c0c0c0c-0000-4000-8000-000000000002"
)

var (
	personAyse    = uuid.MustParse("1a000000-0000-4000-8000-000000000001")
	personMehmet  = uuid.MustParse("1a000000-0000-4000-8000-000000000002")
	personZeynep  = uuid.MustParse("1a000000-0000-4000-8000-000000000003")
	personGone    = uuid.MustParse("1a000000-0000-4000-8000-000000000004")
	personOff     = uuid.MustParse("1a000000-0000-4000-8000-000000000005")
	personNoMail  = uuid.MustParse("1a000000-0000-4000-8000-000000000006")
	personPending = uuid.MustParse("1a000000-0000-4000-8000-000000000007")
	personBroken  = uuid.MustParse("1a000000-0000-4000-8000-000000000008")
)

// people is Keycloak's users as GetUser reads them; asked counts the lookups.
type people struct {
	mu    sync.Mutex
	users map[string]*gocloak.User
	asked int
}

func (p *people) GetUser(_ context.Context, id string) (*gocloak.User, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked++
	if id == personBroken.String() {
		return nil, errors.New("keycloak did not answer")
	}
	return p.users[id], nil
}

func keycloakUser(email string, enabled, verified bool, first, last string) *gocloak.User {
	return &gocloak.User{Email: gocloak.StringP(email), Enabled: gocloak.BoolP(enabled), EmailVerified: gocloak.BoolP(verified),
		FirstName: gocloak.StringP(first), LastName: gocloak.StringP(last)}
}

type notificationWorld struct {
	t      *testing.T
	store  *database.Store
	app    *fiber.App
	people *people
}

// claimsUserinfo answers userinfo with the bearer's own sub and test_roles,
// as Keycloak would for a token it issued.
func claimsUserinfo(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(token, ".")
		if r.URL.Path != "/protocol/openid-connect/userinfo" || len(parts) != 3 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Sub   string   `json:"sub"`
			Roles []string `json:"test_roles"`
		}
		_ = json.Unmarshal(payload, &claims)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":             claims.Sub,
			"resource_access": map[string]any{"skymail": map[string]any{"roles": claims.Roles}},
		})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func newNotificationWorld(t *testing.T, opts ...mailer.Option) *notificationWorld {
	t.Helper()
	ctx := context.Background()
	postgres := testpostgres.StartDatabase(t)
	if _, err := migrations.Run(ctx, postgres.URL, 0); err != nil {
		t.Fatal(err)
	}
	store := database.NewStore(postgres.Pool)
	for _, statement := range []string{
		`INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		 VALUES ('event.cancelled', 'Etkinlik iptal', '{{.EventName}} iptal', '<p>Merhaba {{.FullName}}, {{.EventName}} iptal.</p>', 'Merhaba {{.FullName}}', '', false),
		        ('core.certificate', 'Sertifika', 'Sertifikan', '<p>{{.FullName}}</p>', '{{.FullName}}', '', false)`,
		`INSERT INTO notification_types (key, category, required, default_channels, lane)
		 VALUES ('event.cancelled', 'events', false, '{email}', 'bulk'),
		        ('core.certificate', NULL, true, '{email}', 'system'),
		        ('event.orphan', 'events', false, '{email}', 'bulk')`,
		`INSERT INTO notification_types (key, required, archived_at) VALUES ('event.old', true, now())`,
	} {
		if _, err := postgres.Pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	directory := &people{users: map[string]*gocloak.User{
		personAyse.String():    keycloakUser("ayse@example.com", true, true, "Ayşe", "Kaya"),
		personMehmet.String():  keycloakUser("mehmet@example.com", true, true, "Mehmet", "Demir"),
		personOff.String():     keycloakUser("off@example.com", false, true, "Kapalı", "Hesap"),
		personNoMail.String():  keycloakUser("", true, false, "Adres", "Yok"),
		personPending.String(): keycloakUser("pending@example.com", true, false, "Doğrulanmamış", "Adres"),
	}}

	app := fiber.New(fiber.Config{ErrorHandler: errorHandler, StructValidator: validator.NewStructValidator()})
	auth := middlewares.NewAuthMiddleware("skymail", claimsUserinfo(t), middlewares.AudienceOff)
	// As main registers them: the person's own routes before /v1's chain.
	registerInboxRoutes(app, auth, nil, handlers.NewInboxHandler(store))
	api := protectedAPI(app, auth, nil)
	registerNotificationRoutes(api, auth, handlers.NewNotificationHandler(store, mailer.NewMailer(store, mailer.SMTPConfig{}, mailer.SenderOn, opts...), directory))
	return &notificationWorld{t: t, store: store, app: app, people: directory}
}

// bearer is a token as Keycloak would issue it: a service account's carries
// client_id = azp, a person's none.
func bearer(t *testing.T, sub string, service bool, roles ...string) string {
	t.Helper()
	claims := jwt.MapClaims{"sub": sub, "azp": "account-center", "test_roles": roles}
	if service {
		claims["azp"], claims["client_id"] = "core", "core"
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("fixture-only"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

var coreRoles = []string{"skymail:access", handlers.NotificationsSendRole}

type sent struct {
	status   int
	replayed string
	raw      []byte
	result   handlers.NotificationResult
	error    struct {
		Code   string         `json:"code"`
		Params map[string]any `json:"params"`
	}
}

func (w *notificationWorld) post(token, key string, body any) sent {
	w.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		w.t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/notifications", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := w.app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		w.t.Fatal(err)
	}
	var s sent
	s.status, s.replayed = response.StatusCode, response.Header.Get("Idempotent-Replayed")
	s.raw, _ = io.ReadAll(response.Body)
	if s.status == fiber.StatusCreated {
		if err := json.Unmarshal(s.raw, &s.result); err != nil {
			w.t.Fatalf("decode %s: %v", s.raw, err)
		}
	} else {
		_ = json.Unmarshal(s.raw, &s.error)
	}
	return s
}

func (w *notificationWorld) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.store.Conn.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

type queued struct {
	email, name string
	sub         *uuid.UUID
	priority    int16
}

func (w *notificationWorld) rows(task uuid.UUID) map[string]queued {
	w.t.Helper()
	rows, err := w.store.Conn.Query(context.Background(),
		`SELECT recipient_email, recipient_full_name, recipient_sub, priority FROM mail_queue WHERE task_id = $1`, task)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]queued{}
	for rows.Next() {
		var q queued
		if err := rows.Scan(&q.email, &q.name, &q.sub, &q.priority); err != nil {
			w.t.Fatal(err)
		}
		out[q.email] = q
	}
	return out
}

func recipient(sub uuid.UUID, email string, allowed ...string) map[string]any {
	r := map[string]any{"sub": sub}
	if email != "" {
		r["email"] = email
	}
	if allowed != nil {
		r["allowed_channels"] = allowed
	}
	return r
}

func notification(kind string, recipients ...map[string]any) map[string]any {
	return map[string]any{
		"type":       kind,
		"recipients": recipients,
		"variables":  map[string]any{"EventName": "Kodlama Gecesi"},
		"source":     map[string]any{"kind": "event", "ref": "evt-1"},
	}
}

// A required type goes on its channels whatever the person allows, in its
// lane, to the person by sub.
func TestARequiredTypeIgnoresAllowedChannels(t *testing.T) {
	w := newNotificationWorld(t)
	s := w.post(bearer(t, coreService, true, coreRoles...), "cert-1", notification("core.certificate",
		recipient(personAyse, "", []string{}...), recipient(personMehmet, "", "push")))
	if s.status != fiber.StatusCreated {
		t.Fatalf("status %d %s", s.status, s.raw)
	}
	email := s.result.Channels["email"]
	if email.Queued != 2 || len(email.Skipped) != 0 || email.TaskID == nil || s.result.Recipients != 2 {
		t.Fatalf("result %s", s.raw)
	}
	rows := w.rows(*email.TaskID)
	ayse := rows["ayse@example.com"]
	if len(rows) != 2 || ayse.name != "Ayşe Kaya" || ayse.sub == nil || *ayse.sub != personAyse || ayse.priority != int16(mailer.PrioritySystem) {
		t.Fatalf("rows %+v", rows)
	}
	if n := w.count(`SELECT count(*) FROM mail_tasks WHERE notification_id = $1 AND id = $2`, s.result.ID, *email.TaskID); n != 1 {
		t.Errorf("the send is not the notification's: %d", n)
	}
}

// An optional type goes only where core allows the person: an empty
// allowed_channels opens no channel, and every recipient must carry the field.
func TestAnOptionalTypeGoesOnlyWhereThePersonAllows(t *testing.T) {
	w := newNotificationWorld(t)
	token := bearer(t, coreService, true, coreRoles...)
	s := w.post(token, "cancel-1", notification("event.cancelled",
		recipient(personAyse, "ayse.kisisel@example.com", []string{}...),
		recipient(personMehmet, "", "email"),
		recipient(personZeynep, "zeynep@example.com", "push", "inbox")))
	if s.status != fiber.StatusCreated {
		t.Fatalf("status %d %s", s.status, s.raw)
	}
	email := s.result.Channels["email"]
	if email.Queued != 1 || email.Skipped["not_allowed"] != 2 || email.TaskID == nil {
		t.Fatalf("result %s", s.raw)
	}
	rows := w.rows(*email.TaskID)
	if _, ok := rows["mehmet@example.com"]; !ok || len(rows) != 1 || rows["mehmet@example.com"].priority != int16(mailer.PriorityBulk) {
		t.Fatalf("rows %+v", rows)
	}

	s = w.post(token, "cancel-2", notification("event.cancelled", recipient(personAyse, "ayse@example.com", "email"), recipient(personMehmet, "")))
	if s.status != fiber.StatusBadRequest || s.error.Code != "notification.allowed_channels_required" || s.error.Params["recipient"] != float64(1) {
		t.Fatalf("missing allowed_channels: %d %s", s.status, s.raw)
	}

	// Nobody allowed: the notification is kept with its answer, no send.
	s = w.post(token, "cancel-3", notification("event.cancelled", recipient(personAyse, "ayse@example.com", []string{}...)))
	if s.status != fiber.StatusCreated || s.result.Channels["email"].TaskID != nil || s.result.Channels["email"].Skipped["not_allowed"] != 1 {
		t.Fatalf("nobody allowed: %d %s", s.status, s.raw)
	}
}

// Without an address in the request the person's primary e-mail is read from
// Keycloak: an unknown person, a disabled account, no address and an
// unverified one are skipped, each for its reason.
func TestRecipientsWithoutAnAddressAreSkippedForTheirReason(t *testing.T) {
	w := newNotificationWorld(t)
	s := w.post(bearer(t, coreService, true, coreRoles...), "cert-2", notification("core.certificate",
		recipient(personAyse, ""), recipient(personGone, ""), recipient(personOff, ""),
		recipient(personNoMail, ""), recipient(personPending, ""), recipient(personMehmet, "mehmet.okul@example.com"),
		recipient(personAyse, "")))
	if s.status != fiber.StatusCreated {
		t.Fatalf("status %d %s", s.status, s.raw)
	}
	email := s.result.Channels["email"]
	want := map[string]int{"unknown_person": 1, "disabled": 1, "no_address": 1, "unverified": 1}
	if email.Queued != 2 || len(email.Skipped) != len(want) || s.result.Duplicates != 1 || s.result.Recipients != 6 {
		t.Fatalf("result %s", s.raw)
	}
	for reason, n := range want {
		if email.Skipped[reason] != n {
			t.Errorf("skipped %s = %d, want %d", reason, email.Skipped[reason], n)
		}
	}
	rows := w.rows(*email.TaskID)
	if _, ok := rows["ayse@example.com"]; !ok {
		t.Errorf("Keycloak's address was not used: %+v", rows)
	}
	if _, ok := rows["mehmet.okul@example.com"]; !ok {
		t.Errorf("the request's address was not used: %+v", rows)
	}
	if w.people.asked != 5 {
		t.Errorf("Keycloak asked %d times, want 5 (once per person without an address)", w.people.asked)
	}

	// Keycloak not answering queues nothing and keeps nothing.
	before := w.count(`SELECT count(*) FROM notifications`)
	s = w.post(bearer(t, coreService, true, coreRoles...), "cert-3", notification("core.certificate", recipient(personAyse, ""), recipient(personBroken, "")))
	if s.status != fiber.StatusServiceUnavailable || s.error.Code != "notification.directory_unavailable" {
		t.Fatalf("Keycloak down: %d %s", s.status, s.raw)
	}
	if after := w.count(`SELECT count(*) FROM notifications`); after != before {
		t.Errorf("a notification was kept although Keycloak did not answer")
	}
}

// The same Idempotency-Key from the same caller is one notification, one
// send and one row; with another request it is refused; another caller's key
// is its own.
func TestTheSameIdempotencyKeyQueuesOnce(t *testing.T) {
	w := newNotificationWorld(t)
	token := bearer(t, coreService, true, coreRoles...)
	body := notification("core.certificate", recipient(personAyse, "ayse@example.com"))
	first := w.post(token, "cert-once", body)
	second := w.post(token, "cert-once", body)
	if first.status != fiber.StatusCreated || second.status != fiber.StatusCreated || !reflect.DeepEqual(first.result, second.result) {
		t.Fatalf("first %d %s, second %d %s", first.status, first.raw, second.status, second.raw)
	}
	if first.replayed != "" || second.replayed != "true" {
		t.Errorf("Idempotent-Replayed first %q second %q", first.replayed, second.replayed)
	}
	if n := w.count(`SELECT count(*) FROM notifications`); n != 1 {
		t.Errorf("%d notifications, want 1", n)
	}
	if n := w.count(`SELECT count(*) FROM mail_tasks`); n != 1 {
		t.Errorf("%d sends, want 1", n)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue`); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}

	other := notification("core.certificate", recipient(personMehmet, "mehmet@example.com"))
	if s := w.post(token, "cert-once", other); s.status != fiber.StatusUnprocessableEntity || s.error.Code != "notification.idempotency_key_reused" {
		t.Fatalf("another request with the key: %d %s", s.status, s.raw)
	}
	if s := w.post(token, "", body); s.status != fiber.StatusBadRequest || s.error.Code != "notification.idempotency_key_required" {
		t.Fatalf("no key: %d %s", s.status, s.raw)
	}
	if s := w.post(bearer(t, formsBot, true, coreRoles...), "cert-once", body); s.status != fiber.StatusCreated || s.replayed != "" || s.result.ID == first.result.ID {
		t.Fatalf("another caller's key: %d %s", s.status, s.raw)
	}
	if n := w.count(`SELECT count(*) FROM mail_queue`); n != 2 {
		t.Errorf("%d rows after another caller, want 2", n)
	}
}

// Two requests with one key at the same time queue once: the second waits
// for the first and answers its answer.
func TestConcurrentRequestsWithOneKeyQueueOnce(t *testing.T) {
	w := newNotificationWorld(t)
	token := bearer(t, coreService, true, coreRoles...)
	body := notification("core.certificate", recipient(personAyse, "ayse@example.com"), recipient(personMehmet, "mehmet@example.com"))
	var wg sync.WaitGroup
	results := make([]sent, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = w.post(token, "cert-race", body)
		}(i)
	}
	wg.Wait()
	for i, s := range results {
		if s.status != fiber.StatusCreated || s.result.ID != results[0].result.ID {
			t.Errorf("request %d: %d %s", i, s.status, s.raw)
		}
	}
	if n := w.count(`SELECT count(*) FROM mail_queue`); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}

// Only a service account's token with skymail:notifications:send may send.
func TestOnlyAServiceAccountWithTheRoleSendsNotifications(t *testing.T) {
	w := newNotificationWorld(t)
	body := notification("core.certificate", recipient(personAyse, "ayse@example.com"))
	if s := w.post(bearer(t, personAyse.String(), false, coreRoles...), "k1", body); s.status != fiber.StatusForbidden || s.error.Code != "auth.service_account_required" {
		t.Errorf("a person's token: %d %s", s.status, s.raw)
	}
	if s := w.post(bearer(t, coreService, true, "skymail:access", "skymail:mails:send"), "k2", body); s.status != fiber.StatusForbidden || s.error.Code != "server.forbidden" {
		t.Errorf("no role: %d %s", s.status, s.raw)
	}
	if n := w.count(`SELECT count(*) FROM notifications`); n != 0 {
		t.Errorf("%d notifications kept", n)
	}
}

// A type that is not there or archived is 422; one going by e-mail without a
// template of its key is 409.
func TestAnUnknownTypeOrOneWithoutItsEmailIsRefused(t *testing.T) {
	w := newNotificationWorld(t)
	token := bearer(t, coreService, true, coreRoles...)
	for kind, want := range map[string]struct {
		status int
		code   string
	}{
		"event.nothing": {fiber.StatusUnprocessableEntity, "notification.type_unknown"},
		"event.old":     {fiber.StatusUnprocessableEntity, "notification.type_unknown"},
		"event.orphan":  {fiber.StatusConflict, "notification.email_variant_missing"},
	} {
		s := w.post(token, "k-"+kind, notification(kind, recipient(personAyse, "ayse@example.com", "email")))
		if s.status != want.status || s.error.Code != want.code {
			t.Errorf("%s: %d %s, want %d %s", kind, s.status, s.raw, want.status, want.code)
		}
	}
}

// The Template seed states a type; it is read back as written.
func TestTheSeedStatesANotificationType(t *testing.T) {
	w := newNotificationWorld(t)
	seed := bearer(t, formsBot, false, "skymail:access", "skymail:templates:write", "skymail:templates:read")
	call := func(method, path string, body any) (int, []byte) {
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		}
		request := httptest.NewRequest(method, path, reader)
		request.Header.Set("Authorization", "Bearer "+seed)
		request.Header.Set("Content-Type", "application/json")
		response, err := w.app.Test(request)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(response.Body)
		return response.StatusCode, raw
	}
	status, raw := call(http.MethodPut, "/v1/notification_types/team.membership-added",
		map[string]any{"category": "teams", "required": false, "default_channels": []string{"email"}, "lane": "bulk"})
	var got handlers.NotificationType
	if status != fiber.StatusOK || json.Unmarshal(raw, &got) != nil || got.Key != "team.membership-added" ||
		got.Category == nil || *got.Category != "teams" || got.Required || got.Lane != "bulk" {
		t.Fatalf("PUT: %d %s", status, raw)
	}
	status, raw = call(http.MethodPut, "/v1/notification_types/team.membership-added",
		map[string]any{"category": nil, "required": true, "default_channels": []string{"email"}, "lane": "system"})
	if status != fiber.StatusOK || json.Unmarshal(raw, &got) != nil || got.Category != nil || !got.Required || got.Lane != "system" {
		t.Fatalf("PUT again: %d %s", status, raw)
	}
	if status, raw = call(http.MethodGet, "/v1/notification_types/team.membership-added", nil); status != fiber.StatusOK || !strings.Contains(string(raw), `"lane":"system"`) {
		t.Fatalf("GET: %d %s", status, raw)
	}
	for _, body := range []map[string]any{
		{"default_channels": []string{"push"}, "lane": "bulk"},
		{"default_channels": []string{"email"}, "lane": "fast"},
		{"category": "Kötü Kategori", "default_channels": []string{"email"}, "lane": "bulk"},
	} {
		if status, raw := call(http.MethodPut, "/v1/notification_types/team.membership-added", body); status != fiber.StatusBadRequest {
			t.Errorf("PUT %v: %d %s, want 400", body, status, raw)
		}
	}
}

// A recipient's unsubscribe_url (bounce ticket 11) is core's signed link: an
// optional type of the bulk lane keeps it on the person's row and renders it
// as .UnsubscribeUrl; a person without one gets neither. A required or
// system-lane type refuses it, and so does a link the header could not carry.
// A request without the field hashes as before it existed.
func TestAnUnsubscribeURLGoesOnlyWithAnOptionalBulkType(t *testing.T) {
	w := newNotificationWorld(t)
	ctx := context.Background()
	for _, statement := range []string{
		`INSERT INTO templates (key, name, subject, html_content, plain_text_content, react_email_content, content_sensitive)
		 VALUES ('club.digest', 'Duyuru', 'Duyuru', '<p>{{.FullName}}</p><a href="{{.UnsubscribeUrl}}">Abonelikten çık</a>', 'Çık: {{.UnsubscribeUrl}}', '', false),
		        ('event.reminder', 'Hatırlatma', 'Hatırlatma', '<p>{{.FullName}}</p>', '{{.FullName}}', '', false)`,
		`INSERT INTO notification_types (key, category, required, default_channels, lane)
		 VALUES ('club.digest', 'news', false, '{email}', 'bulk'),
		        ('event.reminder', 'events', false, '{email}', 'system')`,
	} {
		if _, err := w.store.Conn.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	token := bearer(t, coreService, true, coreRoles...)
	link := "https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=AQEBGqzN0x8K-_c2Vj"
	withLink := func(r map[string]any, url string) map[string]any {
		r["unsubscribe_url"] = url
		return r
	}

	s := w.post(token, "digest-1", notification("club.digest",
		withLink(recipient(personAyse, "", "email"), link), recipient(personMehmet, "", "email")))
	if s.status != fiber.StatusCreated || s.result.Channels["email"].Queued != 2 {
		t.Fatalf("status %d %s", s.status, s.raw)
	}
	rows, err := w.store.Conn.Query(ctx, `SELECT recipient_email, list_unsubscribe_url, body, body_html FROM mail_queue WHERE task_id = $1`,
		*s.result.Channels["email"].TaskID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var email, body string
		var url, page *string
		if err := rows.Scan(&email, &url, &body, &page); err != nil {
			t.Fatal(err)
		}
		seen++
		switch email {
		case "ayse@example.com":
			if url == nil || *url != link || body != "Çık: "+link || page == nil || !strings.Contains(*page, `href="`+link+`"`) {
				t.Errorf("Ayşe's row: link %v, body %q, html %v", url, body, page)
			}
		case "mehmet@example.com":
			if url != nil || strings.Contains(body, "https://") {
				t.Errorf("Mehmet's row: link %v, body %q", url, body)
			}
		default:
			t.Errorf("a row for %s", email)
		}
	}
	if seen != 2 {
		t.Fatalf("%d rows", seen)
	}

	for name, tc := range map[string]struct {
		kind, url, code string
	}{
		"a required type":       {"core.certificate", link, "notification.unsubscribe_url_not_allowed"},
		"a system-lane type":    {"event.reminder", link, "notification.unsubscribe_url_not_allowed"},
		"http":                  {"club.digest", "http://api.yildizskylab.com/u?t=AQ", "notification.unsubscribe_url_invalid"},
		"a line break":          {"club.digest", "https://api.yildizskylab.com/u?t=AQ\r\nBcc: x@example.com", "notification.unsubscribe_url_invalid"},
		"an angle bracket":      {"club.digest", "https://api.yildizskylab.com/u?t=AQ>", "notification.unsubscribe_url_invalid"},
		"a user in the address": {"club.digest", "https://user@api.yildizskylab.com/u?t=AQ", "notification.unsubscribe_url_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			s := w.post(token, "digest-"+strings.ReplaceAll(name, " ", "-"), notification(tc.kind,
				recipient(personMehmet, "", "email"), withLink(recipient(personAyse, "", "email"), tc.url)))
			if s.status != fiber.StatusBadRequest || s.error.Code != tc.code || s.error.Params["recipient"] != float64(1) {
				t.Fatalf("%d %s, want 400 %s for recipient 1", s.status, s.raw, tc.code)
			}
		})
	}
	if n := w.count(`SELECT count(*) FROM mail_queue WHERE list_unsubscribe_url IS NOT NULL`); n != 1 {
		t.Errorf("rows with a link = %d, want Ayşe's alone", n)
	}

	// The field is left out of a request's JSON when empty, so a retry from
	// before it hashes the same.
	raw, err := json.Marshal(requests.NotificationRecipient{Sub: personAyse})
	if err != nil || strings.Contains(string(raw), "unsubscribe_url") {
		t.Errorf("an empty unsubscribe_url is written: %s %v", raw, err)
	}
}
