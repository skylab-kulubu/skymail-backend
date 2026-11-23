package handlers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

// The Idempotency-Key's request digest of a mail without attachments is
// what it was before attachments came (#133, f5c288b): keys stored by the
// image before answer their retries alike after a release. The values were
// computed with that commit's code. A request with attachments digests as
// the same request without them.
func TestTheRequestDigestIsWhatItWasBeforeAttachments(t *testing.T) {
	single := requests.SendSingleMail{TemplateKey: "event.ticket-created", RecipientEmail: "ayse@example.com", RecipientFullName: "Ayşe Kaya",
		BodyVariables: map[string]interface{}{"EventName": "Kodlama Gecesi", "TicketCode": "K7M2-QX9F"}}
	expires := time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC)
	allowed := []string{"email"}
	notification := requests.SendNotification{Type: "event.ticket-created",
		Recipients: []requests.NotificationRecipient{{Sub: uuid.MustParse("31ef736f-72da-4a40-8791-d523199cf9f0"), AllowedChannels: &allowed}},
		Variables:  map[string]interface{}{"EventName": "Kodlama Gecesi"}, Source: &requests.NotificationSource{Kind: "ticket", Ref: "t-1"},
		ExpiresAt: &expires}
	const (
		singleBefore       = "4a732dd4b5652909854dbcae16de46f3f99d133e4453b70752f68fc6dfd56b1f"
		notificationBefore = "74ac4ca59693fd4553c07f4ebbbf66d3798ccbe01783568e1d35ec8f2ec3e403"
	)
	pdf := []requests.MailAttachment{{Filename: "bilet.pdf", ContentType: "application/pdf", ContentBase64: "JVBERi0xLjcK"}}

	for _, c := range []struct {
		name string
		got  func() (string, error)
		want string
	}{
		{"a single mail", func() (string, error) { return singleRequestSHA256(single) }, singleBefore},
		{"a single mail with a PDF", func() (string, error) {
			with := single
			with.Attachments = pdf
			return singleRequestSHA256(with)
		}, singleBefore},
		{"a notification", func() (string, error) { return requestSHA256(notification) }, notificationBefore},
		{"a notification with a PDF", func() (string, error) {
			with := notification
			with.Attachments = pdf
			return requestSHA256(with)
		}, notificationBefore},
	} {
		got, err := c.got()
		if err != nil || got != c.want {
			t.Errorf("%s: digest %s (%v), want %s", c.name, got, err, c.want)
		}
	}
}
