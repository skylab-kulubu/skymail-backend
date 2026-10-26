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

// dsnRelay speaks plain SMTP, offers DSN (or not) in its EHLO and keeps the
// MAIL FROM and RCPT TO lines it was sent.
type dsnRelay struct {
	listener net.Listener
	offerDSN bool

	mu    sync.Mutex
	lines []string
}

func startDSNRelay(t *testing.T, offerDSN bool) *dsnRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &dsnRelay{listener: listener, offerDSN: offerDSN}
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

func (r *dsnRelay) serve(conn net.Conn) {
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
		line = strings.TrimSpace(line)
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		switch verb {
		case "EHLO", "HELO":
			reply("250-relay.test")
			if r.offerDSN {
				reply("250-DSN")
			}
			reply("250 8BITMIME")
		case "MAIL", "RCPT":
			r.mu.Lock()
			r.lines = append(r.lines, line)
			r.mu.Unlock()
			reply("250 OK")
		case "DATA":
			reply("354 go ahead")
			for {
				data, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if data == ".\r\n" {
					break
				}
			}
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func (r *dsnRelay) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

func sendThroughDSNRelay(t *testing.T, relay *dsnRelay, opts ...mailer.Option) []string {
	t.Helper()
	m := mailer.NewMailer(nil, mailer.SMTPConfig{
		FromEmail: "skymail@example.com",
		Host:      "127.0.0.1",
		Port:      relay.listener.Addr().(*net.TCPAddr).Port,
		FQDN:      "example.com",
	}, mailer.SenderOn, opts...)
	plainSMTP(m)
	if err := mailer.SendOnce(context.Background(), m, database.MailQueue{
		ID: uuid.New(), Channel: mailer.ChannelEmail, RecipientFullName: "Alıcı", RecipientEmail: "alici@example.com",
		Subject: "Konu", Body: "Gövde",
	}); err != nil {
		t.Fatal(err)
	}
	return relay.commands()
}

// With the NDR reader on, a mail asks a relay that offers DSN for a report of
// a failure only, with the headers only: RET=HDRS NOTIFY=FAILURE, never
// NOTIFY=SUCCESS (ADR 0073 decision 9). A relay that does not offer DSN is
// asked nothing, and without the reader nothing changes.
func TestTheReaderAsksForAFailureReportWithTheHeadersOnly(t *testing.T) {
	got := sendThroughDSNRelay(t, startDSNRelay(t, true), mailer.WithDSNRequest(true))
	if len(got) != 2 || got[0] != "MAIL FROM:<skymail@example.com> BODY=8BITMIME RET=HDRS" || got[1] != "RCPT TO:<alici@example.com> NOTIFY=FAILURE" {
		t.Errorf("with the reader, a DSN relay was sent %q", got)
	}
	if got := sendThroughDSNRelay(t, startDSNRelay(t, false), mailer.WithDSNRequest(true)); len(got) != 2 || strings.Contains(strings.Join(got, " "), "RET=") || strings.Contains(strings.Join(got, " "), "NOTIFY=") {
		t.Errorf("with the reader, a relay without DSN was sent %q", got)
	}
	if got := sendThroughDSNRelay(t, startDSNRelay(t, true)); len(got) != 2 || strings.Contains(strings.Join(got, " "), "RET=") || strings.Contains(strings.Join(got, " "), "NOTIFY=") {
		t.Errorf("without the reader, a DSN relay was sent %q", got)
	}
}
