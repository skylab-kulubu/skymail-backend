package mailer_test

import (
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// A mail's stream is its send's purpose (ADR 0072 decision 2), not its queue
// priority: islem goes out of bilgi@ from the bulk lane too, duyuru out of
// duyuru@ from the system lane too. Account mail is hesap whatever its
// purpose; a send without one is read by the rule of before.
func TestLaneOfReadsThePurpose(t *testing.T) {
	islem, duyuru, other := ptr(mailer.PurposeIslem), ptr(mailer.PurposeDuyuru), ptr("ticari")
	for _, tc := range []struct {
		name     string
		purpose  *string
		key      *string
		system   bool
		priority mailer.Priority
		want     mailer.Lane
	}{
		{"an operator's islem send to a Forms source, in the bulk lane", islem, ptr("free.basic"), false, mailer.PriorityBulk, mailer.LaneBilgi},
		{"an islem single mail", islem, ptr("free.basic"), false, mailer.PrioritySystem, mailer.LaneBilgi},
		{"a duyuru send to a list", duyuru, ptr("free.basic"), false, mailer.PriorityBulk, mailer.LaneDuyuru},
		{"a duyuru single mail in the system lane", duyuru, ptr("free.basic"), false, mailer.PrioritySystem, mailer.LaneDuyuru},
		{"account mail named duyuru", duyuru, ptr("keycloak.login-code"), true, mailer.PriorityBulk, mailer.LaneHesap},
		{"account mail named islem", islem, ptr("account.verify-email"), true, mailer.PrioritySystem, mailer.LaneHesap},
		{"no purpose, the bulk lane", nil, ptr("free.basic"), false, mailer.PriorityBulk, mailer.LaneDuyuru},
		{"no purpose, the system lane", nil, ptr("forms.submission-received"), true, mailer.PrioritySystem, mailer.LaneBilgi},
		{"a value the database refuses reads as none", other, nil, false, mailer.PrioritySystem, mailer.LaneBilgi},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mailer.LaneOf(tc.purpose, tc.key, tc.system, tc.priority); got != tc.want {
				t.Errorf("lane %q, want %q", got, tc.want)
			}
		})
	}
}

// SKYMAIL_MAIL_PURPOSE is off unless it says on; anything else stops startup.
func TestTheMailPurposeSwitch(t *testing.T) {
	for value, want := range map[string]bool{"": false, "off": false, "OFF": false, "on": true, " On ": true} {
		lanes, err := mailer.SendLanesFromEnv(getenv(map[string]string{"SKYMAIL_MAIL_PURPOSE": value}))
		if err != nil || lanes.Purpose != want {
			t.Errorf("%q: purpose %v (%v), want %v", value, lanes.Purpose, err, want)
		}
	}
	if _, err := mailer.SendLanesFromEnv(getenv(map[string]string{"SKYMAIL_MAIL_PURPOSE": "yes"})); err == nil || !strings.Contains(err.Error(), "SKYMAIL_MAIL_PURPOSE") {
		t.Errorf("yes: %v, want startup stopped", err)
	}
}

// purposeLanes is newLaneMailer whose sends have the purposes given.
func purposeLanes(t *testing.T, env map[string]string, sends map[uuid.UUID]*string) *laneMailer {
	t.Helper()
	l := newLaneMailer(t, env, nil)
	mailer.SetSendLanes(l.m, func(_ context.Context, task uuid.UUID) (*string, bool, *string, error) {
		purpose, ok := sends[task]
		if !ok {
			t.Errorf("the send %s was read", task)
		}
		return ptr("free.basic"), false, purpose, nil
	})
	return l
}

// purposeJob is a mail of task in priority's lane, with core's link.
func purposeJob(task uuid.UUID, priority mailer.Priority, htmlBody *string) database.MailQueue {
	link := coreLink
	return database.MailQueue{ID: uuid.New(), TaskID: task, Channel: mailer.ChannelEmail, Priority: int16(priority),
		RecipientFullName: "Ayşe Yılmaz", RecipientEmail: "ayse@example.net", Subject: "Başvurun hakkında",
		Body: "Görüşme yerin değişti.", BodyHtml: htmlBody, ListUnsubscribeUrl: &link}
}

// With SKYMAIL_MAIL_PURPOSE on, an operator's islem send waits in the bulk
// lane and goes out of bilgi@, as JSON, without List-Unsubscribe or the
// visible line, though its row carries a link; a duyuru single mail in the
// system lane goes out of duyuru@ with both. Off, the purpose is not read:
// the bulk lane is duyuru@ as before.
func TestThePurposePicksTheMailbox(t *testing.T) {
	captureMailerLogs(t)
	islemTask, duyuruTask := uuid.New(), uuid.New()
	sends := map[uuid.UUID]*string{islemTask: ptr(mailer.PurposeIslem), duyuruTask: ptr(mailer.PurposeDuyuru)}
	env := graphEnv("on")
	env["SKYMAIL_MAIL_PURPOSE"] = "on"
	l := purposeLanes(t, env, sends)
	sendEach(t, l.m, purposeJob(islemTask, mailer.PriorityBulk, html("<p>Görüşme yerin değişti.</p>")),
		purposeJob(duyuruTask, mailer.PrioritySystem, html("<p>Ekim duyurusu</p>")))
	calls := l.graph.taken()
	if len(calls) != 2 {
		t.Fatalf("Graph took %d requests, want 2", len(calls))
	}
	if islem := calls[0]; islem.path != "/v1.0/users/bilgi@bildirim.example.org/sendMail" || islem.contentType != "application/json" ||
		strings.Contains(islem.raw, "unsubscribe") || strings.Contains(islem.raw, "Abonelikten") {
		t.Errorf("islem: %s %s %s; want bilgi@, JSON, no link", islem.path, islem.contentType, islem.raw)
	}
	duyuru := calls[1]
	if duyuru.path != "/v1.0/users/duyuru@bildirim.example.org/sendMail" || duyuru.contentType != "text/plain" {
		t.Fatalf("duyuru: %s %s; want duyuru@ as MIME", duyuru.path, duyuru.contentType)
	}
	msg, err := mail.ReadMessage(strings.NewReader(duyuru.mime))
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Header.Get("List-Unsubscribe"); got != "<"+coreLink+">" {
		t.Errorf("duyuru List-Unsubscribe %q", got)
	}
	plain, htmlPart := textParts(t, duyuru.mime)
	if !strings.Contains(plain, "SKY LAB duyurularını almak istemiyorsan: Abonelikten çık <"+coreLink+">") ||
		!strings.Contains(htmlPart, `<a href="`+coreLink+`"`) {
		t.Errorf("duyuru without the visible line:\nplain %q\nhtml %q", plain, htmlPart)
	}

	off := graphEnv("on")
	l = purposeLanes(t, off, nil)
	mailer.SetTemplateKeys(l.m, func(context.Context, uuid.UUID) (*string, bool, error) { return ptr("free.basic"), false, nil })
	sendEach(t, l.m, purposeJob(islemTask, mailer.PriorityBulk, html("<p>Görüşme yerin değişti.</p>")))
	if calls := l.graph.taken(); len(calls) != 1 || calls[0].path != "/v1.0/users/duyuru@bildirim.example.org/sendMail" ||
		strings.Contains(calls[0].mime, "Abonelikten") {
		t.Errorf("off: %+v; want duyuru@ as before, no visible line", calls)
	}
}

// The visible line: added to the HTML and to the plain text of a duyuru mail
// whose row has a link, each part only when it lacks the link (a template
// writing {{.UnsubscribeUrl}} gets nothing), before </body>; never with the
// switch off, never on a mail without a link.
func TestTheVisibleUnsubscribeLine(t *testing.T) {
	captureMailerLogs(t)
	task := uuid.New()
	env := graphEnv("off")
	env["SKYMAIL_MAIL_PURPOSE"] = "on"
	withTemplateLink := "<html><body><p>Ekim</p><a href=\"" + coreLink + "\">Çık</a></body></html>"
	l := purposeLanes(t, env, map[uuid.UUID]*string{task: ptr(mailer.PurposeDuyuru)})
	plainOnly := purposeJob(task, mailer.PriorityBulk, nil)
	noLink := purposeJob(task, mailer.PriorityBulk, html("<p>Ekim</p>"))
	noLink.ListUnsubscribeUrl = nil
	sendEach(t, l.m, purposeJob(task, mailer.PriorityBulk, html("<html><BODY><p>Ekim</p></BODY></html>")),
		purposeJob(task, mailer.PriorityBulk, &withTemplateLink), plainOnly, noLink)
	calls := l.graph.taken()
	if len(calls) != 4 {
		t.Fatalf("Graph took %d requests", len(calls))
	}
	content := func(i int) string {
		message, _ := calls[i].body["message"].(map[string]any)
		body, _ := message["body"].(map[string]any)
		s, _ := body["content"].(string)
		return s
	}
	line := `<p style="margin:24px 0 0;font-size:12px;line-height:18px;color:#6b7280">SKY LAB duyurularını almak istemiyorsan: <a href="` + coreLink + `" style="color:#6b7280">Abonelikten çık</a></p>`
	if got, want := content(0), "<html><BODY><p>Ekim</p>"+line+"</BODY></html>"; got != want {
		t.Errorf("HTML:\n%s\nwant\n%s", got, want)
	}
	if got := content(1); got != withTemplateLink {
		t.Errorf("a template with the link got %s", got)
	}
	if got, want := content(2), "Görüşme yerin değişti.\n\nSKY LAB duyurularını almak istemiyorsan: Abonelikten çık <"+coreLink+">\n"; got != want {
		t.Errorf("plain text %q, want %q", got, want)
	}
	if got := content(3); strings.Contains(got, "Abonelikten") {
		t.Errorf("a mail without a link got the line: %s", got)
	}

	env["SKYMAIL_MAIL_PURPOSE"] = "off"
	l = purposeLanes(t, env, nil)
	mailer.SetTemplateKeys(l.m, func(context.Context, uuid.UUID) (*string, bool, error) { return ptr("free.basic"), false, nil })
	sendEach(t, l.m, purposeJob(task, mailer.PriorityBulk, html("<p>Ekim</p>")))
	if got := l.graph.taken()[0].raw; strings.Contains(got, "Abonelikten") {
		t.Errorf("the switch off and the mail got the line: %s", got)
	}
}

// With every sender lane off (the old path, SMTP_FROM), a duyuru mail's
// stream is still read for the line, and the line goes in both parts.
func TestTheVisibleLineOnTheOldPath(t *testing.T) {
	captureMailerLogs(t)
	task := uuid.New()
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn, mailer.WithSendLanes(mailer.SendLanes{Purpose: true}))
	plainSMTP(m)
	reads := 0
	mailer.SetSendLanes(m, func(context.Context, uuid.UUID) (*string, bool, *string, error) {
		reads++
		return ptr("free.basic"), false, ptr(mailer.PurposeDuyuru), nil
	})
	sendEach(t, m, purposeJob(task, mailer.PriorityBulk, html("<p>Ekim</p>")))
	if len(relay.taken()) != 1 || reads != 1 {
		t.Fatalf("relay %d mails, the send read %d times", len(relay.taken()), reads)
	}
	plain, htmlPart := textParts(t, relay.taken()[0])
	if !strings.Contains(plain, "Abonelikten çık <"+coreLink+">") || !strings.Contains(htmlPart, "Abonelikten çık</a></p>") {
		t.Errorf("plain %q html %q", plain, htmlPart)
	}
}

// The footer functions alone: the link as html/template writes it into an
// attribute (& escaped) counts as present; a body ending in blank lines gets
// one blank line before the line.
func TestTheFooterFindsTheLinkAsATemplateWritesIt(t *testing.T) {
	link := "https://x.example.org/u/a?b=1&c=2"
	if got := mailer.HTMLWithUnsubscribeFooter(`<a href="https://x.example.org/u/a?b=1&amp;c=2">x</a>`, link); strings.Contains(got, "Abonelikten") {
		t.Errorf("the escaped link was not found: %s", got)
	}
	if got := mailer.HTMLWithUnsubscribeFooter("<p>x</p>", link); !strings.HasSuffix(got, `<a href="https://x.example.org/u/a?b=1&amp;c=2" style="color:#6b7280">Abonelikten çık</a></p>`) {
		t.Errorf("no body tag: %s", got)
	}
	if got, want := mailer.PlainWithUnsubscribeFooter("Metin\n\n\n", link), "Metin\n\nSKY LAB duyurularını almak istemiyorsan: Abonelikten çık <"+link+">\n"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
	if got := mailer.PlainWithUnsubscribeFooter("", link); got != "SKY LAB duyurularını almak istemiyorsan: Abonelikten çık <"+link+">\n" {
		t.Errorf("empty body: %q", got)
	}
}

// textParts decodes a mail's text/plain and text/html parts.
func textParts(t *testing.T, raw string) (plain, htmlPart string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("not a mail: %v", err)
	}
	var walk func(contentType, encoding string, body io.Reader)
	walk = func(contentType, encoding string, body io.Reader) {
		media, params, err := mime.ParseMediaType(contentType)
		if err != nil {
			t.Fatalf("content type %q: %v", contentType, err)
		}
		if strings.HasPrefix(media, "multipart/") {
			parts := multipart.NewReader(body, params["boundary"])
			for {
				part, err := parts.NextRawPart()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				walk(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part)
			}
		}
		switch strings.ToLower(encoding) {
		case "quoted-printable":
			body = quotedprintable.NewReader(body)
		case "base64":
			body = base64.NewDecoder(base64.StdEncoding, body)
		}
		data, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		switch media {
		case "text/plain":
			plain = string(data)
		case "text/html":
			htmlPart = string(data)
		}
	}
	walk(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
	return plain, htmlPart
}

// The line goes before </body> in the original bytes: a Turkish İ (two bytes,
// one when lower-cased), a Kelvin sign K (three bytes, one lower-cased) or
// an Ⱥ (two bytes, three lower-cased) before it never shifts where it goes,
// and the tag is found in any ASCII case.
func TestTheVisibleLineGoesBeforeTheBodyTagInTurkishMail(t *testing.T) {
	link := "https://x.example.org/u/a"
	line := `<p style="margin:24px 0 0;font-size:12px;line-height:18px;color:#6b7280">SKY LAB duyurularını almak istemiyorsan: <a href="` + link + `" style="color:#6b7280">Abonelikten çık</a></p>`
	for _, head := range []string{
		"<html><body><p>İyi günler, İLETİŞİM için bize yazın. Şimdi ğ ı ş ü ö ç.</p></div>",
		"<html><BODY><p>\u212a KELVİN İİİ Ⱥ</p>",
		"<html><Body><p>ıİşğ</p>",
	} {
		for _, tag := range []string{"</body>", "</BODY>", "</Body >"} {
			body := head + tag + "</html>"
			if got, want := mailer.HTMLWithUnsubscribeFooter(body, link), head+line+tag+"</html>"; got != want {
				t.Errorf("%q:\n got %q\nwant %q", body, got, want)
			}
		}
	}
	// Only the last </body> counts; one inside an earlier part does not.
	body := "<p>İ</body></p><div>İ</div></body>"
	if got, want := mailer.HTMLWithUnsubscribeFooter(body, link), "<p>İ</body></p><div>İ</div>"+line+"</body>"; got != want {
		t.Errorf("last tag:\n got %q\nwant %q", got, want)
	}
}
