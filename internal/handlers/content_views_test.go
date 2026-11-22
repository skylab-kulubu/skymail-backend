package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
)

// contentViewsAnswer is GET /mail_tasks/{id}/content-views as an audit
// reader got it.
type contentViewsAnswer struct {
	status int
	views  []SentContentView
	total  string
	cache  string
	body   string
}

func contentViewsAs(t *testing.T, app *fiber.App, task uuid.UUID, query string) contentViewsAnswer {
	t.Helper()
	request := httptest.NewRequest(fiber.MethodGet, "/mail_tasks/"+task.String()+"/content-views"+query, nil)
	request.Header.Set("X-Test-Sub", uuid.NewString())
	request.Header.Set("X-Test-Roles", strings.Join([]string{"skymail:access", AuditReadRole}, ","))
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	answer := contentViewsAnswer{
		status: response.StatusCode,
		total:  response.Header.Get("X-Total-Count"),
		cache:  response.Header.Get(fiber.HeaderCacheControl),
		body:   string(body),
	}
	if answer.status == fiber.StatusOK {
		if err := json.Unmarshal(body, &answer.views); err != nil {
			t.Fatal(err)
		}
	}
	return answer
}

// Who opened a masked mail (hub ticket skymail-frontend-rewrite 32): every
// opening, newest first, with the queue row and how it was masked; never the
// recipient. Paged with X-Total-Count; an erased account reads as Silinmiş
// kullanıcı.
func TestContentViewsListWhoOpenedAMaskedMailNewestFirst(t *testing.T) {
	db := lifecycleHandlerStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	app := contentApp(t, db, now)
	system := []string{"skymail:access", "skymail:mails:read", SystemMailsReadRole}
	task, row := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", "service-account-keycloak-mailer", now.Add(-time.Hour)))
	other, _ := seedRenderedSend(t, db, keycloakLike("keycloak.reset-password", "service-account-keycloak-mailer", now.Add(-time.Hour)))

	// None yet: an empty list, not an error.
	if got := contentViewsAs(t, app, task, ""); got.status != fiber.StatusOK || len(got.views) != 0 || got.total != "0" || got.body != "[]" {
		t.Fatalf("before any opening: %d total %q %s", got.status, got.total, got.body)
	}

	maskNow(t, db)
	path := "/mail_tasks/" + task.String() + "/queue/" + row.String() + "/content"
	first, second := uuid.NewString(), uuid.NewString()
	for _, opener := range []struct{ sub, name string }{{first, "Ece Yıldız"}, {second, "Mert Kaya"}, {second, "Mert Kaya"}} {
		if status, body, _ := getAsNamed(t, app, path, opener.sub, opener.name, system...); status != fiber.StatusOK {
			t.Fatalf("opening as %s: %d %s", opener.name, status, body)
		}
	}
	// Another send's opening is not this send's.
	if _, err := db.Conn.Exec(context.Background(), `
		INSERT INTO sent_content_views (task_id, queue_item_id, actor_sub, actor_name, masked_by)
		VALUES ($1, gen_random_uuid(), $2, 'Başka Biri', 'patterns')`, other, uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	got := contentViewsAs(t, app, task, "")
	if got.status != fiber.StatusOK || got.total != "3" || len(got.views) != 3 || got.cache != "no-store" {
		t.Fatalf("list = %d, total %q, Cache-Control %q: %s", got.status, got.total, got.cache, got.body)
	}
	views := got.views
	wantNames := []string{"Mert Kaya", "Mert Kaya", "Ece Yıldız"}
	for i, view := range views {
		if view.Actor.Name == nil || *view.Actor.Name != wantNames[i] {
			t.Errorf("view %d: actor %+v, want %s (newest first)", i, view.Actor, wantNames[i])
		}
		if view.QueueItemID != row || view.MaskedBy != "variables" || strings.Join(view.MaskedVariables, ",") != "code,link" {
			t.Errorf("view %d: row %s masked %s %v", i, view.QueueItemID, view.MaskedBy, view.MaskedVariables)
		}
		if i > 0 && view.CreatedAt.After(views[i-1].CreatedAt) {
			t.Errorf("view %d is newer than view %d", i, i-1)
		}
	}
	if views[2].Actor.Sub == nil || *views[2].Actor.Sub != first {
		t.Errorf("the oldest opening's sub = %v, want %s", views[2].Actor.Sub, first)
	}
	for _, recipient := range []string{"ayse@example.com", "Ayşe Kaya", "recipient"} {
		if strings.Contains(got.body, recipient) {
			t.Errorf("the list names the recipient (%q): %s", recipient, got.body)
		}
	}
	assertNoSecret(t, "the list", got.body, resetLink, loginCode)

	// Paged: the second newest alone, the total still all of them.
	page := contentViewsAs(t, app, task, "?_start=1&_end=2")
	if page.status != fiber.StatusOK || page.total != "3" || len(page.views) != 1 || *page.views[0].Actor.Name != "Mert Kaya" {
		t.Fatalf("the second one alone = %d total %q %s", page.status, page.total, page.body)
	}
	page = contentViewsAs(t, app, task, "?_start=2&_end=12")
	if page.status != fiber.StatusOK || page.total != "3" || len(page.views) != 1 || *page.views[0].Actor.Name != "Ece Yıldız" {
		t.Fatalf("the last page = %d total %q %s", page.status, page.total, page.body)
	}

	// Account erasure: the opener's sub and name are replaced.
	if _, err := db.EraseAccount(context.Background(), database.AccountErasure{
		RequestID: uuid.New(), Subject: first, Emails: []string{"ece@example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	views = contentViewsAs(t, app, task, "").views
	if len(views) != 3 {
		t.Fatalf("after erasure %d openings, want 3", len(views))
	}
	erased := views[2].Actor
	if erased.Name == nil || *erased.Name != database.DeletedUserName || erased.Sub == nil || *erased.Sub != database.DeletedUserSubject {
		t.Errorf("after erasure the opener reads %+v, want %s", erased, database.DeletedUserName)
	}
	if *views[0].Actor.Name != "Mert Kaya" {
		t.Errorf("erasure changed another opener: %+v", views[0].Actor)
	}

	// An unknown send is 404.
	if got := contentViewsAs(t, app, uuid.New(), ""); got.status != fiber.StatusNotFound {
		t.Errorf("unknown send = %d, want 404", got.status)
	}
}
