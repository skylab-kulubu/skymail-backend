package consent_test

import (
	"context"
	"encoding/json"
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
