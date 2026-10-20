package mailer_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// answeringRelay speaks plain SMTP and answers one command with a reply of
// its own (reject, say "RCPT" with "550 5.1.1 no such user"; "." is the end
// of DATA). It keeps the mails it took, headers and all.
type answeringRelay struct {
	listener net.Listener
	reject   string
	with     string

	mu    sync.Mutex
	mails []string
}

func startAnsweringRelay(t *testing.T, reject, with string) *answeringRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &answeringRelay{listener: listener, reject: reject, with: with}
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

func (r *answeringRelay) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	reply := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }
	reply("220 relay.test ESMTP")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(strings.TrimSpace(line) + " x")[0])
		switch {
		case verb == r.reject:
			reply(r.with)
		case verb == "EHLO" || verb == "HELO":
			reply("250-relay.test")
			reply("250 8BITMIME")
		case verb == "DATA":
			reply("354 go ahead")
			var mail strings.Builder
			for {
				data, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if data == ".\r\n" {
					break
				}
				mail.WriteString(data)
			}
			if r.reject == "." {
				reply(r.with)
				continue
			}
			r.mu.Lock()
			r.mails = append(r.mails, mail.String())
			r.mu.Unlock()
			reply("250 queued")
		case verb == "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func (r *answeringRelay) taken() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.mails...)
}

func answeringConfig(r *answeringRelay) mailer.SMTPConfig {
	return mailer.SMTPConfig{
		FromEmail: "skymail@example.com",
		Host:      "127.0.0.1",
		Port:      r.listener.Addr().(*net.TCPAddr).Port,
		FQDN:      "example.com",
	}
}

// A mail carries Message-ID <row id@FQDN>: the row's id, so a relay's
// bounce or a reply names the row it came from.
func TestAnEmailCarriesTheRowsMessageID(t *testing.T) {
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	id := uuid.New()
	if err := mailer.SendOnce(context.Background(), m, database.MailQueue{
		ID: id, Channel: mailer.ChannelEmail, RecipientFullName: "Alıcı", RecipientEmail: "alici@example.com",
		Subject: "Konu", Body: "Gövde",
	}); err != nil {
		t.Fatal(err)
	}
	mails := relay.taken()
	if len(mails) != 1 {
		t.Fatalf("the relay took %d mails, want 1", len(mails))
	}
	want := "Message-ID: <" + id.String() + "@example.com>"
	for _, line := range strings.Split(mails[0], "\r\n") {
		if line == want {
			return
		}
	}
	t.Fatalf("the mail has no %q header:\n%s", want, mails[0])
}

// A relay's refusal is written as the worker meets it: a refusal of the
// recipient's address (a 5.1.x to RCPT TO or at the end of DATA) fails the row
// for good after that one attempt, with the stage and the reply's codes as its
// error; a 4xx, or a 5xx that is not about the address (MAIL FROM, a 5.7.x to
// RCPT TO or at the end of DATA), puts it back to pending with the attempt
// counted, to be tried again.
func TestARelaysRefusalIsWrittenByItsReply(t *testing.T) {
	for _, refusal := range []struct{ name, verb, reply, status, error string }{
		{"RCPT 5xx", "RCPT", "550 5.1.1 no such user", "failed", "smtp_rejected: RCPT TO 550 5.1.1"},
		{"reserved domain at RCPT", "RCPT", "501 5.1.5 Recipient address reserved by RFC 2606", "failed", "smtp_rejected: RCPT TO 501 5.1.5"},
		{"end of DATA 5.1.x", ".", "550 5.1.1 no such user", "failed", "smtp_rejected: DATA 550 5.1.1"},
		{"RCPT 4xx", "RCPT", "450 4.2.1 greylisted", "pending", "smtp_deferred: RCPT TO 450 4.2.1"},
		{"RCPT 5.7.x", "RCPT", "554 5.7.1 relay access denied", "pending", "smtp_deferred: RCPT TO 554 5.7.1"},
		{"MAIL FROM 5xx", "MAIL", "554 5.2.0 submission quota exceeded", "pending", "smtp_deferred: MAIL FROM 554 5.2.0"},
		{"end of DATA 5.7.x", ".", "554 5.7.1 message refused", "pending", "smtp_deferred: DATA 554 5.7.1"},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			captureMailerLogs(t)
			db, ids := pendingQueue(t, 1)
			relay := startAnsweringRelay(t, refusal.verb, refusal.reply)
			m := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn)
			plainSMTP(m)
			startMailer(t, m, 1)
			m.Wake()

			var row channelRow
			waitFor(t, "the refused send's outcome", 10*time.Second, func() bool {
				row = channelRowOf(t, db, ids[0])
				return row.attempts > 0
			})
			if row.status != refusal.status || row.attempts != 1 || row.error != refusal.error {
				t.Fatalf("the refused row = %+v, want %s after one attempt, with %q", row, refusal.status, refusal.error)
			}
		})
	}
}
