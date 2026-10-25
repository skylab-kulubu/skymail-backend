package consent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skymail-backend/internal/consent"
)

type token string

func (t token) ServiceToken(context.Context) (string, error) { return string(t), nil }

// The audience is read page by page with the service token, until next is
// null; addresses come back normalized.
func TestAudienceReadsEveryPage(t *testing.T) {
	var asked []string
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != consent.AudiencePath || r.Header.Get("Authorization") != "Bearer svc" {
			t.Errorf("asked %s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		asked = append(asked, r.URL.RawQuery)
		if r.URL.Query().Get("after") == "" {
			fmt.Fprint(w, `{"items":[{"id":"1","email":" Ayse@Example.com ","subject":"address","textVersion":"tanitim-v1","withdrawUrl":"https://api.example.org/v1/consents/withdraw?token=a","renewalDue":false}],"next":"1"}`)
			return
		}
		fmt.Fprint(w, `{"items":[{"id":"2","email":"deniz@example.com","subject":"account","textVersion":"tanitim-v1","withdrawUrl":"https://api.example.org/v1/consents/withdraw?token=d","renewalDue":true}],"next":null}`)
	}))
	defer core.Close()

	items, err := consent.NewClient(core.URL+"/", token("svc"), nil).Audience(context.Background(), consent.PurposePromotional)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Email != "ayse@example.com" || items[0].WithdrawURL != "https://api.example.org/v1/consents/withdraw?token=a" ||
		items[1].Email != "deniz@example.com" || !items[1].RenewalDue {
		t.Errorf("items %+v", items)
	}
	if len(asked) != 2 || asked[0] != "limit=1000&purpose=promotional" || asked[1] != "after=1&limit=1000&purpose=promotional" {
		t.Errorf("asked %v", asked)
	}
}

// Anything off the contract, or core not answering 200, is no audience.
func TestAudienceRefusesWhatIsNotTheContract(t *testing.T) {
	for name, answer := range map[string]struct {
		status int
		body   string
	}{
		"503 without CONTACT_CONSENT_KEY": {http.StatusServiceUnavailable, `{"code":"consents_unavailable"}`},
		"no items":                        {http.StatusOK, `{"next":null}`},
		"an item without an address":      {http.StatusOK, `{"items":[{"email":" "}],"next":null}`},
		"not JSON":                        {http.StatusOK, `<html>`},
		"next never moves":                {http.StatusOK, `{"items":[],"next":""}`},
	} {
		t.Run(name, func(t *testing.T) {
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(answer.status)
				fmt.Fprint(w, answer.body)
			}))
			defer core.Close()
			if items, err := consent.NewClient(core.URL, token("svc"), nil).Audience(context.Background(), consent.PurposePromotional); err == nil {
				t.Errorf("items %+v, want an error", items)
			}
		})
	}
}

// recipients:resolve is asked once per address, normalized, with the send's
// category; the answer is read in the order asked.
func TestMatchedAsksEachAddressOnce(t *testing.T) {
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != consent.ResolvePath {
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Channel    string `json:"channel"`
			Category   string `json:"category"`
			Recipients []map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Channel != "email" || body.Category != "club:duyuru" || len(body.Recipients) != 2 ||
			body.Recipients[0]["email"] != "uye@example.com" || body.Recipients[1]["email"] != "misafir@example.com" {
			t.Errorf("body %+v", body)
		}
		fmt.Fprint(w, `{"results":[{"matched":true,"sub":"7d3c1f6e-0000-4000-8000-000000000001","allowed":true,"reason":null},{"matched":false}]}`)
	}))
	defer core.Close()

	matched, err := consent.NewClient(core.URL, token("svc"), nil).Matched(context.Background(), "club:duyuru",
		[]string{"Uye@Example.com", "misafir@example.com", " uye@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 2 || !matched["uye@example.com"] || matched["misafir@example.com"] {
		t.Errorf("matched %v", matched)
	}
}

// An answer about fewer recipients than asked, or a refusal, is no answer.
func TestMatchedRefusesAShortAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"short":   `{"results":[{"matched":false}]}`,
		"missing": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, body)
			}))
			defer core.Close()
			if _, err := consent.NewClient(core.URL, token("svc"), nil).Matched(context.Background(), "club:duyuru",
				[]string{"a@example.com", "b@example.com"}); err == nil || strings.Contains(err.Error(), "@") {
				t.Errorf("err %v, want an error naming no address", err)
			}
		})
	}
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer core.Close()
	if _, err := consent.NewClient(core.URL, token("svc"), nil).Matched(context.Background(), "club:duyuru", []string{"a@example.com"}); err == nil {
		t.Error("a 503 is an answer")
	}
}

// More than ResolveBatch addresses go in batches of at most ResolveBatch,
// each answered in the order asked.
func TestMatchedAsksInBatches(t *testing.T) {
	var sizes []int
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Recipients []map[string]string `json:"recipients"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(body.Recipients))
		results := make([]string, len(body.Recipients))
		for i, recipient := range body.Recipients {
			results[i] = `{"matched":false}`
			if strings.HasPrefix(recipient["email"], "uye") {
				results[i] = `{"matched":true,"sub":"7d3c1f6e-0000-4000-8000-000000000001","allowed":true,"reason":null}`
			}
		}
		fmt.Fprint(w, `{"results":[`+strings.Join(results, ",")+`]}`)
	}))
	defer core.Close()

	addresses := make([]string, 0, consent.ResolveBatch+1)
	for i := 0; i < consent.ResolveBatch; i++ {
		addresses = append(addresses, fmt.Sprintf("misafir%d@example.com", i))
	}
	addresses = append(addresses, "uye@example.com")
	matched, err := consent.NewClient(core.URL, token("svc"), nil).Matched(context.Background(), "club:duyuru", addresses)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 2 || sizes[0] != consent.ResolveBatch || sizes[1] != 1 {
		t.Errorf("batches %v, want %d and 1", sizes, consent.ResolveBatch)
	}
	if len(matched) != consent.ResolveBatch+1 || !matched["uye@example.com"] || matched["misafir0@example.com"] {
		t.Errorf("matched %d addresses, uye %v", len(matched), matched["uye@example.com"])
	}
}

// Resolve sends each recipient by sub or by address, in one call of at most
// ResolveBatch, and reads every field of the answer in the order asked
// (abonelik-cikisi 09).
func TestResolveReadsTheWholeAnswer(t *testing.T) {
	var body map[string]any
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != consent.ResolvePath || r.Header.Get("Authorization") != "Bearer svc" {
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, `{"results":[
			{"matched":true,"sub":"7d3c1f6e-0000-4000-8000-000000000001","allowed":true,"reason":null,"unsubscribeUrl":"https://api.example.org/v1/communication-preferences/unsubscribe?t=a"},
			{"matched":false},
			{"matched":true,"sub":"7d3c1f6e-0000-4000-8000-000000000002","allowed":false,"reason":"opted_out"},
			{"matched":true,"allowed":false,"reason":"ambiguous"}]}`)
	}))
	defer core.Close()

	results, err := consent.NewClient(core.URL, token("svc"), nil).Resolve(context.Background(), "team:2b0f8a4e-0f4c-4a43-9b0b-1d1e2f3a4b5c:duyuru", []consent.Recipient{
		{Sub: "7d3c1f6e-0000-4000-8000-000000000001"}, {Email: "misafir@example.com"}, {Email: "uye@example.com"}, {Email: "iki@example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["channel"] != "email" || body["category"] != "team:2b0f8a4e-0f4c-4a43-9b0b-1d1e2f3a4b5c:duyuru" {
		t.Errorf("asked %v", body)
	}
	if asked, _ := json.Marshal(body["recipients"]); string(asked) != `[{"sub":"7d3c1f6e-0000-4000-8000-000000000001"},{"email":"misafir@example.com"},{"email":"uye@example.com"},{"email":"iki@example.com"}]` {
		t.Errorf("recipients %s", asked)
	}
	want := []consent.Result{
		{Matched: true, Sub: "7d3c1f6e-0000-4000-8000-000000000001", Allowed: true, UnsubscribeURL: "https://api.example.org/v1/communication-preferences/unsubscribe?t=a"},
		{},
		{Matched: true, Sub: "7d3c1f6e-0000-4000-8000-000000000002", Reason: "opted_out"},
		{Matched: true, Reason: "ambiguous"},
	}
	if len(results) != len(want) {
		t.Fatalf("results %+v", results)
	}
	for i := range want {
		if results[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, results[i], want[i])
		}
	}
}

// core's 400 recipients_resolve_category_unknown is ErrCategoryUnknown;
// every other refusal, a short answer, a reason off the contract or an
// allowed answer without its link is no answer at all.
func TestResolveRefusesWhatIsNotTheContract(t *testing.T) {
	for name, answer := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"unknown team":       {http.StatusBadRequest, `{"code":"recipients_resolve_category_unknown"}`, consent.ErrCategoryUnknown},
		"invalid request":    {http.StatusBadRequest, `{"code":"recipients_resolve_invalid"}`, nil},
		"directory down":     {http.StatusServiceUnavailable, `{"code":"recipients_resolve_directory_unavailable"}`, nil},
		"short":              {http.StatusOK, `{"results":[]}`, nil},
		"unknown reason":     {http.StatusOK, `{"results":[{"matched":true,"allowed":false,"reason":"maybe"}]}`, nil},
		"allowed, no link":   {http.StatusOK, `{"results":[{"matched":true,"allowed":true}]}`, nil},
		"not allowed, empty": {http.StatusOK, `{"results":[{"matched":true,"allowed":false}]}`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(answer.status)
				fmt.Fprint(w, answer.body)
			}))
			defer core.Close()
			results, err := consent.NewClient(core.URL, token("svc"), nil).Resolve(context.Background(), "club:duyuru", []consent.Recipient{{Email: "a@example.com"}})
			switch {
			case err == nil:
				t.Fatalf("results %+v, want an error", results)
			case answer.want != nil && !errors.Is(err, answer.want):
				t.Errorf("err %v, want %v", err, answer.want)
			case answer.want == nil && errors.Is(err, consent.ErrCategoryUnknown):
				t.Errorf("err %v is not an unknown category", err)
			}
		})
	}
}
