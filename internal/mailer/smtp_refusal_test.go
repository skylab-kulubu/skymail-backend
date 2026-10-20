package mailer_test

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// sendThrough sends one mail to the address through a relay that answers verb
// with reply, the way a worker does, and returns the send's error.
func sendThrough(t *testing.T, verb, reply, address string) error {
	t.Helper()
	relay := startAnsweringRelay(t, verb, reply)
	m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	return mailer.SendOnce(context.Background(), m, database.MailQueue{
		ID: uuid.New(), Channel: mailer.ChannelEmail, RecipientFullName: "Alıcı", RecipientEmail: address,
		Subject: "Konu", Body: "Gövde",
	})
}

// A 5xx reply is the relay's final answer (RFC 5321 §4.2.1): the row fails
// for good at once, whatever the stage, and is not tried again. Its error is
// the stage and the reply's codes only: the relay's own text may echo the
// address, and the error is logged and shown.
func TestARelaysPermanentRefusalFailsAtOnce(t *testing.T) {
	for _, refusal := range []struct{ name, verb, reply, want string }{
		{"reserved domain at RCPT", "RCPT", "501 5.1.5 Recipient address reserved by RFC 2606", "smtp_rejected: RCPT TO 501 5.1.5"},
		{"no such user", "RCPT", "550 5.1.1 <alici@example.com>: Recipient address rejected", "smtp_rejected: RCPT TO 550 5.1.1"},
		{"user not local", "RCPT", "551 5.1.6 user not local", "smtp_rejected: RCPT TO 551 5.1.6"},
		{"mailbox name not allowed", "RCPT", "553 5.1.3 bad address syntax", "smtp_rejected: RCPT TO 553 5.1.3"},
		{"no enhanced code", "RCPT", "550 mailbox unavailable", "smtp_rejected: RCPT TO 550"},
		{"sender refused", "MAIL", "550 5.7.1 sender refused", "smtp_rejected: MAIL FROM 550 5.7.1"},
		{"end of DATA", ".", "554 5.7.1 message refused", "smtp_rejected: DATA 554 5.7.1"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			err := sendThrough(t, refusal.verb, refusal.reply, "alici@example.com")
			if err == nil {
				t.Fatal("the refused send succeeded")
			}
			if !mailer.IsPermanent(err) {
				t.Errorf("error %q is retried, want it permanent: a 5xx will be refused again", err)
			}
			if err.Error() != refusal.want {
				t.Errorf("error = %q, want %q", err, refusal.want)
			}
			if strings.Contains(err.Error(), "alici") {
				t.Errorf("error %q carries the address", err)
			}
		})
	}
}

// A 4xx reply is a transient failure (greylisting, a full mailbox, a rate
// limit): the row is tried again later, as before. Its error too is the stage
// and the codes only.
func TestARelaysTransientRefusalIsTriedAgain(t *testing.T) {
	for _, refusal := range []struct{ name, verb, reply, want string }{
		{"greylisted", "RCPT", "450 4.2.1 <alici@example.com>: greylisted", "smtp_deferred: RCPT TO 450 4.2.1"},
		{"service closing", "RCPT", "421 4.7.0 try again later", "smtp_deferred: RCPT TO 421 4.7.0"},
		{"end of DATA", ".", "451 4.3.0 local error", "smtp_deferred: DATA 451 4.3.0"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			err := sendThrough(t, refusal.verb, refusal.reply, "alici@example.com")
			if err == nil {
				t.Fatal("the refused send succeeded")
			}
			if mailer.IsPermanent(err) {
				t.Errorf("error %q fails for good, want it tried again", err)
			}
			if err.Error() != refusal.want {
				t.Errorf("error = %q, want %q", err, refusal.want)
			}
		})
	}
}

// A relay that cannot be reached is the moment, not the address: tried again.
func TestAnUnreachableRelayIsTriedAgain(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	m := mailer.NewMailer(nil, mailer.SMTPConfig{FromEmail: "skymail@example.com", Host: "127.0.0.1", Port: port, FQDN: "example.com"}, mailer.SenderOn)
	plainSMTP(m)
	err = mailer.SendOnce(context.Background(), m, database.MailQueue{
		ID: uuid.New(), Channel: mailer.ChannelEmail, RecipientFullName: "Alıcı", RecipientEmail: "alici@yildizskylab.com",
		Subject: "Konu", Body: "Gövde",
	})
	if err == nil {
		t.Fatal("the send to a closed port succeeded")
	}
	if mailer.IsPermanent(err) {
		t.Errorf("error %q fails for good, want it tried again", err)
	}
}

// The domains RFC 2606 and RFC 6761 reserve never take mail: a recipient there
// is failed when the send is queued and never reaches the relay.
func TestReservedDomain(t *testing.T) {
	for address, want := range map[string]bool{
		"seed-bot@example.com":        true,
		"a@EXAMPLE.ORG":               true,
		"a@example.net":               true,
		"a@mail.example.com":          true,
		"a@example.com.":              true,
		"a@kaya.example":              true,
		"a@relay.test":                true,
		"a@nowhere.invalid":           true,
		"a@localhost":                 true,
		"a@app.localhost":             true,
		"a@yildizskylab.com":          false,
		"a@std.yildiz.edu.tr":         false,
		"a@hotmail.com":               false,
		"a@example.com.tr":            false,
		"a@myexample.com":             false,
		"a@testing.com":               false,
		"a@contest":                   false,
		"no-at-sign":                  false,
		"\"a@example.com\"@gmail.com": false,
	} {
		if got := mailer.ReservedDomain(address); got != want {
			t.Errorf("ReservedDomain(%q) = %v, want %v", address, got, want)
		}
	}
}
