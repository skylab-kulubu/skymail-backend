package handlers

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Nerzal/gocloak/v13"
	"github.com/gofiber/fiber/v3"
	"github.com/skylab-kulubu/skynotify-backend/internal/apperrors"
	"github.com/skylab-kulubu/skynotify-backend/internal/database"
)

func TestSearchInstantTakesDaysInIstanbulAndTimes(t *testing.T) {
	zone := summaryLocation()
	for _, tc := range []struct {
		raw  string
		end  bool
		want string
	}{
		{"2026-11-10", false, "2026-11-09T21:00:00Z"},
		{"2026-11-10", true, "2026-11-10T21:00:00Z"}, // the whole day
		{"2026-11-10T10:00:00+03:00", false, "2026-11-10T07:00:00Z"},
		{"2026-11-10T10:00:00+03:00", true, "2026-11-10T07:00:00.000001Z"},
		{"2026-11-10T10:00:00.0000009Z", true, "2026-11-10T10:00:00.000001Z"},
		{"2026-11-10T10:00:00 03:00", false, "2026-11-10T07:00:00Z"}, // an unencoded +
	} {
		got, ok := parseSearchInstant(tc.raw, zone, tc.end)
		if !ok || got.UTC().Format(time.RFC3339Nano) != tc.want {
			t.Errorf("%s (end %v) = %s %v; want %s", tc.raw, tc.end, got.UTC().Format(time.RFC3339Nano), ok, tc.want)
		}
	}
	for _, raw := range []string{"yesterday", "2026-13-01", "10.11.2026", "2026-11-10 10:00"} {
		if _, ok := parseSearchInstant(raw, zone, false); ok {
			t.Errorf("%q parsed", raw)
		}
	}
}

func TestSearchFoldReadsTurkishCapitals(t *testing.T) {
	for _, tc := range []struct{ text, query string }{
		{"Giriş kodu", "GİRİŞ"},
		{"Işık Atölyesi", "ışık"},
		{"ışık", "IŞIK"},
		{"Tüm üyeler", "TÜM ÜYELER"},
		{"ÇAĞRI", "çağrı"},
		{"Öğrenci", "ÖĞRENCİ"},
	} {
		if !strings.Contains(searchFold(tc.text), searchFold(tc.query)) {
			t.Errorf("%q does not find %q (%q, %q)", tc.query, tc.text, searchFold(tc.query), searchFold(tc.text))
		}
	}
}

func TestSearchParam(t *testing.T) {
	if q, ok := searchParam("   "); q != nil || !ok {
		t.Errorf("blank: %v %v", q, ok)
	}
	if q, ok := searchParam("  Ayşe "); q == nil || *q != "Ayşe" || !ok {
		t.Errorf("trimmed: %v %v", q, ok)
	}
	// Characters, not bytes: 200 Turkish letters are fine.
	if _, ok := searchParam(strings.Repeat("ş", 200)); !ok {
		t.Error("200 characters refused")
	}
	for _, bad := range []string{strings.Repeat("a", 201), "a\x00b", "\xff\xfe", "ab\xc3"} {
		if _, ok := searchParam(bad); ok {
			t.Errorf("%q taken", bad)
		}
	}
}

func TestGroupsMatching(t *testing.T) {
	name := func(s string) *string { return &s }
	groups := []*gocloak.Group{
		{Name: name("Yapay Zeka"), Path: name("/Ekipler/Yapay Zeka")},
		{Name: name("Yönetim Kurulu"), Path: name("/Yönetim Kurulu")},
		{Name: nil, Path: nil},
	}
	if got := groupsMatching(groups, nil); len(got) != 3 {
		t.Errorf("no search: %d", len(got))
	}
	q := "ekipler"
	if got := groupsMatching(groups, &q); len(got) != 1 || *got[0].Name != "Yapay Zeka" {
		t.Errorf("by path: %v", got)
	}
	q = "YÖNETİM"
	if got := groupsMatching(groups, &q); len(got) != 1 || *got[0].Name != "Yönetim Kurulu" {
		t.Errorf("Turkish capitals: %v", got)
	}
	q = "KURUL"
	if got := groupsMatching(groups, &q); len(got) != 1 || *got[0].Name != "Yönetim Kurulu" {
		t.Errorf("by name, any case: %v", got)
	}
}

func TestRecipientListFilterTakesSkipped(t *testing.T) {
	status, skipped, err := parseRecipientListFilter(" Skipped ")
	if err != nil || !skipped || !status.Valid || status.MailQueueStatus != database.MailQueueStatusFailed {
		t.Errorf("skipped: %+v %v %v", status, skipped, err)
	}
	status, skipped, err = parseRecipientListFilter("failed")
	if err != nil || skipped || status.MailQueueStatus != database.MailQueueStatusFailed {
		t.Errorf("failed: %+v %v %v", status, skipped, err)
	}
	if _, _, err := parseRecipientListFilter("bounced"); err == nil || !strings.Contains(errorParams(err), "skipped") {
		t.Errorf("unknown: %v", err)
	}
}

func TestSendListQuery(t *testing.T) {
	read := func(target string, roles ...string) (sendListQuery, error) {
		t.Helper()
		var query sendListQuery
		var parseErr error
		app := fiber.New()
		app.Get("/", func(c fiber.Ctx) error {
			c.Locals("roles", roles)
			query, parseErr = parseSendListQuery(c)
			return nil
		})
		if _, err := app.Test(httptest.NewRequest("GET", target, nil)); err != nil {
			t.Fatal(err)
		}
		return query, parseErr
	}

	query, err := read("/?q=+bahar+&kind=SYSTEM&template_id=61000000-0000-4000-8000-000000000001&sent_by=abc&from=2026-11-01&to=2026-11-02&_order=asc&status=sent")
	if err != nil {
		t.Fatal(err)
	}
	f := query.filter
	if *f.Search != "bahar" || *f.Kind != SendKindSystem || f.TemplateID.String() != "61000000-0000-4000-8000-000000000001" ||
		*f.SentBy != "abc" || *f.Status != "sent" || f.ListID != nil || !query.ascending || query.sort != "" ||
		f.CreatedBefore.Sub(*f.CreatedFrom) != 48*time.Hour {
		t.Errorf("parsed %+v", query)
	}
	list := query.list(10, 20)
	if list.Limit != 10 || list.Offset != 20 || list.Search != f.Search || list.Kind != f.Kind || list.TaskID != nil {
		t.Errorf("list params %+v", list)
	}
	sorted := query.sorted(10, 20)
	if sorted.Limit != 10 || sorted.Offset != 20 || sorted.Search != f.Search || !sorted.Ascending || sorted.Sort != "" || sorted.SentBy != f.SentBy {
		t.Errorf("sorted params %+v", sorted)
	}

	// Every invalid value is named at once.
	_, err = read("/?kind=x&from=y&list_id=z&_sort=w&_order=v")
	var appErr *apperrors.AppError
	if !errors.As(err, &appErr) || appErr.Code != "validation.error" {
		t.Fatalf("invalid: %v", err)
	}
	for _, name := range []string{"kind", "from", "list_id", "_sort", "_order"} {
		if _, ok := appErr.Params[name]; !ok {
			t.Errorf("%s not named: %v", name, appErr.Params)
		}
	}
	if _, err := read("/?from=2026-11-02&to=2026-11-01"); err == nil {
		t.Error("to before from taken")
	}

	// open_rate needs the tracking role.
	if _, err := read("/?_sort=open_rate", "skynotify:mails:read"); !errors.Is(err, errOpenRateSortForbidden) {
		t.Errorf("open_rate without the role: %v", err)
	}
	if query, err := read("/?_sort=open_rate", "skynotify:mails:read", TrackingReadRole); err != nil || query.sort != sendSortOpenRate {
		t.Errorf("open_rate with the role: %+v %v", query, err)
	}
	// A validation error comes before the role's refusal.
	if _, err := read("/?_sort=open_rate&kind=x", "skynotify:mails:read"); errors.Is(err, errOpenRateSortForbidden) {
		t.Error("forbidden before validation")
	}
}

func errorParams(err error) string {
	var appErr *apperrors.AppError
	if !errors.As(err, &appErr) {
		return ""
	}
	var out strings.Builder
	for k, v := range appErr.Params {
		out.WriteString(k)
		out.WriteString("=")
		if s, ok := v.(string); ok {
			out.WriteString(s)
		}
		out.WriteString(";")
	}
	return out.String()
}
