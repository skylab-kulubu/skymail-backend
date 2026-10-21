package mailer_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/msauth"
	"github.com/wneessen/go-mail"
)

// fakeEntra is Entra's token endpoint: it verifies each client assertion with
// the application's certificate and answers with a token named after the
// scope, numbered.
type fakeEntra struct {
	server *httptest.Server
	mu     sync.Mutex
	asked  map[string]int
}

func startFakeEntra(t *testing.T) *fakeEntra {
	t.Helper()
	key, _ := appKey()
	e := &fakeEntra{asked: map[string]int{}}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		_, err := jwt.Parse(r.PostForm.Get("client_assertion"), func(*jwt.Token) (any, error) { return &key.PublicKey, nil },
			jwt.WithAudience(e.server.URL+"/"+laneTenant+"/oauth2/v2.0/token"), jwt.WithValidMethods([]string{"PS256"}))
		if err != nil || r.PostForm.Get("client_id") != laneClient {
			t.Errorf("a client assertion that does not verify: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		scope := r.PostForm.Get("scope")
		e.mu.Lock()
		e.asked[scope]++
		n := e.asked[scope]
		e.mu.Unlock()
		name := map[string]string{msauth.ScopeGraph: "graph", msauth.ScopeExchange: "exchange"}[scope]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type": "Bearer", "expires_in": 3599, "access_token": name + "-token-" + string(rune('0'+n)),
		})
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *fakeEntra) times(scope string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.asked[scope]
}

// graphCall is one request the fake Graph took: a JSON sendMail's body, or a
// MIME one's mail, decoded.
type graphCall struct {
	path, authorization, contentType, clientRequestID string
	body                                              map[string]any
	raw                                               string
	mime                                              string
}

// fakeGraph is Microsoft Graph's sendMail: it keeps each request and answers
// 202, or what answer says.
type fakeGraph struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []graphCall
	answer func(w http.ResponseWriter) bool
}

func startFakeGraph(t *testing.T) *fakeGraph {
	t.Helper()
	g := &fakeGraph{}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		call := graphCall{
			path: r.URL.EscapedPath(), authorization: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), clientRequestID: r.Header.Get("client-request-id"),
			raw: string(raw),
		}
		if call.contentType == "text/plain" {
			mime, err := base64.StdEncoding.DecodeString(string(raw))
			if err != nil {
				t.Errorf("a MIME sendMail body is not base64: %v", err)
			}
			call.mime = string(mime)
		} else if err := json.Unmarshal(raw, &call.body); err != nil {
			t.Errorf("sendMail body is not JSON: %v", err)
		}
		g.mu.Lock()
		g.calls = append(g.calls, call)
		answer := g.answer
		g.mu.Unlock()
		if r.Method != http.MethodPost {
			t.Errorf("method %s", r.Method)
		}
		w.Header().Set("request-id", "8a1b2c3d-0000-4000-8000-00000000000a")
		if answer != nil && answer(w) {
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGraph) taken() []graphCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]graphCall(nil), g.calls...)
}

// oauthMail is a mail the relay took and the AUTH it came with: the
// mechanism, the user, and the token or password.
type oauthMail struct {
	mechanism, user, token, mailFrom, data string
}

// xoauth2Relay speaks SMTP with AUTH XOAUTH2, as smtp.office365.com offers it
// to an application, and takes a token it was told to. It takes AUTH LOGIN
// and PLAIN too, with the password accept is told, as the off path uses them.
type xoauth2Relay struct {
	listener net.Listener
	accept   func(token string) bool
	mu       sync.Mutex
	mails    []oauthMail
	auths    atomic.Int32
}

func startXOAUTH2Relay(t *testing.T, accept func(token string) bool) *xoauth2Relay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &xoauth2Relay{listener: listener, accept: accept}
	var conns sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				r.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		conns.Wait()
	})
	return r
}

func (r *xoauth2Relay) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	reply := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }
	reply("220 relay.test ESMTP")
	var current oauthMail
	authed := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line + " x")
		switch verb := strings.ToUpper(fields[0]); {
		case verb == "EHLO" || verb == "HELO":
			reply("250-relay.test")
			reply("250-AUTH LOGIN PLAIN XOAUTH2")
			reply("250 8BITMIME")
		case verb == "AUTH" && len(fields) >= 2 && strings.ToUpper(fields[1]) == "LOGIN":
			r.auths.Add(1)
			current.mechanism = "LOGIN"
			var answers []string
			for _, prompt := range []string{"VXNlcm5hbWU6", "UGFzc3dvcmQ6"} {
				reply("334 " + prompt)
				answer, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				decoded, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(answer))
				answers = append(answers, string(decoded))
			}
			current.user, current.token = answers[0], answers[1]
			if !r.accept(current.token) {
				reply("535 5.7.3 Authentication unsuccessful")
				continue
			}
			authed = true
			reply("235 2.7.0 Authentication successful")
		case verb == "AUTH" && len(fields) >= 3 && strings.ToUpper(fields[1]) == "PLAIN":
			r.auths.Add(1)
			current.mechanism = "PLAIN"
			decoded, _ := base64.StdEncoding.DecodeString(fields[2])
			parts := strings.Split(string(decoded), "\x00")
			if len(parts) != 3 {
				reply("501 5.5.2 malformed PLAIN")
				continue
			}
			current.user, current.token = parts[1], parts[2]
			if !r.accept(current.token) {
				reply("535 5.7.3 Authentication unsuccessful")
				continue
			}
			authed = true
			reply("235 2.7.0 Authentication successful")
		case verb == "AUTH":
			r.auths.Add(1)
			current.mechanism = "XOAUTH2"
			if len(fields) < 3 || strings.ToUpper(fields[1]) != "XOAUTH2" {
				reply("504 5.7.4 unrecognized authentication type")
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(fields[2])
			if err != nil {
				reply("501 5.5.2 cannot decode")
				continue
			}
			parts := strings.Split(string(raw), "\x01")
			if len(parts) != 4 || !strings.HasPrefix(parts[0], "user=") || !strings.HasPrefix(parts[1], "auth=Bearer ") {
				reply("501 5.5.2 malformed XOAUTH2")
				continue
			}
			current.user = strings.TrimPrefix(parts[0], "user=")
			current.token = strings.TrimPrefix(parts[1], "auth=Bearer ")
			if !r.accept(current.token) {
				reply("535 5.7.3 Authentication unsuccessful")
				continue
			}
			authed = true
			reply("235 2.7.0 Authentication successful")
		case verb == "MAIL":
			if !authed {
				reply("530 5.7.57 Client not authenticated to send mail")
				continue
			}
			from := fields[1]
			if i := strings.IndexByte(from, '<'); i >= 0 {
				from = from[i+1:]
			}
			current.mailFrom = strings.TrimSuffix(from, ">")
			reply("250 2.1.0 Sender OK")
		case verb == "DATA":
			reply("354 go ahead")
			var data strings.Builder
			for {
				part, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if part == ".\r\n" {
					break
				}
				data.WriteString(part)
			}
			current.data = data.String()
			r.mu.Lock()
			r.mails = append(r.mails, current)
			r.mu.Unlock()
			reply("250 2.6.0 queued")
		case verb == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func (r *xoauth2Relay) taken() []oauthMail {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]oauthMail(nil), r.mails...)
}

// headerOf is a header's value in a mail as the relay took it.
func headerOf(data, name string) string {
	for _, line := range strings.Split(data, "\r\n") {
		if line == "" {
			return ""
		}
		if k, v, ok := strings.Cut(line, ": "); ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// laneMailer is a mailer with the decided lanes, its Graph and Entra the
// fakes, its relay the XOAUTH2 one, reading templates from keys.
type laneMailer struct {
	m      mailer.Transactional
	entra  *fakeEntra
	graph  *fakeGraph
	relay  *xoauth2Relay
	tokens *msauth.TokenSource
}

func newLaneMailer(t *testing.T, env map[string]string, keys map[uuid.UUID]struct {
	key    *string
	system bool
}) *laneMailer {
	t.Helper()
	lanes, err := mailer.SendLanesFromEnv(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	l := &laneMailer{entra: startFakeEntra(t), graph: startFakeGraph(t)}
	l.relay = startXOAUTH2Relay(t, func(token string) bool { return strings.HasPrefix(token, "exchange-token-") })
	l.m = mailer.NewMailer(nil, mailer.SMTPConfig{
		FromEmail: "SKY LAB <info@example.org>", Host: "127.0.0.1",
		Port: l.relay.listener.Addr().(*net.TCPAddr).Port, User: "info@example.org", Password: "not-used", FQDN: "example.com",
	}, mailer.SenderOn, mailer.WithSendLanes(lanes))
	// The off path without AUTH or TLS, the XOAUTH2 lane without TLS: the
	// lane's AUTH XOAUTH2 is always its own.
	plainSMTP(l.m)
	if lanes.Credentials != nil {
		l.tokens = msauth.NewTokenSource(lanes.Credentials, msauth.ScopeGraph, msauth.WithAuthority(l.entra.server.URL))
		mailer.SetGraph(l.m, l.graph.server.URL+"/v1.0", l.graph.server.Client(), l.tokens)
		mailer.SetExchangeTokens(l.m, msauth.NewTokenSource(lanes.Credentials, msauth.ScopeExchange, msauth.WithAuthority(l.entra.server.URL)))
	}
	mailer.SetTemplateKeys(l.m, func(_ context.Context, task uuid.UUID) (*string, bool, error) {
		k, ok := keys[task]
		if !ok {
			t.Errorf("the template of task %s was read", task)
		}
		return k.key, k.system, nil
	})
	return l
}

func html(s string) *string { return &s }

// The decided lanes (hub ticket 10): a login code goes out of hesap@ and a
// Forms confirmation out of bilgi@ through Graph sendMail, with no copy in
// Sent Items; a send to a list goes out of duyuru@ over SMTP with AUTH
// XOAUTH2. Every mail has Reply-To info@ and X-SkyMail-Queue-Id; Graph's token
// is fetched once for both of its mails.
func TestTheLanesSendFromTheirMailboxes(t *testing.T) {
	captureMailerLogs(t)
	loginTask, formsTask, listTask := uuid.New(), uuid.New(), uuid.New()
	l := newLaneMailer(t, decidedEnv(), map[uuid.UUID]struct {
		key    *string
		system bool
	}{
		loginTask: {ptr("keycloak.login-code"), true},
		formsTask: {ptr("forms.submission-received"), true},
		listTask:  {nil, false},
	})
	ctx := context.Background()
	login := database.MailQueue{ID: uuid.New(), TaskID: loginTask, Channel: mailer.ChannelEmail, Priority: int16(mailer.PrioritySystem),
		RecipientFullName: "Ayşe Yılmaz", RecipientEmail: "ayse@example.net", Subject: "Giriş kodun", Body: "Kod: 482910",
		BodyHtml: html("<p>Kod: <b>482910</b></p>")}
	forms := database.MailQueue{ID: uuid.New(), TaskID: formsTask, Channel: mailer.ChannelEmail, Priority: int16(mailer.PrioritySystem),
		RecipientFullName: "Ali Veli", RecipientEmail: "ali@example.net", Subject: "Başvurun alındı", Body: "Teşekkürler"}
	list := database.MailQueue{ID: uuid.New(), TaskID: listTask, Channel: mailer.ChannelEmail, Priority: int16(mailer.PriorityBulk),
		RecipientFullName: "Üye", RecipientEmail: "uye@example.net", Subject: "Ekim duyurusu", Body: "Duyuru",
		BodyHtml: html("<p>Duyuru</p>")}
	for _, job := range []database.MailQueue{login, forms, list} {
		if err := mailer.SendOnce(ctx, l.m, job); err != nil {
			t.Fatalf("send %s: %v", job.Subject, err)
		}
	}

	calls := l.graph.taken()
	if len(calls) != 2 {
		t.Fatalf("Graph took %d requests, want 2", len(calls))
	}
	for i, want := range []struct {
		job                     database.MailQueue
		path, fromName, from    string
		contentType, content    string
		recipientName, toAddres string
	}{
		{login, "/v1.0/users/hesap@bildirim.example.org/sendMail", "SKY LAB Hesap", "hesap@bildirim.example.org",
			"HTML", "<p>Kod: <b>482910</b></p>", "Ayşe Yılmaz", "ayse@example.net"},
		{forms, "/v1.0/users/bilgi@bildirim.example.org/sendMail", "SKY LAB Bildirim Hizmeti", "bilgi@bildirim.example.org",
			"Text", "Teşekkürler", "Ali Veli", "ali@example.net"},
	} {
		call := calls[i]
		if call.path != want.path || call.authorization != "Bearer graph-token-1" || call.contentType != "application/json" ||
			call.clientRequestID != want.job.ID.String() {
			t.Errorf("request %d: %s %q %q %q", i, call.path, call.authorization, call.contentType, call.clientRequestID)
		}
		saved, present := call.body["saveToSentItems"]
		if !present || saved != false {
			t.Errorf("request %d: saveToSentItems %v (present %v), want false", i, saved, present)
		}
		message, _ := call.body["message"].(map[string]any)
		wantMessage := map[string]any{
			"subject": want.job.Subject,
			"body":    map[string]any{"contentType": want.contentType, "content": want.content},
			"from":    map[string]any{"emailAddress": map[string]any{"name": want.fromName, "address": want.from}},
			"toRecipients": []any{map[string]any{"emailAddress": map[string]any{
				"name": want.recipientName, "address": want.toAddres}}},
			"replyTo": []any{map[string]any{"emailAddress": map[string]any{"address": "info@example.org"}}},
			"internetMessageHeaders": []any{map[string]any{
				"name": "X-SkyMail-Queue-Id", "value": "<" + want.job.ID.String() + "@example.com>"}},
		}
		gotJSON, _ := json.Marshal(message)
		wantJSON, _ := json.Marshal(wantMessage)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("request %d message\n got %s\nwant %s", i, gotJSON, wantJSON)
		}
	}

	// The markup goes as it is, not as \u003c escapes.
	if !strings.Contains(calls[0].raw, `"content":"<p>Kod: <b>482910</b></p>"`) {
		t.Errorf("the HTML is escaped in the request: %s", calls[0].raw)
	}

	mails := l.relay.taken()
	if len(mails) != 1 {
		t.Fatalf("the relay took %d mails, want 1", len(mails))
	}
	got := mails[0]
	if got.user != "duyuru@bildirim.example.org" || got.token != "exchange-token-1" || got.mailFrom != "duyuru@bildirim.example.org" {
		t.Errorf("AUTH as %q with %q, MAIL FROM %q", got.user, got.token, got.mailFrom)
	}
	for header, want := range map[string]string{
		"From":               `"SKY LAB Bildirim Hizmeti" <duyuru@bildirim.example.org>`,
		"Reply-To":           "<info@example.org>",
		"Message-ID":         "<" + list.ID.String() + "@example.com>",
		"X-SkyMail-Queue-Id": "<" + list.ID.String() + "@example.com>",
		"Subject":            "Ekim duyurusu",
	} {
		if v := headerOf(got.data, header); v != want {
			t.Errorf("%s: %q, want %q", header, v, want)
		}
	}
	if l.entra.times(msauth.ScopeGraph) != 1 || l.entra.times(msauth.ScopeExchange) != 1 {
		t.Errorf("tokens asked: graph %d, exchange %d; want one each", l.entra.times(msauth.ScopeGraph), l.entra.times(msauth.ScopeExchange))
	}
}

// With every lane off, a mail goes out as it always has: from SMTP_FROM,
// without X-SkyMail-Queue-Id, and its template is never read. SMTP_REPLY_TO
// alone adds the Reply-To.
func TestEveryLaneOffSendsAsAlways(t *testing.T) {
	for _, replyTo := range []string{"", "info@example.org"} {
		t.Run("reply-to "+replyTo, func(t *testing.T) {
			captureMailerLogs(t)
			relay := startAnsweringRelay(t, "", "")
			lanes, err := mailer.SendLanesFromEnv(getenv(map[string]string{"SMTP_REPLY_TO": replyTo}))
			if err != nil {
				t.Fatal(err)
			}
			m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn, mailer.WithSendLanes(lanes))
			plainSMTP(m)
			mailer.SetTemplateKeys(m, func(context.Context, uuid.UUID) (*string, bool, error) {
				t.Error("a template was read with every lane off")
				return nil, false, nil
			})
			id := uuid.New()
			if err := mailer.SendOnce(context.Background(), m, database.MailQueue{
				ID: id, Channel: mailer.ChannelEmail, RecipientFullName: "Alıcı", RecipientEmail: "alici@example.com",
				Subject: "Konu", Body: "Gövde",
			}); err != nil {
				t.Fatal(err)
			}
			mails := relay.taken()
			if len(mails) != 1 {
				t.Fatalf("%d mails", len(mails))
			}
			if from := headerOf(mails[0], "From"); from != "<skymail@example.com>" {
				t.Errorf("From %q", from)
			}
			if v := headerOf(mails[0], "X-SkyMail-Queue-Id"); v != "" {
				t.Errorf("X-SkyMail-Queue-Id %q on the off path", v)
			}
			wantReplyTo := ""
			if replyTo != "" {
				wantReplyTo = "<" + replyTo + ">"
			}
			if v := headerOf(mails[0], "Reply-To"); v != wantReplyTo {
				t.Errorf("Reply-To %q, want %q", v, wantReplyTo)
			}
		})
	}
}

// One lane on, the others off: a mail of an off lane still goes the old way.
func TestALaneOffFallsBackToSMTPAuth(t *testing.T) {
	captureMailerLogs(t)
	env := decidedEnv()
	env["SKYMAIL_SEND_BILGI"] = "off"
	env["SKYMAIL_SEND_DUYURU"] = "off"
	task := uuid.New()
	l := newLaneMailer(t, env, map[uuid.UUID]struct {
		key    *string
		system bool
	}{task: {ptr("forms.submission-received"), true}})
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn, mailer.WithSendLanes(mustLanes(t, env)))
	plainSMTP(m)
	mailer.SetGraph(m, l.graph.server.URL+"/v1.0", l.graph.server.Client(), l.tokens)
	mailer.SetTemplateKeys(m, func(context.Context, uuid.UUID) (*string, bool, error) { return ptr("forms.x"), true, nil })
	if err := mailer.SendOnce(context.Background(), m, database.MailQueue{ID: uuid.New(), TaskID: task, Channel: mailer.ChannelEmail,
		Priority: int16(mailer.PrioritySystem), RecipientFullName: "Alıcı", RecipientEmail: "alici@example.com", Subject: "Konu", Body: "Gövde"}); err != nil {
		t.Fatal(err)
	}
	if len(relay.taken()) != 1 || len(l.graph.taken()) != 0 {
		t.Fatalf("relay %d, graph %d: want the relay", len(relay.taken()), len(l.graph.taken()))
	}
	if from := headerOf(relay.taken()[0], "From"); from != "<skymail@example.com>" {
		t.Errorf("From %q", from)
	}
}

func mustLanes(t *testing.T, env map[string]string) mailer.SendLanes {
	t.Helper()
	lanes, err := mailer.SendLanesFromEnv(getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	return lanes
}

// Graph's refusals: an invalid recipient fails the row at once; a throttle
// keeps its Retry-After and is tried again; a 401 drops the token, and the
// next send asks for a new one. An address that does not parse never
// reaches Graph. No error carries Graph's message text.
func TestGraphRefusals(t *testing.T) {
	captureMailerLogs(t)
	task := uuid.New()
	l := newLaneMailer(t, decidedEnv(), map[uuid.UUID]struct {
		key    *string
		system bool
	}{task: {ptr("keycloak.login-code"), true}})
	job := func(email string) database.MailQueue {
		return database.MailQueue{ID: uuid.New(), TaskID: task, Channel: mailer.ChannelEmail, Priority: int16(mailer.PrioritySystem),
			RecipientFullName: "Alıcı", RecipientEmail: email, Subject: "Kod", Body: "482910"}
	}
	answer := func(status int, retryAfter, code string) {
		l.graph.mu.Lock()
		defer l.graph.mu.Unlock()
		l.graph.answer = func(w http.ResponseWriter) bool {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"Recipient 'kisi@example.net' isn't resolved."}}`)
			return true
		}
	}
	ctx := context.Background()

	answer(http.StatusBadRequest, "", "ErrorInvalidRecipients")
	err := mailer.SendOnce(ctx, l.m, job("kisi@example.net"))
	if !mailer.IsPermanent(err) || err.Error() != "graph_rejected: 400 ErrorInvalidRecipients" {
		t.Errorf("invalid recipient: %v (permanent %v)", err, mailer.IsPermanent(err))
	}

	answer(http.StatusTooManyRequests, "30", "ApplicationThrottled")
	err = mailer.SendOnce(ctx, l.m, job("kisi@example.net"))
	if mailer.IsPermanent(err) || err.Error() != "graph_deferred: 429 ApplicationThrottled retry-after 30s" {
		t.Errorf("throttled: %v", err)
	}

	answer(http.StatusUnauthorized, "", "InvalidAuthenticationToken")
	if err := mailer.SendOnce(ctx, l.m, job("kisi@example.net")); mailer.IsPermanent(err) || err == nil {
		t.Errorf("401: %v", err)
	}
	answer(http.StatusAccepted, "", "")
	if err := mailer.SendOnce(ctx, l.m, job("kisi@example.net")); err != nil {
		t.Fatal(err)
	}
	calls := l.graph.taken()
	if last := calls[len(calls)-1]; last.authorization != "Bearer graph-token-2" || l.entra.times(msauth.ScopeGraph) != 2 {
		t.Errorf("after a 401: %q, %d tokens asked", last.authorization, l.entra.times(msauth.ScopeGraph))
	}

	before := len(calls)
	err = mailer.SendOnce(ctx, l.m, job("not an address"))
	if !mailer.IsPermanent(err) || !strings.HasPrefix(err.Error(), "invalid_recipient:") || len(l.graph.taken()) != before {
		t.Errorf("unparsable address: %v, Graph took %d", err, len(l.graph.taken())-before)
	}

	// A mail over Graph's 4 MB request limit is refused before it is sent.
	large := job("kisi@example.net")
	large.Body = strings.Repeat("x", 4<<20)
	err = mailer.SendOnce(ctx, l.m, large)
	if !mailer.IsPermanent(err) || !strings.HasPrefix(err.Error(), "graph_rejected: the mail is over") || len(l.graph.taken()) != before {
		t.Errorf("a mail over 4 MB: %v, Graph took %d", err, len(l.graph.taken())-before)
	}

	// Any 2xx took the mail: a 200 is sent, not tried again.
	l.graph.mu.Lock()
	l.graph.answer = func(w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusOK)
		return true
	}
	l.graph.mu.Unlock()
	if err := mailer.SendOnce(ctx, l.m, job("kisi@example.net")); err != nil {
		t.Errorf("a 200: %v", err)
	}
	if n := len(l.graph.taken()) - before; n != 1 {
		t.Errorf("a 200: Graph took %d requests, want 1", n)
	}
}

// The XOAUTH2 lane waits for its token no longer than the send budget, as
// the Graph lane does: a slow Entra cannot hold a row past its lease.
func TestXOAUTH2TokenWaitIsBoundedByTheSendBudget(t *testing.T) {
	captureMailerLogs(t)
	task := uuid.New()
	l := newLaneMailer(t, decidedEnv(), map[uuid.UUID]struct {
		key    *string
		system bool
	}{task: {nil, false}})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the body first: only then does the server notice the client
		// going away and end r's context.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	mailer.SetExchangeTokens(l.m, msauth.NewTokenSource(mustLanes(t, decidedEnv()).Credentials, msauth.ScopeExchange, msauth.WithAuthority(slow.URL)))
	mailer.SetSendBudget(l.m, 300*time.Millisecond)
	begun := time.Now()
	err := mailer.SendOnce(context.Background(), l.m, database.MailQueue{ID: uuid.New(), TaskID: task, Channel: mailer.ChannelEmail,
		Priority: int16(mailer.PriorityBulk), RecipientFullName: "Üye", RecipientEmail: "uye@example.net", Subject: "Duyuru", Body: "Duyuru"})
	if err == nil || mailer.IsPermanent(err) || !strings.HasPrefix(err.Error(), "send failed: exchange token:") || time.Since(begun) > 2*time.Second {
		t.Fatalf("%v after %v, want a transient token failure within the budget", err, time.Since(begun))
	}
	if len(l.relay.taken()) != 0 {
		t.Fatal("a mail reached the relay")
	}
}

// The off path authenticates as it always has: AUTH LOGIN, or PLAIN with
// SMTP_PLAIN, as SMTP_USER with SMTP_PASS, from SMTP_FROM; the lanes' XOAUTH2
// client is not even made.
func TestTheOffPathAuthenticatesAsSMTPUser(t *testing.T) {
	for _, plain := range []bool{false, true} {
		want := map[bool]string{false: "LOGIN", true: "PLAIN"}[plain]
		t.Run(want, func(t *testing.T) {
			captureMailerLogs(t)
			relay := startXOAUTH2Relay(t, func(secret string) bool { return secret == "smtp-pass" })
			m := mailer.NewMailer(nil, mailer.SMTPConfig{
				FromEmail: "SKY LAB <info@example.org>", Host: "127.0.0.1", Port: relay.listener.Addr().(*net.TCPAddr).Port,
				User: "info@example.org", Password: "smtp-pass", FQDN: "example.com", Plain: plain,
			}, mailer.SenderOn)
			mailer.SetClientOptions(m, mail.WithTLSPolicy(mail.NoTLS))
			mailer.SetSendBudget(m, 30*time.Second)
			if err := mailer.SendOnce(context.Background(), m, database.MailQueue{ID: uuid.New(), Channel: mailer.ChannelEmail,
				RecipientFullName: "Alıcı", RecipientEmail: "alici@example.net", Subject: "Konu", Body: "Gövde"}); err != nil {
				t.Fatal(err)
			}
			mails := relay.taken()
			if len(mails) != 1 {
				t.Fatalf("%d mails", len(mails))
			}
			got := mails[0]
			if got.mechanism != want || got.user != "info@example.org" || got.token != "smtp-pass" || got.mailFrom != "info@example.org" {
				t.Errorf("AUTH %s as %q, MAIL FROM %q; want %s as info@example.org", got.mechanism, got.user, got.mailFrom, want)
			}
			if from := headerOf(got.data, "From"); from != `"SKY LAB" <info@example.org>` {
				t.Errorf("From %q", from)
			}
		})
	}
}

// AUTH XOAUTH2 refused drops the token: the next send asks for a new one.
func TestXOAUTH2RefusalDropsTheToken(t *testing.T) {
	captureMailerLogs(t)
	task := uuid.New()
	l := newLaneMailer(t, decidedEnv(), map[uuid.UUID]struct {
		key    *string
		system bool
	}{task: {nil, false}})
	l.relay.accept = func(token string) bool { return token == "exchange-token-2" }
	job := database.MailQueue{ID: uuid.New(), TaskID: task, Channel: mailer.ChannelEmail, Priority: int16(mailer.PriorityBulk),
		RecipientFullName: "Üye", RecipientEmail: "uye@example.net", Subject: "Duyuru", Body: "Duyuru"}
	err := mailer.SendOnce(context.Background(), l.m, job)
	if err == nil || mailer.IsPermanent(err) || !strings.HasPrefix(err.Error(), "dial failed:") || strings.Contains(err.Error(), "exchange-token") {
		t.Fatalf("refused AUTH: %v", err)
	}
	if err := mailer.SendOnce(context.Background(), l.m, job); err != nil {
		t.Fatalf("with a new token: %v", err)
	}
	if n := l.entra.times(msauth.ScopeExchange); n != 2 {
		t.Errorf("%d exchange tokens asked, want 2", n)
	}
}

// Graph's answers, classified: only an invalid recipient (400
// ErrorInvalidRecipients) and a mail too large (413) fail a row at once;
// everything else is tried again, Retry-After kept.
func TestGraphFailureClassification(t *testing.T) {
	now := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		status     int
		code       string
		retryAfter string
		permanent  bool
		wait       time.Duration
		text       string
	}{
		{400, "ErrorInvalidRecipients", "", true, 0, "graph_rejected: 400 ErrorInvalidRecipients"},
		{413, "ErrorMessageSizeExceeded", "", true, 0, "graph_rejected: 413 ErrorMessageSizeExceeded"},
		{400, "RequestBodyRead", "", false, 0, "graph_deferred: 400 RequestBodyRead"},
		{401, "InvalidAuthenticationToken", "", false, 0, "graph_deferred: 401 InvalidAuthenticationToken"},
		{403, "ErrorAccessDenied", "", false, 0, "graph_deferred: 403 ErrorAccessDenied"},
		{404, "ErrorInvalidUser", "", false, 0, "graph_deferred: 404 ErrorInvalidUser"},
		{429, "ApplicationThrottled", "120", false, 2 * time.Minute, "graph_deferred: 429 ApplicationThrottled retry-after 2m0s"},
		{503, "ServiceUnavailable", "Wed, 21 Oct 2026 10:00:45 GMT", false, 45 * time.Second, "graph_deferred: 503 ServiceUnavailable retry-after 45s"},
		{500, "", "", false, 0, "graph_deferred: 500"},
		{400, "a code with spaces and 'kisi@example.net'", "", false, 0, "graph_deferred: 400"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			header := http.Header{}
			if tc.retryAfter != "" {
				header.Set("Retry-After", tc.retryAfter)
			}
			body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": tc.code, "message": "Recipient 'kisi@example.net' isn't resolved."}})
			err := mailer.GraphFailure(tc.status, body, header, now)
			if mailer.IsPermanent(err) != tc.permanent || err.Error() != tc.text {
				t.Errorf("%v (permanent %v)", err, mailer.IsPermanent(err))
			}
			var later interface{ RetryAfter() time.Duration }
			if ok := errors.As(err, &later); !ok || later.RetryAfter() != tc.wait {
				t.Errorf("retry after %v", later)
			}
		})
	}
	for value, want := range map[string]time.Duration{
		"": 0, "0": 0, "-5": 0, "soon": 0, "7200": time.Hour, "Wed, 21 Oct 2026 09:00:00 GMT": 0,
	} {
		if got := mailer.RetryAfterOf(value, now); got != want {
			t.Errorf("Retry-After %q = %v, want %v", value, got, want)
		}
	}
}
