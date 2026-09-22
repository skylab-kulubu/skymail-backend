package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/docs"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

type queueRow struct {
	ID     uuid.UUID `json:"id"`
	Status struct {
		MailQueueStatus string `json:"mail_queue_status"`
		Valid           bool   `json:"valid"`
	} `json:"status"`
}

func queueRows(t *testing.T, app *fiber.App, path string) ([]queueRow, string) {
	t.Helper()
	var rows []queueRow
	response := getJSON(t, app, path, &rows)
	return rows, response.Header.Get("X-Total-Count")
}

func TestSendRecipientsPageEveryRowExactlyOnce(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	// A list send's rows are written by one insert and share one created_at, so
	// that alone cannot order them into pages.
	id := seedSend(t, db, seededSend{createdAt: now.Add(-time.Hour), recipients: append(
		recipientsWith(database.MailQueueStatusSent, 25), recipientsWith(database.MailQueueStatusFailed, 1)...)})
	app := sendSummaryApp(t, db, now, lifecycleKeycloakStub{})

	seen := map[uuid.UUID]int{}
	for start := 0; start < 26; start += 5 {
		rows, total := queueRows(t, app, fmt.Sprintf("/mail_tasks/%s/queue?_start=%d&_end=%d", id, start, start+5))
		if total != "26" {
			t.Fatalf("X-Total-Count = %s, want 26", total)
		}
		for _, row := range rows {
			seen[row.ID]++
		}
	}
	if len(seen) != 26 {
		t.Errorf("pages showed %d distinct recipients, want all 26", len(seen))
	}
	for rowID, times := range seen {
		if times != 1 {
			t.Errorf("recipient %s shown %d times across pages", rowID, times)
		}
	}
}

func TestSendRecipientsFilterByStatus(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	id := seedSend(t, db, seededSend{createdAt: now.Add(-time.Hour), recipients: append(append(append(
		recipientsWith(database.MailQueueStatusSent, 7),
		recipientsWith(database.MailQueueStatusFailed, 3)...),
		recipientsWith(database.MailQueueStatusPending, 1)...),
		recipientsWith(database.MailQueueStatusProcessing, 1)...)})
	// Another send's failures are not this send's.
	seedSend(t, db, seededSend{createdAt: now.Add(-2 * time.Hour), recipients: recipientsWith(database.MailQueueStatusFailed, 4)})
	app := sendSummaryApp(t, db, now, lifecycleKeycloakStub{})
	base := "/mail_tasks/" + id.String() + "/queue"

	for status, want := range map[string]int{"failed": 3, "FAILED": 3, "sent": 7, "pending": 1, "processing": 1, "": 12} {
		rows, total := queueRows(t, app, base+"?_start=0&_end=50&status="+status)
		if len(rows) != want || total != fmt.Sprint(want) {
			t.Errorf("status=%q: %d rows, X-Total-Count %s, want %d", status, len(rows), total, want)
		}
		for _, row := range rows {
			if status != "" && !strings.EqualFold(row.Status.MailQueueStatus, status) {
				t.Errorf("status=%q listed a %q recipient", status, row.Status.MailQueueStatus)
			}
		}
	}

	// "Only failed" holds across pages, and the total counts every failed one.
	first, total := queueRows(t, app, base+"?status=failed&_start=0&_end=2")
	second, total2 := queueRows(t, app, base+"?status=failed&_start=2&_end=4")
	if len(first) != 2 || len(second) != 1 || total != "3" || total2 != "3" {
		t.Errorf("failed pages = %d + %d rows, totals %s/%s, want 2 + 1 of 3", len(first), len(second), total, total2)
	}

	for _, bad := range []string{"bogus", "sending", "empty"} {
		response, err := app.Test(httptest.NewRequest(fiber.MethodGet, base+"?status="+bad, nil))
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Params  map[string]any `json:"params"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != fiber.StatusBadRequest || body.Code != "validation.error" || body.Message == "" || body.Params["status"] == nil {
			t.Errorf("status=%s = %d %+v, want 400 validation.error naming status", bad, response.StatusCode, body)
		}
	}
}

type sendDetail struct {
	ID              uuid.UUID       `json:"id"`
	TemplateName    *string         `json:"template_name"`
	TemplateKey     *string         `json:"template_key"`
	MailListName    *string         `json:"mail_list_name"`
	Status          string          `json:"status"`
	RecipientCounts summaryCounts   `json:"recipient_counts"`
	Audience        summaryAudience `json:"audience"`
}

func TestSendDetailCarriesStatusCountsAndAudience(t *testing.T) {
	db := lifecycleHandlerStore(t)
	ctx := context.Background()
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	key := "core.certificate"
	template, err := db.CreateTemplate(ctx, database.CreateTemplateParams{
		Name: "Sertifika", Subject: "Sertifikan hazır", HtmlContent: "<p>x</p>", PlainTextContent: "x",
		ReactEmailContent: "{}", Key: &key,
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.CreateMailingList(ctx, "GECEKODU katılımcıları")
	if err != nil {
		t.Fatal(err)
	}
	id := seedSend(t, db, seededSend{createdAt: now.Add(-time.Hour), templateID: &template.ID, mailListID: &list.ID,
		recipients: append(recipientsWith(database.MailQueueStatusSent, 3), recipientsWith(database.MailQueueStatusFailed, 1)...)})
	app := sendSummaryApp(t, db, now, lifecycleKeycloakStub{})

	var detail sendDetail
	getJSON(t, app, "/mail_tasks/"+id.String(), &detail)
	if detail.ID != id || detail.Status != "failed" || detail.RecipientCounts != (summaryCounts{Sent: 3, Failed: 1}) {
		t.Errorf("detail = %s %q %+v, want failed with 3 sent and 1 failed", detail.ID, detail.Status, detail.RecipientCounts)
	}
	if stringValue(detail.TemplateName) != "Sertifika" || stringValue(detail.TemplateKey) != "core.certificate" ||
		stringValue(detail.MailListName) != "GECEKODU katılımcıları" {
		t.Errorf("detail template/list = %s %s %s", stringValue(detail.TemplateName), stringValue(detail.TemplateKey), stringValue(detail.MailListName))
	}
	if a := detail.Audience; a.Kind != "mailing_list" || stringValue(a.Name) != "GECEKODU katılımcıları" || stringValue(a.Source) != "internal" {
		t.Errorf("detail audience = %+v", a)
	}

	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/mail_tasks/"+uuid.NewString(), nil))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusNotFound {
		t.Errorf("unknown send = %d, want 404", response.StatusCode)
	}
}

// countingKeycloakStub names the groups it knows and counts how often it is asked.
type countingKeycloakStub struct {
	groupKeycloakStub
	mu    *sync.Mutex
	calls *int
}

func (k countingKeycloakStub) GetGroup(ctx context.Context, id string) (*gocloak.Group, error) {
	k.mu.Lock()
	*k.calls++
	k.mu.Unlock()
	return k.groupKeycloakStub.GetGroup(ctx, id)
}

func TestSendAudienceIsTheSameOnEveryScreen(t *testing.T) {
	db := lifecycleHandlerStore(t)
	ctx := context.Background()
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	webLab, err := db.CreateMailingList(ctx, "WebLab")
	if err != nil {
		t.Fatal(err)
	}
	members := uuid.MustParse("5c0f3f6e-2d7a-4b43-9b8e-6a1d2f0c9e11")
	sends := []uuid.UUID{
		seedSend(t, db, seededSend{createdAt: now.Add(-4 * time.Hour), mailListID: &members, recipients: recipientsWith(database.MailQueueStatusSent, 2)}),
		seedSend(t, db, seededSend{createdAt: now.Add(-3 * time.Hour), mailListID: &members, recipients: recipientsWith(database.MailQueueStatusFailed, 1)}),
		seedSend(t, db, seededSend{createdAt: now.Add(-2 * time.Hour), mailListID: &webLab.ID, recipients: recipientsWith(database.MailQueueStatusSent, 1)}),
		seedSend(t, db, seededSend{createdAt: now.Add(-1 * time.Hour), recipients: recipientsWith(database.MailQueueStatusSent, 1)}),
	}
	calls := 0
	kc := countingKeycloakStub{groupKeycloakStub: groupKeycloakStub{names: map[string]string{members.String(): "Üyeler"}}, mu: &sync.Mutex{}, calls: &calls}
	app := sendSummaryApp(t, db, now, kc)

	var summary summaryResponse
	getJSON(t, app, "/mail_tasks/summary", &summary)
	fromSummary := map[uuid.UUID]summaryAudience{}
	for _, send := range summary.RecentSends {
		fromSummary[send.ID] = send.Audience
	}

	calls = 0
	var list []sendDetail
	getJSON(t, app, "/mail_tasks?_start=0&_end=10", &list)
	// Two sends to one group on a page ask Keycloak about it once.
	if calls != 1 {
		t.Errorf("the list asked Keycloak %d times for one group, want once", calls)
	}
	fromList := map[uuid.UUID]summaryAudience{}
	for _, send := range list {
		fromList[send.ID] = send.Audience
	}

	for _, id := range sends {
		var detail sendDetail
		getJSON(t, app, "/mail_tasks/"+id.String(), &detail)
		want := fmt.Sprintf("%+v", audienceText(fromSummary[id]))
		if got := fmt.Sprintf("%+v", audienceText(fromList[id])); got != want {
			t.Errorf("send %s: list audience %s, summary %s", id, got, want)
		}
		if got := fmt.Sprintf("%+v", audienceText(detail.Audience)); got != want {
			t.Errorf("send %s: detail audience %s, summary %s", id, got, want)
		}
	}
	if a := fromList[sends[0]]; stringValue(a.Name) != "Üyeler" || stringValue(a.Source) != "keycloak" {
		t.Errorf("group audience on the list = %+v, want Üyeler from Keycloak", audienceText(a))
	}
}

func audienceText(a summaryAudience) []string {
	return []string{a.Kind, stringValue(a.MailListID), stringValue(a.Name), stringValue(a.Source),
		stringValue(a.RecipientFullName), stringValue(a.RecipientEmail)}
}

func TestSendListDoesNotWaitOnAHungKeycloak(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	for i := 1; i <= 3; i++ {
		group := uuid.New()
		seedSend(t, db, seededSend{createdAt: now.Add(-time.Duration(i) * time.Hour), mailListID: &group,
			recipients: recipientsWith(database.MailQueueStatusSent, 1)})
	}
	app := sendSummaryApp(t, db, now, hangingKeycloakStub{})

	started := time.Now()
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, "/mail_tasks", nil),
		fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	var list []sendDetail
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != fiber.StatusOK || len(list) != 3 || response.Header.Get("X-Total-Count") != "3" {
		t.Fatalf("list = %d, %d rows, total %s", response.StatusCode, len(list), response.Header.Get("X-Total-Count"))
	}
	if elapsed > 4*time.Second {
		t.Fatalf("list took %s with Keycloak hung, want the group names given up within about 2s", elapsed)
	}
	for _, send := range list {
		if a := send.Audience; a.Name != nil || stringValue(a.Source) != "keycloak" {
			t.Errorf("audience = %+v, want the group with no name", audienceText(a))
		}
	}
}

// documentedFields reads the top-level fields /docs/openapi.json promises for a
// GET response: an object's properties, or an array item's.
func documentedFields(t *testing.T, path string) []string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal([]byte(docs.SwaggerInfo.ReadDoc()), &document); err != nil {
		t.Fatal(err)
	}
	schema := dig(t, document, "paths", path, "get", "responses", "200", "content", "application/json", "schema")
	if items, ok := schema["items"].(map[string]any); ok {
		schema = items
	}
	ref := strings.TrimPrefix(fmt.Sprint(schema["$ref"]), "#/components/schemas/")
	properties := dig(t, document, "components", "schemas", ref, "properties")
	fields := make([]string, 0, len(properties))
	for field := range properties {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func dig(t *testing.T, node map[string]any, keys ...string) map[string]any {
	t.Helper()
	for _, key := range keys {
		next, ok := node[key].(map[string]any)
		if !ok {
			t.Fatalf("OpenAPI document has no %q under %v", key, keys)
		}
		node = next
	}
	return node
}

func servedFields(t *testing.T, app *fiber.App, path string) []string {
	t.Helper()
	response, err := app.Test(httptest.NewRequest(fiber.MethodGet, path, nil))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	var object map[string]any
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil || len(rows) == 0 {
			t.Fatalf("GET %s: want rows, got %s", path, body)
		}
		object = rows[0]
	} else if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("GET %s: %s", path, body)
	}
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

func TestSendResponsesAreServedAsDocumented(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := istanbulTime(2026, time.September, 22, 12, 0, 0)
	id := seedSend(t, db, seededSend{createdAt: now.Add(-time.Hour), recipients: append(
		recipientsWith(database.MailQueueStatusSent, 1), recipientsWith(database.MailQueueStatusFailed, 1)...)})
	app := sendSummaryApp(t, db, now, lifecycleKeycloakStub{})

	for _, tc := range []struct{ documented, served string }{
		{"/mail_tasks", "/mail_tasks"},
		{"/mail_tasks/{id}", "/mail_tasks/" + id.String()},
		{"/mail_tasks/{id}/queue", "/mail_tasks/" + id.String() + "/queue"},
		{"/mail_tasks/summary", "/mail_tasks/summary"},
	} {
		documented, served := documentedFields(t, tc.documented), servedFields(t, app, tc.served)
		if fmt.Sprint(documented) != fmt.Sprint(served) {
			t.Errorf("GET %s documents %v but serves %v", tc.documented, documented, served)
		}
	}

	// A recipient's status is served as the object the old panel reads.
	rows, _ := queueRows(t, app, "/mail_tasks/"+id.String()+"/queue")
	for _, row := range rows {
		if !row.Status.Valid || (row.Status.MailQueueStatus != "sent" && row.Status.MailQueueStatus != "failed") {
			t.Errorf("queue row status = %+v, want {mail_queue_status, valid}", row.Status)
		}
	}
}
