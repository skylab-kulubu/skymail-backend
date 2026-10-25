package news

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func cmsServer(t *testing.T, status int, body string) *CMS {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/cms/collections/news/yeni-donem" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return NewCMS(server.URL+"/", nil)
}

// A record from before the schema's second version reads with what its
// absent fields mean: a public haber; a v2 record keeps its fields. The read
// is anonymous.
func TestCMSReadsARecordWithItsDefaults(t *testing.T) {
	old := cmsServer(t, http.StatusOK, `{"id":"0c0c0c0c-0000-4000-8000-000000000001","collectionKey":"news","slug":"yeni-donem","version":4,
		"updatedAt":"2026-10-20T15:00:00Z","data":{"title":" Yeni dönem ","summary":"Özet","body":"Satır 1\nSatır 2\n\nİkinci","heroImage":"https://cdn.example/a.png","tags":["x"],"author":"YK","featured":true}}`)
	record, err := old.Get(context.Background(), "yeni-donem")
	if err != nil {
		t.Fatal(err)
	}
	if record.Slug != "yeni-donem" || record.Version != 4 || record.Title != "Yeni dönem" || record.Kind != KindNews || record.Audience != AudiencePublic {
		t.Fatalf("record = %+v", record)
	}
	if record.UpdatedAt.Format("2006-01-02T15:04:05Z") != "2026-10-20T15:00:00Z" {
		t.Fatalf("updatedAt = %v", record.UpdatedAt)
	}

	v2 := cmsServer(t, http.StatusOK, `{"slug":"yeni-donem","version":5,"updatedAt":"2026-10-21T10:00:00Z","data":{"title":"Kayıtlar açıldı","body":"<p>x</p>",
		"kind":"duyuru","audience":"herkes","heroAlt":"Afiş","cta":{"href":"https://forms.example/f","label":"Başvur"}}}`)
	record, err = v2.Get(context.Background(), "yeni-donem")
	if err != nil {
		t.Fatal(err)
	}
	if record.Kind != KindAnnouncement || record.HeroAlt != "Afiş" || record.Cta == nil || record.Cta.Label != "Başvur" {
		t.Fatalf("record = %+v", record)
	}
}

// An unknown or archived record (inscribed hides it from an anonymous read)
// is ErrNotFound; any other answer is an error, never a record.
func TestCMSRefusesWhatIsNoPublishedRecord(t *testing.T) {
	if _, err := cmsServer(t, http.StatusNotFound, ``).Get(context.Background(), "yeni-donem"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404: %v, want ErrNotFound", err)
	}
	for name, server := range map[string]*CMS{
		"5xx":          cmsServer(t, http.StatusServiceUnavailable, `{}`),
		"not a record": cmsServer(t, http.StatusOK, `[]`),
		"no version":   cmsServer(t, http.StatusOK, `{"slug":"yeni-donem","data":{"title":"t","body":"b"}}`),
		"no body":      cmsServer(t, http.StatusOK, `{"slug":"yeni-donem","version":1,"data":{"title":"t"}}`),
		"bad kind":     cmsServer(t, http.StatusOK, `{"slug":"yeni-donem","version":1,"data":{"title":"t","body":"b","kind":"reklam"}}`),
	} {
		if _, err := server.Get(context.Background(), "yeni-donem"); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want an error other than ErrNotFound", name, err)
		}
	}
}

type token string

func (t token) ServiceToken(context.Context) (string, error) { return string(t), nil }

// The audience is read page by page with SkyMail's service token, and the
// numbers are the last page's (each page counts the whole audience).
func TestCoreReadsTheWholeAudience(t *testing.T) {
	var asked []AudienceRequest
	var afters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ResolvePath || r.Header.Get("Authorization") != "Bearer servis" || r.URL.Query().Get("limit") != "500" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body AudienceRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		asked = append(asked, body)
		afters = append(afters, r.URL.Query().Get("after"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after") == "" {
			_, _ = io.WriteString(w, `{"items":[{"userId":"u1","email":"a@example.org","name":"A","categories":["club:duyuru"],"via":"member","reason":"SKY LAB üyesi olduğun için","categoryLabel":"Kulüp geneli · Duyurular","unsubscribeUrl":"https://core.example/u?t=1"}],
				"next":"u1","counts":{"members":2,"followers":0,"total":2},"skipped":{"optedOut":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"userId":"u2","email":"b@example.org","name":"B","categories":["club:duyuru"],"via":"member","reason":"r","categoryLabel":"c","unsubscribeUrl":"https://core.example/u?t=2"}],
			"next":null,"counts":{"members":2,"followers":0,"total":2},"skipped":{"optedOut":1,"unverified":1}}`)
	}))
	t.Cleanup(server.Close)

	audience, err := NewCore(server.URL, token("servis"), nil).Resolve(context.Background(),
		AudienceRequest{Type: AudienceTypeMembers, NewsKind: KindAnnouncement, Visibility: AudiencePublic})
	if err != nil {
		t.Fatal(err)
	}
	if len(audience.People) != 2 || audience.People[1].UnsubscribeURL != "https://core.example/u?t=2" {
		t.Fatalf("people = %+v", audience.People)
	}
	if audience.Counts.Total != 2 || audience.Skipped.Unverified != 1 || audience.Skipped.OptedOut != 1 {
		t.Fatalf("counts = %+v %+v", audience.Counts, audience.Skipped)
	}
	if len(asked) != 2 || afters[1] != "u1" || asked[0].NewsKind != "duyuru" || asked[0].Visibility != "herkes" || asked[0].Teams != nil {
		t.Fatalf("asked = %+v after %v", asked, afters)
	}
}

// core's refusals: an unknown team and a record core may not send yet are
// their own errors; anything else is core not answering.
func TestCoreNamesItsRefusals(t *testing.T) {
	answer := func(status int, body string) error {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		defer server.Close()
		_, err := NewCore(server.URL, token("servis"), nil).Resolve(context.Background(), AudienceRequest{Type: AudienceTypeTeams, Teams: []string{"/UYELER/X"}})
		return err
	}
	if err := answer(http.StatusBadRequest, `{"code":"audience_team_unknown"}`); !errors.Is(err, ErrTeamUnknown) {
		t.Errorf("team unknown: %v", err)
	}
	if err := answer(http.StatusBadRequest, `{"code":"audience_visibility_not_enabled"}`); !errors.Is(err, ErrVisibilityNotEnabled) {
		t.Errorf("visibility: %v", err)
	}
	for _, body := range []string{`{"code":"audience_directory_unavailable"}`, ``} {
		if err := answer(http.StatusServiceUnavailable, body); err == nil || errors.Is(err, ErrTeamUnknown) {
			t.Errorf("503 %q: %v", body, err)
		}
	}
	if err := answer(http.StatusOK, `{"items":null}`); err == nil {
		t.Error("an answer without items was taken")
	}
}

// A body of plain text from before the schema's second version reads as
// paragraphs, escaped; markup stays as it is (the mailer narrows it).
func TestBodyHTMLTurnsPlainTextIntoParagraphs(t *testing.T) {
	if got, want := BodyHTML("Satır 1\r\nSatır <2> & 3\n\n\nİkinci"), "<p>Satır 1<br>Satır &lt;2&gt; &amp; 3</p><p>İkinci</p>"; got != want {
		t.Errorf("BodyHTML = %q, want %q", got, want)
	}
	if got := BodyHTML(" <p>Merhaba</p> "); got != "<p>Merhaba</p>" {
		t.Errorf("BodyHTML(markup) = %q", got)
	}
}

// The template's variables: the subject's default, the read-more link, a
// summary send without the body, an image and a button only as web links.
func TestVariablesOfARecord(t *testing.T) {
	record := Record{Slug: "kayitlar", Title: "Kayıtlar açıldı", Summary: "Özet", Body: "Gövde", Kind: KindAnnouncement,
		HeroImage: "javascript:alert(1)", Cta: &Link{Href: "https://forms.example/f"}}
	vars := Variables(record, FormatSummary, "", "", ReadMoreHome)
	if vars["Subject"] != "Duyuru: Kayıtlar açıldı" || vars["Preheader"] != "Özet" || vars["BodyHtml"] != "" || vars["FullBody"] != false {
		t.Fatalf("vars = %v", vars)
	}
	if vars["ReadMoreUrl"] != "https://yildizskylab.com/" || vars["HeroUrl"] != "" || vars["CtaHref"] != "https://forms.example/f" || vars["CtaLabel"] != "Bağlantıyı aç" {
		t.Fatalf("vars = %v", vars)
	}
	vars = Variables(record, FormatFull, " Kendi konum ", "Önizleme", ReadMorePages)
	if vars["Subject"] != "Kendi konum" || vars["Preheader"] != "Önizleme" || vars["BodyHtml"] != "<p>Gövde</p>" || vars["KindLabel"] != "Duyuru" {
		t.Fatalf("vars = %v", vars)
	}
	if vars["ReadMoreUrl"] != "https://yildizskylab.com/duyurular/kayitlar" {
		t.Fatalf("read more = %v", vars["ReadMoreUrl"])
	}
	news := Variables(Record{Slug: "a", Title: "Haber", Body: "b", Kind: KindNews}, FormatFull, "", "", ReadMorePages)
	if news["Subject"] != "Haber" || !strings.HasSuffix(news["ReadMoreUrl"].(string), "/haberler/a") {
		t.Fatalf("news vars = %v", news)
	}
}

// The same audience in another order, or a team named twice, is one key.
func TestAudienceKey(t *testing.T) {
	if got := AudienceKey(AudienceTypeMembers, []string{"x"}); got != "members" {
		t.Errorf("members = %q", got)
	}
	a := AudienceKey(AudienceTypeTeams, []string{"/UYELER/B", " /UYELER/A", "/UYELER/B"})
	b := AudienceKey(AudienceTypeTeams, []string{"/UYELER/A", "/UYELER/B"})
	if a != b || a != "teams:/UYELER/A,/UYELER/B" {
		t.Errorf("keys %q %q", a, b)
	}
}
