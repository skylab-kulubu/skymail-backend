package tracking_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/tracking"
)

type staticToken struct {
	token string
	err   error
}

func (s staticToken) ServiceToken(context.Context) (string, error) { return s.token, s.err }

// fakeCore is core's tracking:resolve as ticket 16 describes it: the
// people it is asked about, each allowed or not with why.
type fakeCore struct {
	mu       sync.Mutex
	requests []resolveRequest
	answer   func(subs []string) (int, any)
}

type resolveRequest struct {
	method, path, auth, contentType string
	subs                            []string
	raw                             string
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed struct {
		Subs []string `json:"subs"`
	}
	_ = json.Unmarshal(body, &parsed)
	f.mu.Lock()
	f.requests = append(f.requests, resolveRequest{
		method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"), subs: parsed.Subs, raw: string(body),
	})
	f.mu.Unlock()
	status, answer := f.answer(parsed.Subs)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(answer)
}

func (f *fakeCore) seen() []resolveRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]resolveRequest(nil), f.requests...)
}

type result struct {
	Sub     string  `json:"sub"`
	Allowed bool    `json:"allowed"`
	Reason  *string `json:"reason"`
}

// answerEach answers every person asked about with decide.
func answerEach(decide func(sub string) result) func([]string) (int, any) {
	return func(subs []string) (int, any) {
		results := []result{}
		for _, sub := range subs {
			results = append(results, decide(sub))
		}
		return http.StatusOK, map[string]any{"results": results}
	}
}

func startCore(t *testing.T, answer func([]string) (int, any)) (*fakeCore, *httptest.Server) {
	t.Helper()
	core := &fakeCore{answer: answer}
	server := httptest.NewServer(core)
	t.Cleanup(server.Close)
	return core, server
}

func reason(s string) *string { return &s }

// The contract with core (ticket 16): POST
// /internal/v1/communication-preferences/tracking:resolve with SkyMail's
// service token and {"subs": [...]}, each person once; the answer has each
// of them once, allowed or not with why.
func TestCoreClientAsksCoreAboutEachPersonOnce(t *testing.T) {
	ayse, mehmet, zeynep := uuid.New(), uuid.New(), uuid.New()
	core, server := startCore(t, answerEach(func(sub string) result {
		switch sub {
		case ayse.String():
			return result{Sub: sub, Allowed: true}
		case mehmet.String():
			return result{Sub: sub, Allowed: false, Reason: reason("preference_off")}
		default:
			return result{Sub: sub, Allowed: false, Reason: reason("not_member")}
		}
	}))
	client := tracking.NewCoreClient(server.URL+"/", staticToken{token: "service-token"}, nil)

	got, err := client.Resolve(context.Background(), []uuid.UUID{ayse, mehmet, zeynep, ayse})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]tracking.Decision{
		ayse:   {Allowed: true},
		mehmet: {Allowed: false, Reason: "preference_off"},
		zeynep: {Allowed: false, Reason: "not_member"},
	}
	if len(got) != len(want) {
		t.Fatalf("decisions = %+v, want %+v", got, want)
	}
	for sub, decision := range want {
		if got[sub] != decision {
			t.Errorf("%s: %+v, want %+v", sub, got[sub], decision)
		}
	}

	requests := core.seen()
	if len(requests) != 1 {
		t.Fatalf("%d requests, want 1", len(requests))
	}
	r := requests[0]
	if r.method != http.MethodPost || r.path != "/internal/v1/communication-preferences/tracking:resolve" ||
		r.auth != "Bearer service-token" || !strings.HasPrefix(r.contentType, "application/json") {
		t.Errorf("request = %+v", r)
	}
	if len(r.subs) != 3 {
		t.Errorf("asked about %v, want each person once", r.subs)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.raw), &fields); err != nil || len(fields) != 1 {
		t.Errorf("body %s: only subs is sent", r.raw)
	}
}

// Core takes at most 1000 people a call: more are asked in batches.
func TestCoreClientAsksInBatchesOf1000(t *testing.T) {
	core, server := startCore(t, answerEach(func(sub string) result { return result{Sub: sub, Allowed: true} }))
	client := tracking.NewCoreClient(server.URL, staticToken{token: "t"}, nil)
	subs := make([]uuid.UUID, 2345)
	for i := range subs {
		subs[i] = uuid.New()
	}
	got, err := client.Resolve(context.Background(), subs)
	if err != nil || len(got) != len(subs) {
		t.Fatalf("%d decisions, %v", len(got), err)
	}
	var sizes []int
	for _, r := range core.seen() {
		sizes = append(sizes, len(r.subs))
	}
	if fmt.Sprint(sizes) != "[1000 1000 345]" {
		t.Fatalf("batches %v, want [1000 1000 345]", sizes)
	}
	if got, err := client.Resolve(context.Background(), nil); err != nil || len(got) != 0 || len(core.seen()) != 3 {
		t.Fatalf("nobody to ask: %v, %v, %d requests", got, err, len(core.seen()))
	}
}

// Anything but a whole answer is no answer: the caller tracks no one. The
// error never names a person.
func TestCoreClientTakesOnlyAWholeAnswer(t *testing.T) {
	ayse, mehmet := uuid.New(), uuid.New()
	cases := map[string]func([]string) (int, any){
		"503": func([]string) (int, any) { return http.StatusServiceUnavailable, map[string]any{} },
		"404 (core without ticket 16)": func([]string) (int, any) {
			return http.StatusNotFound, map[string]any{"code": "not_found"}
		},
		"someone missing": func(subs []string) (int, any) {
			return http.StatusOK, map[string]any{"results": []result{{Sub: subs[0], Allowed: true}}}
		},
		"someone twice": func(subs []string) (int, any) {
			return http.StatusOK, map[string]any{"results": []result{
				{Sub: subs[0], Allowed: true}, {Sub: subs[0], Allowed: true}, {Sub: subs[1], Allowed: true}}}
		},
		"someone else instead": func(subs []string) (int, any) {
			return http.StatusOK, map[string]any{"results": []result{
				{Sub: subs[0], Allowed: true}, {Sub: uuid.NewString(), Allowed: true}}}
		},
		"someone not asked about": func(subs []string) (int, any) {
			return http.StatusOK, map[string]any{"results": []result{
				{Sub: subs[0], Allowed: true}, {Sub: subs[1], Allowed: true}, {Sub: uuid.NewString(), Allowed: true}}}
		},
		"not a subject": func(subs []string) (int, any) {
			return http.StatusOK, map[string]any{"results": []result{{Sub: "ayse", Allowed: true}, {Sub: subs[1], Allowed: true}}}
		},
		"no results": func([]string) (int, any) { return http.StatusOK, map[string]any{} },
		"not json":   func([]string) (int, any) { return http.StatusOK, "merhaba" },
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			_, server := startCore(t, answer)
			client := tracking.NewCoreClient(server.URL, staticToken{token: "t"}, nil)
			got, err := client.Resolve(context.Background(), []uuid.UUID{ayse, mehmet})
			if err == nil {
				t.Fatalf("Resolve = %+v, want an error", got)
			}
			if strings.Contains(err.Error(), ayse.String()) || strings.Contains(err.Error(), mehmet.String()) {
				t.Fatalf("the error names a person: %v", err)
			}
		})
	}
}

// No token, no call; a core that does not answer in time is no answer.
func TestCoreClientWithoutATokenOrAnAnswer(t *testing.T) {
	core, server := startCore(t, answerEach(func(sub string) result { return result{Sub: sub, Allowed: true} }))
	client := tracking.NewCoreClient(server.URL, staticToken{err: errors.New("keycloak down")}, nil)
	if _, err := client.Resolve(context.Background(), []uuid.UUID{uuid.New()}); err == nil || len(core.seen()) != 0 {
		t.Fatalf("without a token: %v, %d requests", err, len(core.seen()))
	}

	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(stalled.Close)
	t.Cleanup(func() { close(release) })
	client = tracking.NewCoreClient(stalled.URL, staticToken{token: "t"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := client.Resolve(ctx, []uuid.UUID{uuid.New()}); err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("a stalled core: %v after %s", err, time.Since(started))
	}
}

// A reason core gives is the row's skip reason; one SkyMail does not know is
// not_allowed.
func TestSkipReasonOfCoresReason(t *testing.T) {
	for core, want := range map[string]string{
		"preference_off": tracking.SkipPreferenceOff,
		"not_member":     tracking.SkipNotMember,
		"inactive":       tracking.SkipInactive,
		"":               tracking.SkipNotAllowed,
		"suspended":      tracking.SkipNotAllowed,
		"no_account":     tracking.SkipNotAllowed,
	} {
		if got := tracking.SkipReasonOf(core); got != want {
			t.Errorf("SkipReasonOf(%q) = %q, want %q", core, got, want)
		}
	}
}
