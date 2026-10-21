package mailer_test

import (
	"context"
	"net/mail"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// coreLink is an unsubscribe link as core signs it.
const coreLink = "https://api.example.org/v1/communication-preferences/unsubscribe?t=AQEBGqzN0x8Kc2Vjb25kLWZha2UtdG9rZW4tYWJjZGVmZ2hpamtsbW5vcA"

// graphEnv is every lane on Graph, as production sends, with the switch.
func graphEnv(listUnsubscribe string) map[string]string {
	env := decidedEnv()
	env["SKYMAIL_SEND_DUYURU"] = "graph"
	env["SKYMAIL_FROM_DUYURU"] = "SKY LAB Duyurular <duyuru@bildirim.example.org>"
	if listUnsubscribe != "" {
		env["SKYMAIL_LIST_UNSUBSCRIBE"] = listUnsubscribe
	}
	return env
}

// laneJobs are a bulk mail with a link, one without, and a login code and a
// Forms confirmation whose rows carry one anyway (no caller can put one there:
// the header must still not follow).
type laneJobs struct {
	withLink, withoutLink, login, forms database.MailQueue
	keys                                map[uuid.UUID]struct {
		key    *string
		system bool
	}
}

func newLaneJobs() laneJobs {
	link := coreLink
	bulk, login, forms := uuid.New(), uuid.New(), uuid.New()
	j := laneJobs{keys: map[uuid.UUID]struct {
		key    *string
		system bool
	}{
		bulk:  {ptr("club.news"), false},
		login: {ptr("keycloak.login-code"), true},
		forms: {ptr("forms.submission-received"), true},
	}}
	j.withLink = database.MailQueue{ID: uuid.New(), TaskID: bulk, Channel: mailer.ChannelEmail, Priority: int16(mailer.PriorityBulk),
		RecipientFullName: "Ayşe Yılmaz", RecipientEmail: "ayse@example.net", Subject: "Ekim duyurusu", Body: "Duyuru metni",
		BodyHtml: html("<p>Duyuru <b>metni</b></p>"), ListUnsubscribeUrl: &link}
	j.withoutLink = database.MailQueue{ID: uuid.New(), TaskID: bulk, Channel: mailer.ChannelEmail, Priority: int16(mailer.PriorityBulk),
		RecipientFullName: "Ali Veli", RecipientEmail: "ali@example.net", Subject: "Ekim duyurusu", Body: "Duyuru metni"}
	j.login = database.MailQueue{ID: uuid.New(), TaskID: login, Channel: mailer.ChannelEmail, Priority: int16(mailer.PrioritySystem),
		RecipientFullName: "Ayşe Yılmaz", RecipientEmail: "ayse@example.net", Subject: "Giriş kodun", Body: "Kod: 482910",
		ListUnsubscribeUrl: &link}
	j.forms = database.MailQueue{ID: uuid.New(), TaskID: forms, Channel: mailer.ChannelEmail, Priority: int16(mailer.PrioritySystem),
		RecipientFullName: "Ali Veli", RecipientEmail: "ali@example.net", Subject: "Başvurun alındı", Body: "Teşekkürler",
		ListUnsubscribeUrl: &link}
	return j
}

func sendEach(t *testing.T, m mailer.Transactional, jobs ...database.MailQueue) {
	t.Helper()
	for _, job := range jobs {
		if err := mailer.SendOnce(context.Background(), m, job); err != nil {
			t.Fatalf("send %s to %s: %v", job.Subject, job.RecipientEmail, err)
		}
	}
}

// With the switch on, a bulk mail core gave a link for goes out of duyuru@ as
// MIME with both RFC 8058 headers, its plain text and HTML, Reply-To and
// X-SkyMail-Queue-Id; Graph's JSON could not carry them. A bulk mail without
// a link stays JSON with no copy in Sent Items, and hesap@ and bilgi@ never
// carry the headers whatever their row holds.
func TestABulkMailWithALinkGoesAsMIMEWithListUnsubscribe(t *testing.T) {
	captureMailerLogs(t)
	j := newLaneJobs()
	l := newLaneMailer(t, graphEnv("on"), j.keys)
	sendEach(t, l.m, j.withLink, j.withoutLink, j.login, j.forms)

	calls := l.graph.taken()
	if len(calls) != 4 {
		t.Fatalf("Graph took %d requests, want 4", len(calls))
	}
	mime := calls[0]
	if mime.path != "/v1.0/users/duyuru@bildirim.example.org/sendMail" || mime.contentType != "text/plain" ||
		mime.clientRequestID != j.withLink.ID.String() || mime.authorization != "Bearer graph-token-1" {
		t.Fatalf("MIME request: %s %q %q %q", mime.path, mime.contentType, mime.clientRequestID, mime.authorization)
	}
	msg, err := mail.ReadMessage(strings.NewReader(mime.mime))
	if err != nil {
		t.Fatalf("the MIME body is not a mail: %v\n%s", err, mime.mime)
	}
	queueID := "<" + j.withLink.ID.String() + "@example.com>"
	for header, want := range map[string]string{
		"List-Unsubscribe":      "<" + coreLink + ">",
		"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		"From":                  `"SKY LAB Duyurular" <duyuru@bildirim.example.org>`,
		"To":                    `=?utf-8?q?Ay=C5=9Fe_Y=C4=B1lmaz?= <ayse@example.net>`,
		"Reply-To":              "<info@example.org>",
		"Subject":               "Ekim duyurusu",
		"Message-Id":            queueID,
		"X-Skymail-Queue-Id":    queueID,
	} {
		if got := msg.Header.Get(header); got != want {
			t.Errorf("%s: %q, want %q", header, got, want)
		}
	}
	if !strings.Contains(msg.Header.Get("Content-Type"), "multipart/alternative") ||
		!strings.Contains(mime.mime, "Duyuru metni") || !strings.Contains(mime.mime, "<b>metni</b>") {
		t.Errorf("the mail has not both parts:\n%s", mime.mime)
	}
	if strings.Contains(mime.mime, "saveToSentItems") {
		t.Errorf("a MIME body carries JSON")
	}

	for i, want := range []struct {
		job  database.MailQueue
		path string
	}{
		{j.withoutLink, "/v1.0/users/duyuru@bildirim.example.org/sendMail"},
		{j.login, "/v1.0/users/hesap@bildirim.example.org/sendMail"},
		{j.forms, "/v1.0/users/bilgi@bildirim.example.org/sendMail"},
	} {
		call := calls[i+1]
		if call.path != want.path || call.contentType != "application/json" || call.clientRequestID != want.job.ID.String() {
			t.Errorf("request %d: %s %q", i+1, call.path, call.contentType)
		}
		if saved, present := call.body["saveToSentItems"]; !present || saved != false {
			t.Errorf("request %d: saveToSentItems %v", i+1, saved)
		}
		if strings.Contains(call.raw, "List-Unsubscribe") || strings.Contains(call.raw, "unsubscribe") {
			t.Errorf("request %d carries the link: %s", i+1, call.raw)
		}
	}
}

// Off (the default) every Graph mail is JSON as before, the row's link or not.
func TestTheSwitchOffSendsEveryMailAsBefore(t *testing.T) {
	for _, value := range []string{"", "off", "OFF"} {
		t.Run("SKYMAIL_LIST_UNSUBSCRIBE="+value, func(t *testing.T) {
			captureMailerLogs(t)
			j := newLaneJobs()
			l := newLaneMailer(t, graphEnv(value), j.keys)
			sendEach(t, l.m, j.withLink)
			calls := l.graph.taken()
			if len(calls) != 1 || calls[0].contentType != "application/json" || strings.Contains(calls[0].raw, "unsubscribe") {
				t.Fatalf("requests %+v", calls)
			}
		})
	}
}

// The XOAUTH2 lane writes the headers into the SMTP mail the same way.
func TestTheXOAUTH2LaneCarriesListUnsubscribe(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"on", true}, {"off", false}} {
		t.Run(tc.value, func(t *testing.T) {
			captureMailerLogs(t)
			j := newLaneJobs()
			env := decidedEnv()
			env["SKYMAIL_LIST_UNSUBSCRIBE"] = tc.value
			l := newLaneMailer(t, env, j.keys)
			sendEach(t, l.m, j.withLink, j.withoutLink)
			mails := l.relay.taken()
			if len(mails) != 2 {
				t.Fatalf("the relay took %d mails, want 2", len(mails))
			}
			got := headerOf(mails[0].data, "List-Unsubscribe")
			post := headerOf(mails[0].data, "List-Unsubscribe-Post")
			if tc.want && (got != "<"+coreLink+">" || post != "List-Unsubscribe=One-Click") {
				t.Errorf("List-Unsubscribe %q, -Post %q", got, post)
			}
			if !tc.want && (got != "" || post != "") {
				t.Errorf("the switch off and the mail carries %q %q", got, post)
			}
			if headerOf(mails[1].data, "List-Unsubscribe") != "" {
				t.Errorf("a mail without a link carries the header")
			}
		})
	}
}

// The duyuru lane off sends the old way, from SMTP_FROM: never with the
// header, which is the duyuru@ mailbox's alone.
func TestTheOldPathNeverCarriesListUnsubscribe(t *testing.T) {
	captureMailerLogs(t)
	j := newLaneJobs()
	env := graphEnv("on")
	env["SKYMAIL_SEND_DUYURU"] = "off"
	l := newLaneMailer(t, env, j.keys)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn, mailer.WithSendLanes(mustLanes(t, env)))
	plainSMTP(m)
	mailer.SetGraph(m, l.graph.server.URL+"/v1.0", l.graph.server.Client(), l.tokens)
	mailer.SetTemplateKeys(m, func(context.Context, uuid.UUID) (*string, bool, error) { return ptr("club.news"), false, nil })
	sendEach(t, m, j.withLink)
	if len(relay.taken()) != 1 || len(l.graph.taken()) != 0 {
		t.Fatalf("relay %d, graph %d: want the relay", len(relay.taken()), len(l.graph.taken()))
	}
	if got := headerOf(relay.taken()[0], "List-Unsubscribe"); got != "" {
		t.Errorf("the old path carries List-Unsubscribe %q", got)
	}
}

// A row whose link is no longer one the header can carry (written before the
// check, say) goes without it, as JSON.
func TestARowWithABrokenLinkGoesWithoutTheHeader(t *testing.T) {
	captureMailerLogs(t)
	j := newLaneJobs()
	broken := "https://api.example.org/u?t=a>\r\nBcc: someone@example.net"
	j.withLink.ListUnsubscribeUrl = &broken
	l := newLaneMailer(t, graphEnv("on"), j.keys)
	sendEach(t, l.m, j.withLink)
	calls := l.graph.taken()
	if len(calls) != 1 || calls[0].contentType != "application/json" || strings.Contains(calls[0].raw, "Bcc") {
		t.Fatalf("requests %+v", calls)
	}
}

// A MIME mail over Graph's 4 MB request, base64 counted, is never sent.
func TestAMIMEMailOverGraphsLimitFailsForGood(t *testing.T) {
	captureMailerLogs(t)
	j := newLaneJobs()
	big := "<p>" + strings.Repeat("a", 3<<20) + "</p>"
	j.withLink.BodyHtml = &big
	l := newLaneMailer(t, graphEnv("on"), j.keys)
	err := mailer.SendOnce(context.Background(), l.m, j.withLink)
	if err == nil || !mailer.IsPermanent(err) || !strings.Contains(err.Error(), "4 MB") {
		t.Fatalf("err %v, want the permanent 4 MB refusal", err)
	}
	if len(l.graph.taken()) != 0 {
		t.Errorf("Graph was asked")
	}
}

func TestValidUnsubscribeURL(t *testing.T) {
	for link, want := range map[string]bool{
		coreLink: true,
		"https://api.yildizskylab.com/v1/communication-preferences/unsubscribe?t=AQ-_": true,
		"":                                    false,
		"http://api.example.org/u?t=a":        false,
		"HTTPS://api.example.org/u?t=a":       false,
		"https:///u?t=a":                      false,
		"https://user@api.example.org/u?t=a":  false,
		"https://api.example.org/u?t=a b":     false,
		"https://api.example.org/u?t=a\r\nX:": false,
		"https://api.example.org/u?t=a>":      false,
		"https://api.example.org/u?t=<a":      false,
		"https://api.example.org/u?t=a,b":     false,
		"https://api.example.org/u?t=\"a":     false,
		"https://api.example.org/ü":           false,
		"https://api.example.org/" + strings.Repeat("a", mailer.MaxUnsubscribeURLLength): false,
	} {
		if got := mailer.ValidUnsubscribeURL(link); got != want {
			t.Errorf("%q: %v, want %v", link, got, want)
		}
	}
}

// A recipient's link is rendered as .UnsubscribeUrl, over a send's variable of
// that name; without one the send's variable stays.
func TestTheRecipientsLinkIsRendered(t *testing.T) {
	page := `<a href="{{.UnsubscribeUrl}}">Abonelikten çık</a>`
	got, err := mailer.Render("Konu", "Çık: {{.UnsubscribeUrl}}", page, []byte(`{"UnsubscribeUrl":"https://example.org/genel"}`),
		mailer.RecipientInfo{FullName: "Ayşe", Email: "ayse@example.net", UnsubscribeURL: coreLink})
	if err != nil {
		t.Fatal(err)
	}
	if got.PlainText != "Çık: "+coreLink || !strings.Contains(got.HTML, `href="`+coreLink+`"`) {
		t.Errorf("rendered %+v", got)
	}
	got, err = mailer.Render("Konu", "Çık: {{.UnsubscribeUrl}}", page, []byte(`{"UnsubscribeUrl":"https://example.org/genel"}`),
		mailer.RecipientInfo{FullName: "Ali", Email: "ali@example.net"})
	if err != nil {
		t.Fatal(err)
	}
	if got.PlainText != "Çık: https://example.org/genel" {
		t.Errorf("without a link: %+v", got)
	}
}
