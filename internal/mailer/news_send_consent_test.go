package mailer_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// A News send (POST /v1/news_sends, duyuru-haber 06) is a duyuru like every
// other under the announcement consent rule (kvkk-s6 23): an address without
// an account gets it only with its announcement consent. Its recipients come
// from core's audience as accounts (recipient_sub set), so none of them waits
// on core; a row of it to an address without an account would be held as any
// announcement is.
func TestANewsSendIsUnderTheAnnouncementConsentRule(t *testing.T) {
	captureMailerLogs(t)
	db := suppressionQueue(t)
	core := announcementCore()
	m := gatedMailer(t, db, mailer.ConsentGate{ListConsent: true, Core: core}, purposesOn())

	purpose, category, absent := mailer.PurposeDuyuru, "club:haber", true
	ref := "news:kulup-toplantisi"
	member := uuid.New()
	// Names start with an ASCII letter: initialTemplate writes the name's first byte.
	task, err := m.EnqueueWithRecipients(context.Background(), mailer.EnqueueWithRecipientsParams{
		SentBy: laneSender, TemplateID: initialTemplate, BodyVariables: []byte(`{}`), SourceRef: &ref,
		Purpose: mailer.SendPurpose{Purpose: &purpose, Category: &category, CommercialContentAbsent: &absent},
		Recipients: []mailer.RecipientInfo{
			{FullName: "Uye", Email: "misafir@example.com", Sub: &member},
			{FullName: "Hesapsiz", Email: "eski@example.com"},
			{FullName: "Rizali", Email: "rizali@example.com"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if taken := gatedTurn(t, m); len(taken) != 2 {
		t.Errorf("taken %d, want the member's and the consented address's", len(taken))
	}
	for _, email := range []string{"misafir@example.com", "rizali@example.com"} {
		if got := rowTo(t, db, task, email); got.error != "" || got.status == "failed" {
			t.Errorf("%s: %+v, want it sent", email, got)
		}
	}
	if got := rowTo(t, db, task, "eski@example.com"); got.status != "failed" || got.error != "suppressed: no_announcement_consent" || got.attempts != 0 {
		t.Errorf("the address without an account or a current consent: %+v, want failed with suppressed: no_announcement_consent", got)
	}
	for _, call := range core.calls() {
		if strings.Contains(call, "misafir@example.com") {
			t.Errorf("core was asked about the member's row, which names its account: %q", call)
		}
	}
}
