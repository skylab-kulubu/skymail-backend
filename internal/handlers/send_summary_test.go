package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/keycloak"
)

// The club is in Turkey: "today" on the home screen is a Turkish operator's
// today, whatever the server's clock zone is.
var istanbul = func() *time.Location {
	location, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		panic(err)
	}
	return location
}()

func istanbulTime(year int, month time.Month, day, hour, minute, second int) time.Time {
	return time.Date(year, month, day, hour, minute, second, 0, istanbul)
}

type seededRecipient struct {
	status   database.MailQueueStatus
	queuedAt time.Time
}

type seededSend struct {
	createdAt  time.Time
	templateID *uuid.UUID
	mailListID *uuid.UUID
	recipients []seededRecipient
}

// seedSend writes a mail task and its queue rows the way the mailer leaves
// them, without going through the mailer: the summary only reads what is there.
func seedSend(t *testing.T, db *database.Store, send seededSend) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var taskID uuid.UUID
	if err := db.Conn.QueryRow(ctx, `
		INSERT INTO mail_tasks (sent_by, template_id, mail_list_id, body_variables, created_at)
		VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', $1, $2, '{}', $3)
		RETURNING id`,
		send.templateID, send.mailListID, send.createdAt,
	).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	for i, recipient := range send.recipients {
		queuedAt := recipient.queuedAt
		if queuedAt.IsZero() {
			queuedAt = send.createdAt
		}
		if _, err := db.Conn.Exec(ctx, `
			INSERT INTO mail_queue (task_id, recipient_full_name, recipient_email, subject, body, status, created_at, next_attempt_at)
			VALUES ($1, $2, $3, 'Konu', 'Gövde', $4, $5, $5)`,
			taskID, fmt.Sprintf("Alıcı %d", i+1), fmt.Sprintf("alici%d@example.com", i+1),
			string(recipient.status), queuedAt,
		); err != nil {
			t.Fatal(err)
		}
	}
	return taskID
}

func recipientsWith(status database.MailQueueStatus, count int) []seededRecipient {
	recipients := make([]seededRecipient, count)
	for i := range recipients {
		recipients[i] = seededRecipient{status: status}
	}
	return recipients
}

func sendSummaryApp(t *testing.T, db *database.Store, now time.Time, kc keycloak.Client) *fiber.App {
	t.Helper()
	handler := &mailHandlerImpl{
		db:     db,
		mailer: &lifecycleMailerStub{},
		kc:     kc,
		now:    func() time.Time { return now },
	}
	app := fiber.New(fiber.Config{ErrorHandler: func(c fiber.Ctx, err error) error {
		var appErr *apperrors.AppError
		if errors.As(err, &appErr) {
			return c.Status(appErr.Status).JSON(appErr)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return c.SendStatus(fiber.StatusNotFound)
		}
		return c.Status(fiber.StatusInternalServerError).SendString(err.Error())
	}})
	app.Get("/mail_tasks/summary", handler.GetSummary)
	app.Get("/mail_tasks", handler.GetTasks)
	return app
}

func getJSON(t *testing.T, app *fiber.App, path string, out any) *http.Response {
	t.Helper()
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, response.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("GET %s: decode %s: %v", path, body, err)
	}
	return response
}

type summaryCounts struct {
	Pending    int64 `json:"pending"`
	Processing int64 `json:"processing"`
	Sent       int64 `json:"sent"`
	Failed     int64 `json:"failed"`
}

type summaryDay struct {
	Date string `json:"date"`
	Sent int64  `json:"sent"`
}

type summaryAudience struct {
	Kind              string  `json:"kind"`
	MailListID        *string `json:"mail_list_id"`
	Name              *string `json:"name"`
	Source            *string `json:"source"`
	RecipientFullName *string `json:"recipient_full_name"`
	RecipientEmail    *string `json:"recipient_email"`
}

type summarySend struct {
	ID              uuid.UUID       `json:"id"`
	CreatedAt       time.Time       `json:"created_at"`
	SentBy          string          `json:"sent_by"`
	TemplateID      *uuid.UUID      `json:"template_id"`
	TemplateName    *string         `json:"template_name"`
	TemplateKey     *string         `json:"template_key"`
	Audience        summaryAudience `json:"audience"`
	Status          string          `json:"status"`
	RecipientCounts summaryCounts   `json:"recipient_counts"`
}

type sendCounts struct {
	Failed  int64 `json:"failed"`
	Sending int64 `json:"sending"`
	Sent    int64 `json:"sent"`
}

type summaryResponse struct {
	TimeZone    string        `json:"time_zone"`
	QueueCounts summaryCounts `json:"queue_counts"`
	SendCounts  sendCounts    `json:"send_counts"`
	DailySent   []summaryDay  `json:"daily_sent"`
	RecentSends []summarySend `json:"recent_sends"`
}

func TestSendSummaryCountsQueueRowsByStatus(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)

	// Every status the queue has, spread over sends of different ages: the
	// counts describe the queue as it stands, not a window of it.
	seedSend(t, db, seededSend{createdAt: now.Add(-time.Hour), recipients: append(
		recipientsWith(database.MailQueueStatusPending, 2),
		recipientsWith(database.MailQueueStatusProcessing, 1)...,
	)})
	seedSend(t, db, seededSend{createdAt: now.AddDate(0, -3, 0), recipients: append(
		recipientsWith(database.MailQueueStatusSent, 4),
		recipientsWith(database.MailQueueStatusFailed, 1)...,
	)})
	seedSend(t, db, seededSend{createdAt: now.AddDate(0, 0, -2), recipients: recipientsWith(database.MailQueueStatusSent, 3)})

	var summary summaryResponse
	getJSON(t, sendSummaryApp(t, db, now, lifecycleKeycloakStub{}), "/mail_tasks/summary", &summary)

	want := summaryCounts{Pending: 2, Processing: 1, Sent: 7, Failed: 1}
	if summary.QueueCounts != want {
		t.Fatalf("queue_counts = %+v, want %+v", summary.QueueCounts, want)
	}
}

func TestSendSummaryDailySeriesUsesIstanbulDays(t *testing.T) {
	db := lifecycleHandlerStore(t)
	// Ten past midnight in Istanbul is still the previous evening in UTC: a
	// series cut at UTC midnight would call this day the 21st.
	now := istanbulTime(2026, time.September, 22, 0, 10, 0)

	seedSend(t, db, seededSend{createdAt: istanbulTime(2026, time.September, 21, 23, 50, 0), recipients: []seededRecipient{
		// Half a minute either side of Istanbul midnight — the same UTC date.
		{status: database.MailQueueStatusSent, queuedAt: istanbulTime(2026, time.September, 21, 23, 59, 30)},
		{status: database.MailQueueStatusSent, queuedAt: istanbulTime(2026, time.September, 22, 0, 0, 30)},
		{status: database.MailQueueStatusSent, queuedAt: istanbulTime(2026, time.September, 22, 0, 5, 0)},
		// Not sent, so not in the series.
		{status: database.MailQueueStatusFailed, queuedAt: istanbulTime(2026, time.September, 22, 0, 1, 0)},
		{status: database.MailQueueStatusPending, queuedAt: istanbulTime(2026, time.September, 22, 0, 9, 0)},
	}})
	seedSend(t, db, seededSend{createdAt: istanbulTime(2026, time.September, 16, 9, 0, 0), recipients: recipientsWith(database.MailQueueStatusSent, 4)})
	seedSend(t, db, seededSend{createdAt: istanbulTime(2026, time.September, 16, 9, 0, 0), recipients: []seededRecipient{
		// The window's first day starts at its Istanbul midnight, inclusive;
		// the second before it is outside.
		{status: database.MailQueueStatusSent, queuedAt: istanbulTime(2026, time.September, 16, 0, 0, 0)},
		{status: database.MailQueueStatusSent, queuedAt: istanbulTime(2026, time.September, 15, 23, 59, 59)},
	}})

	var summary summaryResponse
	getJSON(t, sendSummaryApp(t, db, now, lifecycleKeycloakStub{}), "/mail_tasks/summary?days=7", &summary)

	want := []summaryDay{
		{Date: "2026-09-16", Sent: 5},
		{Date: "2026-09-17", Sent: 0},
		{Date: "2026-09-18", Sent: 0},
		{Date: "2026-09-19", Sent: 0},
		{Date: "2026-09-20", Sent: 0},
		{Date: "2026-09-21", Sent: 1},
		{Date: "2026-09-22", Sent: 2},
	}
	if fmt.Sprint(summary.DailySent) != fmt.Sprint(want) {
		t.Fatalf("daily_sent = %v, want %v", summary.DailySent, want)
	}
	if summary.TimeZone != "Europe/Istanbul" {
		t.Fatalf("time_zone = %q, want Europe/Istanbul", summary.TimeZone)
	}
}

func TestSendSummaryDailySeriesDefaultsToThirtyDays(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)

	var summary summaryResponse
	getJSON(t, sendSummaryApp(t, db, now, lifecycleKeycloakStub{}), "/mail_tasks/summary", &summary)

	if len(summary.DailySent) != 30 {
		t.Fatalf("daily_sent has %d days, want 30", len(summary.DailySent))
	}
	first, last := summary.DailySent[0], summary.DailySent[len(summary.DailySent)-1]
	if first.Date != "2026-08-24" || last.Date != "2026-09-22" {
		t.Fatalf("daily_sent runs %s..%s, want 2026-08-24..2026-09-22", first.Date, last.Date)
	}
	for _, day := range summary.DailySent {
		if day.Sent != 0 {
			t.Fatalf("empty queue sent %d on %s", day.Sent, day.Date)
		}
	}
}

// groupKeycloakStub knows a fixed set of groups by id; it answers any other id
// the way Keycloak answers a group that no longer exists.
type groupKeycloakStub struct {
	lifecycleKeycloakStub
	names map[string]string
}

func (k groupKeycloakStub) GetGroup(_ context.Context, id string) (*gocloak.Group, error) {
	name, ok := k.names[id]
	if !ok {
		return nil, nil
	}
	path := "/" + name
	return &gocloak.Group{ID: &id, Name: &name, Path: &path}, nil
}

type unreachableKeycloakStub struct {
	lifecycleKeycloakStub
}

func (unreachableKeycloakStub) GetGroup(context.Context, string) (*gocloak.Group, error) {
	return nil, errors.New("keycloak: connection refused")
}

func stringValue(value *string) string {
	if value == nil {
		return "<nil>"
	}
	return *value
}

func TestSendSummaryListsRecentSendsWithDerivedStatus(t *testing.T) {
	db := lifecycleHandlerStore(t)
	ctx := context.Background()
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)

	welcomeKey := "core.welcome"
	welcome, err := db.CreateTemplate(ctx, database.CreateTemplateParams{
		Name: "Hoş geldin", Subject: "SKY LAB'e hoş geldin", HtmlContent: "<p>{{.FullName}}</p>",
		PlainTextContent: "{{.FullName}}", ReactEmailContent: "{}", Key: &welcomeKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	bulletin, err := db.CreateTemplate(ctx, database.CreateTemplateParams{
		Name: "Bülten", Subject: "Eylül bülteni", HtmlContent: "<p>Bülten</p>",
		PlainTextContent: "Bülten", ReactEmailContent: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	webLab, err := db.CreateMailingList(ctx, "WebLab")
	if err != nil {
		t.Fatal(err)
	}
	members := uuid.MustParse("5c0f3f6e-2d7a-4b43-9b8e-6a1d2f0c9e11")

	// Oldest first; with the default of five, the first one drops off.
	seedSend(t, db, seededSend{createdAt: now.Add(-6 * time.Hour), templateID: &bulletin.ID, mailListID: &webLab.ID,
		recipients: recipientsWith(database.MailQueueStatusSent, 2)})
	single := seedSend(t, db, seededSend{createdAt: now.Add(-5 * time.Hour), templateID: &welcome.ID,
		recipients: recipientsWith(database.MailQueueStatusSent, 1)})
	oneFailed := seedSend(t, db, seededSend{createdAt: now.Add(-4 * time.Hour), templateID: &bulletin.ID, mailListID: &webLab.ID,
		recipients: append(recipientsWith(database.MailQueueStatusFailed, 1), recipientsWith(database.MailQueueStatusSent, 3)...)})
	stillGoing := seedSend(t, db, seededSend{createdAt: now.Add(-3 * time.Hour), templateID: &bulletin.ID, mailListID: &members,
		recipients: append(append(recipientsWith(database.MailQueueStatusSent, 2),
			recipientsWith(database.MailQueueStatusPending, 1)...),
			recipientsWith(database.MailQueueStatusProcessing, 1)...)})
	// Queued no one — the enqueue failed after the task was written, or the list
	// was empty — and it is long past the moment its rows would have appeared.
	nobody := seedSend(t, db, seededSend{createdAt: now.Add(-2 * time.Hour), templateID: &bulletin.ID, mailListID: &webLab.ID})
	allSent := seedSend(t, db, seededSend{createdAt: now.Add(-1 * time.Hour), templateID: &bulletin.ID, mailListID: &webLab.ID,
		recipients: recipientsWith(database.MailQueueStatusSent, 2)})

	app := sendSummaryApp(t, db, now, groupKeycloakStub{names: map[string]string{members.String(): "Üyeler"}})
	var summary summaryResponse
	getJSON(t, app, "/mail_tasks/summary", &summary)

	wantOrder := []uuid.UUID{allSent, nobody, stillGoing, oneFailed, single}
	if len(summary.RecentSends) != len(wantOrder) {
		t.Fatalf("recent_sends has %d sends, want %d", len(summary.RecentSends), len(wantOrder))
	}
	for i, id := range wantOrder {
		if summary.RecentSends[i].ID != id {
			t.Fatalf("recent_sends[%d] = %s, want %s (newest first)", i, summary.RecentSends[i].ID, id)
		}
	}

	byID := map[uuid.UUID]summarySend{}
	for _, send := range summary.RecentSends {
		byID[send.ID] = send
	}
	for _, tc := range []struct {
		name   string
		id     uuid.UUID
		status string
		counts summaryCounts
	}{
		{"every recipient sent", allSent, "sent", summaryCounts{Sent: 2}},
		{"no recipients queued", nobody, "failed", summaryCounts{}},
		{"some still queued", stillGoing, "sending", summaryCounts{Pending: 1, Processing: 1, Sent: 2}},
		{"one failed among sent", oneFailed, "failed", summaryCounts{Sent: 3, Failed: 1}},
		{"single recipient sent", single, "sent", summaryCounts{Sent: 1}},
	} {
		send := byID[tc.id]
		if send.Status != tc.status || send.RecipientCounts != tc.counts {
			t.Errorf("%s: status=%q counts=%+v, want %q %+v", tc.name, send.Status, send.RecipientCounts, tc.status, tc.counts)
		}
	}

	listSend := byID[oneFailed]
	if stringValue(listSend.TemplateName) != "Bülten" || listSend.TemplateKey != nil || listSend.TemplateID == nil || *listSend.TemplateID != bulletin.ID {
		t.Errorf("list send template = %v %s %s", listSend.TemplateID, stringValue(listSend.TemplateName), stringValue(listSend.TemplateKey))
	}
	if !listSend.CreatedAt.Equal(now.Add(-4 * time.Hour)) {
		t.Errorf("list send created_at = %s, want %s", listSend.CreatedAt, now.Add(-4*time.Hour))
	}
	if a := listSend.Audience; a.Kind != "mailing_list" || stringValue(a.MailListID) != webLab.ID.String() ||
		stringValue(a.Name) != "WebLab" || stringValue(a.Source) != "internal" {
		t.Errorf("internal list audience = %+v", a)
	}

	if a := byID[stillGoing].Audience; a.Kind != "mailing_list" || stringValue(a.MailListID) != members.String() ||
		stringValue(a.Name) != "Üyeler" || stringValue(a.Source) != "keycloak" {
		t.Errorf("keycloak group audience = %+v", a)
	}

	singleSend := byID[single]
	if stringValue(singleSend.TemplateName) != "Hoş geldin" || stringValue(singleSend.TemplateKey) != "core.welcome" {
		t.Errorf("single send template = %s %s", stringValue(singleSend.TemplateName), stringValue(singleSend.TemplateKey))
	}
	if a := singleSend.Audience; a.Kind != "single" || a.MailListID != nil || a.Name != nil ||
		stringValue(a.RecipientFullName) != "Alıcı 1" || stringValue(a.RecipientEmail) != "alici1@example.com" {
		t.Errorf("single audience = %+v", a)
	}

	var fewer summaryResponse
	getJSON(t, app, "/mail_tasks/summary?recent=2", &fewer)
	if len(fewer.RecentSends) != 2 || fewer.RecentSends[0].ID != allSent || fewer.RecentSends[1].ID != nobody {
		t.Fatalf("recent=2 gave %d sends", len(fewer.RecentSends))
	}
}

func TestSendSummaryKeepsWorkingWhenKeycloakCannotNameAGroup(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	group := uuid.MustParse("0b9f7c52-7c1e-4f55-9d0e-2a6f4c1d8b33")
	seedSend(t, db, seededSend{createdAt: now.Add(-time.Hour), mailListID: &group,
		recipients: recipientsWith(database.MailQueueStatusSent, 1)})

	// A group name is a nicety on the home screen; Keycloak being down must not
	// take the whole summary with it.
	var summary summaryResponse
	getJSON(t, sendSummaryApp(t, db, now, unreachableKeycloakStub{}), "/mail_tasks/summary", &summary)

	if len(summary.RecentSends) != 1 {
		t.Fatalf("recent_sends has %d sends, want 1", len(summary.RecentSends))
	}
	if a := summary.RecentSends[0].Audience; a.Kind != "mailing_list" || stringValue(a.MailListID) != group.String() ||
		a.Name != nil || stringValue(a.Source) != "keycloak" {
		t.Fatalf("audience = %+v, want the group id with no name", a)
	}
}

type listedSend struct {
	ID              uuid.UUID     `json:"id"`
	Status          string        `json:"status"`
	RecipientCounts summaryCounts `json:"recipient_counts"`
}

func listSends(t *testing.T, app *fiber.App, path string) ([]listedSend, string) {
	t.Helper()
	var sends []listedSend
	response := getJSON(t, app, path, &sends)
	return sends, response.Header.Get("X-Total-Count")
}

func sendIDs(sends []listedSend) []uuid.UUID {
	ids := make([]uuid.UUID, len(sends))
	for i, send := range sends {
		ids[i] = send.ID
	}
	return ids
}

func TestSendListFiltersByDerivedStatus(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	at := func(hoursAgo int) time.Time { return now.Add(-time.Duration(hoursAgo) * time.Hour) }

	// One failed recipient among several sent ones is enough to call a send
	// failed — the operator has someone to follow up on.
	oneFailed := seedSend(t, db, seededSend{createdAt: at(7), recipients: append(
		recipientsWith(database.MailQueueStatusSent, 3), recipientsWith(database.MailQueueStatusFailed, 1)...)})
	allSent := seedSend(t, db, seededSend{createdAt: at(6), recipients: recipientsWith(database.MailQueueStatusSent, 2)})
	// A failure wins over recipients still in the queue.
	failedWhileQueued := seedSend(t, db, seededSend{createdAt: at(5), recipients: append(
		recipientsWith(database.MailQueueStatusFailed, 1), recipientsWith(database.MailQueueStatusPending, 2)...)})
	pending := seedSend(t, db, seededSend{createdAt: at(4), recipients: append(
		recipientsWith(database.MailQueueStatusSent, 1), recipientsWith(database.MailQueueStatusPending, 1)...)})
	allFailed := seedSend(t, db, seededSend{createdAt: at(3), recipients: recipientsWith(database.MailQueueStatusFailed, 2)})
	processing := seedSend(t, db, seededSend{createdAt: at(2), recipients: recipientsWith(database.MailQueueStatusProcessing, 1)})
	queuedNobody := seedSend(t, db, seededSend{createdAt: at(1)})

	app := sendSummaryApp(t, db, now, lifecycleKeycloakStub{})

	for _, tc := range []struct {
		status string
		want   []uuid.UUID
	}{
		{"failed", []uuid.UUID{queuedNobody, allFailed, failedWhileQueued, oneFailed}},
		{"sending", []uuid.UUID{processing, pending}},
		{"sent", []uuid.UUID{allSent}},
		{"FAILED", []uuid.UUID{queuedNobody, allFailed, failedWhileQueued, oneFailed}},
	} {
		sends, total := listSends(t, app, "/mail_tasks?status="+tc.status)
		if fmt.Sprint(sendIDs(sends)) != fmt.Sprint(tc.want) || total != fmt.Sprint(len(tc.want)) {
			t.Errorf("status=%s: sends=%v total=%s, want %v total=%d", tc.status, sendIDs(sends), total, tc.want, len(tc.want))
		}
		for _, send := range sends {
			if !strings.EqualFold(send.Status, tc.status) {
				t.Errorf("status=%s listed a %s send", tc.status, send.Status)
			}
		}
	}

	// Pages are cut from the filtered sends, and the total counts all of them.
	firstPage, total := listSends(t, app, "/mail_tasks?status=failed&_start=0&_end=3")
	if fmt.Sprint(sendIDs(firstPage)) != fmt.Sprint([]uuid.UUID{queuedNobody, allFailed, failedWhileQueued}) || total != "4" {
		t.Errorf("failed page 1 = %v total=%s", sendIDs(firstPage), total)
	}
	secondPage, total := listSends(t, app, "/mail_tasks?status=failed&_start=3&_end=6")
	if fmt.Sprint(sendIDs(secondPage)) != fmt.Sprint([]uuid.UUID{oneFailed}) || total != "4" {
		t.Errorf("failed page 2 = %v total=%s", sendIDs(secondPage), total)
	}
	pastTheEnd, total := listSends(t, app, "/mail_tasks?status=failed&_start=10&_end=20")
	if len(pastTheEnd) != 0 || total != "4" {
		t.Errorf("failed past the end = %v total=%s", sendIDs(pastTheEnd), total)
	}

	// Without a filter every send is listed, each with its status and counts.
	everything, total := listSends(t, app, "/mail_tasks?_start=0&_end=50")
	if len(everything) != 7 || total != "7" {
		t.Fatalf("unfiltered list = %d sends total=%s, want 7", len(everything), total)
	}
	for _, send := range everything {
		if send.ID == oneFailed && (send.Status != "failed" || send.RecipientCounts != (summaryCounts{Sent: 3, Failed: 1})) {
			t.Errorf("one-failed send listed as %q %+v", send.Status, send.RecipientCounts)
		}
	}
}

func TestSendListKeepsTheFieldsTheOldPanelReads(t *testing.T) {
	db := lifecycleHandlerStore(t)
	ctx := context.Background()
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	template, err := db.CreateTemplate(ctx, database.CreateTemplateParams{
		Name: "Bülten", Subject: "Konu", HtmlContent: "<p>x</p>", PlainTextContent: "x", ReactEmailContent: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.CreateMailingList(ctx, "WebLab")
	if err != nil {
		t.Fatal(err)
	}
	id := seedSend(t, db, seededSend{createdAt: now.Add(-time.Hour), templateID: &template.ID, mailListID: &list.ID,
		recipients: recipientsWith(database.MailQueueStatusSent, 1)})

	var rows []map[string]any
	response := getJSON(t, sendSummaryApp(t, db, now, lifecycleKeycloakStub{}), "/mail_tasks", &rows)
	if response.Header.Get("X-Total-Count") != "1" || len(rows) != 1 {
		t.Fatalf("list = %v total=%s", rows, response.Header.Get("X-Total-Count"))
	}
	row := rows[0]
	want := map[string]any{
		"id":             id.String(),
		"sent_by":        "31ef736f-72da-4a40-8791-d523199cf9f0",
		"template_id":    template.ID.String(),
		"template_name":  "Bülten",
		"mail_list_id":   list.ID.String(),
		"mail_list_name": "WebLab",
		"body_variables": "e30=", // the JSONB {} as bytes, exactly as it has always been encoded
	}
	for field, value := range want {
		if row[field] != value {
			t.Errorf("%s = %#v, want %#v", field, row[field], value)
		}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, fmt.Sprint(row["created_at"]))
	if err != nil || !createdAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("created_at = %v, want %s", row["created_at"], now.Add(-time.Hour))
	}
	// Only additions: the old panel ignores what it does not know.
	known := map[string]bool{"created_at": true, "status": true, "recipient_counts": true}
	for field := range want {
		known[field] = true
	}
	for field := range row {
		if !known[field] {
			t.Errorf("unexpected field %q", field)
		}
	}
}

func TestSendSummaryAndListRejectBadInput(t *testing.T) {
	db := lifecycleHandlerStore(t)
	app := sendSummaryApp(t, db, istanbulTime(2026, time.September, 22, 12, 0, 0), lifecycleKeycloakStub{})

	for _, tc := range []struct {
		path  string
		param string
	}{
		{"/mail_tasks/summary?days=0", "days"},
		{"/mail_tasks/summary?days=-3", "days"},
		{"/mail_tasks/summary?days=91", "days"},
		{"/mail_tasks/summary?days=thirty", "days"},
		{"/mail_tasks/summary?days=7.5", "days"},
		{"/mail_tasks/summary?recent=0", "recent"},
		{"/mail_tasks/summary?recent=21", "recent"},
		{"/mail_tasks/summary?recent=five", "recent"},
		{"/mail_tasks?status=deleted", "status"},
		{"/mail_tasks?status=empty", "status"},
		{"/mail_tasks?status=pending", "status"},
		{"/mail_tasks?status=processing", "status"},
	} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, tc.path, nil))
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Params  map[string]any `json:"params"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatalf("GET %s: decode: %v", tc.path, err)
		}
		if response.StatusCode != fiber.StatusBadRequest || body.Code != "validation.error" || body.Message == "" || body.Params[tc.param] == nil {
			t.Errorf("GET %s = %d %+v, want 400 validation.error naming %s", tc.path, response.StatusCode, body, tc.param)
		}
	}

	// The bounds themselves are allowed.
	for _, path := range []string{
		"/mail_tasks/summary?days=1&recent=1",
		"/mail_tasks/summary?days=90&recent=20",
		"/mail_tasks?status=",
	} {
		var out any
		getJSON(t, app, path, &out)
	}
}

// seedTaskWithoutRecipients writes only the task row, aged by the database's own
// clock: the grace period is measured against the database's now().
func seedTaskWithoutRecipients(t *testing.T, db *database.Store, age time.Duration) uuid.UUID {
	t.Helper()
	var taskID uuid.UUID
	if err := db.Conn.QueryRow(context.Background(), `
		INSERT INTO mail_tasks (sent_by, body_variables, created_at)
		VALUES ('31ef736f-72da-4a40-8791-d523199cf9f0', '{}', now() - make_interval(secs => $1))
		RETURNING id`, age.Seconds(),
	).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	return taskID
}

func TestSendThatQueuedNobodyIsFailedOnceItsRowsAreOverdue(t *testing.T) {
	db := lifecycleHandlerStore(t)
	// The mailer writes the task first and its queue rows after; a template
	// that does not parse, or a failed insert, leaves the task with none. A
	// minute is far more than that gap, so by then nothing is coming.
	overdue := seedTaskWithoutRecipients(t, db, 5*time.Minute)
	fresh := seedTaskWithoutRecipients(t, db, 0)
	app := sendSummaryApp(t, db, time.Now(), lifecycleKeycloakStub{})

	failed, total := listSends(t, app, "/mail_tasks?status=failed")
	if fmt.Sprint(sendIDs(failed)) != fmt.Sprint([]uuid.UUID{overdue}) || total != "1" {
		t.Errorf("status=failed = %v total=%s, want only the overdue send", sendIDs(failed), total)
	}
	sending, total := listSends(t, app, "/mail_tasks?status=sending")
	if fmt.Sprint(sendIDs(sending)) != fmt.Sprint([]uuid.UUID{fresh}) || total != "1" {
		t.Errorf("status=sending = %v total=%s, want only the fresh send", sendIDs(sending), total)
	}

	var summary summaryResponse
	getJSON(t, app, "/mail_tasks/summary", &summary)
	statuses := map[uuid.UUID]string{}
	for _, send := range summary.RecentSends {
		statuses[send.ID] = send.Status
	}
	if statuses[overdue] != "failed" || statuses[fresh] != "sending" {
		t.Errorf("recent_sends statuses = %v, want overdue failed and fresh sending", statuses)
	}
	if summary.SendCounts != (sendCounts{Failed: 1, Sending: 1}) {
		t.Errorf("send_counts = %+v, want one failed and one sending", summary.SendCounts)
	}
}

func TestSendSummaryCountsSendsAsTheListFiltersThem(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	at := func(hoursAgo int) time.Time { return now.Add(-time.Duration(hoursAgo) * time.Hour) }

	seedSend(t, db, seededSend{createdAt: at(9), recipients: append(
		recipientsWith(database.MailQueueStatusSent, 3), recipientsWith(database.MailQueueStatusFailed, 1)...)})
	seedSend(t, db, seededSend{createdAt: at(8), recipients: recipientsWith(database.MailQueueStatusFailed, 2)})
	seedSend(t, db, seededSend{createdAt: at(7)})
	seedSend(t, db, seededSend{createdAt: at(6), recipients: recipientsWith(database.MailQueueStatusPending, 2)})
	seedSend(t, db, seededSend{createdAt: at(5), recipients: recipientsWith(database.MailQueueStatusSent, 4)})
	seedSend(t, db, seededSend{createdAt: at(4), recipients: recipientsWith(database.MailQueueStatusSent, 1)})
	app := sendSummaryApp(t, db, now, lifecycleKeycloakStub{})

	var summary summaryResponse
	getJSON(t, app, "/mail_tasks/summary", &summary)
	if summary.SendCounts != (sendCounts{Failed: 3, Sending: 1, Sent: 2}) {
		t.Fatalf("send_counts = %+v, want 3 failed, 1 sending, 2 sent", summary.SendCounts)
	}
	// The home screen's failed tile opens ?status=failed; the number on the
	// tile and the length of that list are the same number.
	for status, count := range map[string]int64{
		"failed":  summary.SendCounts.Failed,
		"sending": summary.SendCounts.Sending,
		"sent":    summary.SendCounts.Sent,
	} {
		_, total := listSends(t, app, "/mail_tasks?status="+status)
		if total != fmt.Sprint(count) {
			t.Errorf("send_counts.%s = %d, but ?status=%s has X-Total-Count %s", status, count, status, total)
		}
	}
	// Recipients are still counted separately: five failed mails in three sends.
	if summary.QueueCounts.Failed != 3 {
		t.Errorf("queue_counts.failed = %d, want 3", summary.QueueCounts.Failed)
	}
}
