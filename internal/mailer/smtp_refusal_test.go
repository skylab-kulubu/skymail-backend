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

// A refusal of the recipient's address (a 5.1.x to RCPT TO or at the end of
// DATA, or a 550, 551 or 553 without an enhanced code to RCPT TO) fails the
// row for good at once: the address is refused again. Its error is
// the stage and the reply's codes only: the relay's own text may echo the
// address, and the error is logged and shown.
func TestARelaysPermanentRefusalFailsAtOnce(t *testing.T) {
	for _, refusal := range []struct{ name, verb, reply, want string }{
		{"reserved domain at RCPT", "RCPT", "501 5.1.5 Recipient address reserved by RFC 2606", "smtp_rejected: RCPT TO 501 5.1.5"},
		{"no such user", "RCPT", "550 5.1.1 <alici@example.com>: Recipient address rejected", "smtp_rejected: RCPT TO 550 5.1.1"},
		{"user not local", "RCPT", "551 5.1.6 user not local", "smtp_rejected: RCPT TO 551 5.1.6"},
		{"mailbox name not allowed", "RCPT", "553 5.1.3 bad address syntax", "smtp_rejected: RCPT TO 553 5.1.3"},
		{"550 without enhanced code", "RCPT", "550 mailbox unavailable", "smtp_rejected: RCPT TO 550"},
		{"551 without enhanced code", "RCPT", "551 user not local", "smtp_rejected: RCPT TO 551"},
		{"553 without enhanced code", "RCPT", "553 mailbox name not allowed", "smtp_rejected: RCPT TO 553"},
		{"address status at end of DATA", ".", "550 5.1.1 no such user", "smtp_rejected: DATA 550 5.1.1"},
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
// limit), and so is a 5xx that is not about the recipient's address: one to
// MAIL FROM, one to RCPT TO or at the end of DATA that is 5.2.x, 5.3.x or
// 5.7.x (relay access denied, policy), or one without an enhanced code other
// than a 550, 551 or 553 to RCPT TO. Failing those at once would fail a whole list at once;
// the row is tried again later, as before. Its error too is the stage and the
// codes only.
func TestARelaysTransientRefusalIsTriedAgain(t *testing.T) {
	for _, refusal := range []struct{ name, verb, reply, want string }{
		{"greylisted", "RCPT", "450 4.2.1 <alici@example.com>: greylisted", "smtp_deferred: RCPT TO 450 4.2.1"},
		{"service closing", "RCPT", "421 4.7.0 try again later", "smtp_deferred: RCPT TO 421 4.7.0"},
		{"end of DATA 4xx", ".", "451 4.3.0 local error", "smtp_deferred: DATA 451 4.3.0"},
		{"relay access denied at RCPT", "RCPT", "554 5.7.1 <alici@example.com>: Relay access denied", "smtp_deferred: RCPT TO 554 5.7.1"},
		{"policy at RCPT", "RCPT", "550 5.7.1 recipient refused by policy", "smtp_deferred: RCPT TO 550 5.7.1"},
		{"quota at RCPT", "RCPT", "552 5.2.2 mailbox full", "smtp_deferred: RCPT TO 552 5.2.2"},
		{"mail system at RCPT", "RCPT", "550 5.3.0 mail system error", "smtp_deferred: RCPT TO 550 5.3.0"},
		{"554 without enhanced code at RCPT", "RCPT", "554 transaction failed", "smtp_deferred: RCPT TO 554"},
		{"501 without enhanced code at RCPT", "RCPT", "501 syntax error", "smtp_deferred: RCPT TO 501"},
		{"submission quota at MAIL FROM", "MAIL", "554 5.2.0 STOREDRV.Submission.Exception:SubmissionQuotaExceededException", "smtp_deferred: MAIL FROM 554 5.2.0"},
		{"sender refused at MAIL FROM", "MAIL", "550 5.7.1 sender refused", "smtp_deferred: MAIL FROM 550 5.7.1"},
		{"sender address status at MAIL FROM", "MAIL", "553 5.1.8 sender domain does not exist", "smtp_deferred: MAIL FROM 553 5.1.8"},
		{"no enhanced code at MAIL FROM", "MAIL", "550 not allowed", "smtp_deferred: MAIL FROM 550"},
		{"quota at end of DATA", ".", "552 5.2.2 mailbox full", "smtp_deferred: DATA 552 5.2.2"},
		{"mail system at end of DATA", ".", "554 5.3.4 message too big", "smtp_deferred: DATA 554 5.3.4"},
		{"policy at end of DATA", ".", "554 5.7.1 message refused", "smtp_deferred: DATA 554 5.7.1"},
		{"no enhanced code at end of DATA", ".", "554 transaction failed", "smtp_deferred: DATA 554"},
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
