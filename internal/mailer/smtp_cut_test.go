package mailer_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/wneessen/go-mail"
)

// scriptedRelay speaks plain SMTP (no STARTTLS, no AUTH) and stops answering
// at one command, stallOn ("DATA": the mail is never handed over; "QUIT":
// the mail was taken, with 250). It counts the mails it took and notes when
// a stalled connection is dropped.
type scriptedRelay struct {
	listener net.Listener
	stallOn  string

	mu      sync.Mutex
	taken   int
	stalled chan struct{}
	dropped chan struct{}
}

func startScriptedRelay(t *testing.T, stallOn string) *scriptedRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &scriptedRelay{listener: listener, stallOn: stallOn, stalled: make(chan struct{}), dropped: make(chan struct{})}
	var conns sync.WaitGroup
	var open []net.Conn
	var openMu sync.Mutex
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			openMu.Lock()
			open = append(open, conn)
			openMu.Unlock()
			conns.Add(1)
			go func() {
				defer conns.Done()
				r.serve(conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		openMu.Lock()
		for _, conn := range open {
			_ = conn.Close()
		}
		openMu.Unlock()
		conns.Wait()
	})
	return r
}

func (r *scriptedRelay) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	reply := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }
	reply("220 relay.test ESMTP")
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(strings.TrimSpace(line) + " x")[0])
		if verb == r.stallOn {
			close(r.stalled)
			// Says nothing more; returns once the client drops the
			// connection.
			_, _ = io.Copy(io.Discard, reader)
			close(r.dropped)
			return
		}
		switch verb {
		case "EHLO", "HELO":
			reply("250-relay.test")
			reply("250 8BITMIME")
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
			r.mu.Lock()
			r.taken++
			r.mu.Unlock()
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 OK")
		}
	}
}

func (r *scriptedRelay) mailsTaken() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.taken
}

// plainSMTP makes m's workers talk to the relay without STARTTLS or AUTH, and
// bounds a send to 30 s, past every wait in these tests: what ends a send
// here is the shutdown, not the budget.
func plainSMTP(m mailer.Transactional) {
	mailer.SetClientOptions(m, mail.WithTLSPolicy(mail.NoTLS), mail.WithSMTPAuth(mail.SMTPAuthNoAuth))
	mailer.SetSendBudget(m, 30*time.Second)
}

func relayConfig(relay *scriptedRelay) mailer.SMTPConfig {
	return mailer.SMTPConfig{
		FromEmail: "skymail@example.com",
		Host:      "127.0.0.1",
		Port:      relay.listener.Addr().(*net.TCPAddr).Port,
		FQDN:      "example.com",
	}
}

// Shutdown's cut (Start's context ending) ends the SMTP conversation of a
// send in progress, not only its dial: a relay that stalls on DATA gets the
// connection dropped at once, never the mail, and the row keeps its claim
// with no attempt counted, to go again after its lease.
func TestShutdownCutsTheSMTPConversationOfASendInProgress(t *testing.T) {
	logs := captureMailerLogs(t)
	db, ids := pendingQueue(t, 1)
	relay := startScriptedRelay(t, "DATA")
	m := mailer.NewMailer(db, relayConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	ctx, cut := context.WithCancel(context.Background())
	t.Cleanup(cut)
	m.Start(ctx, 1)
	m.Wake()
	select {
	case <-relay.stalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the send did not reach DATA")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := m.Stop(stopCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with a send stalled on DATA: %v, want its deadline", err)
	}
	cut()
	select {
	case <-relay.dropped:
	case <-time.After(3 * time.Second):
		t.Fatal("the cut did not drop the relay connection; the send runs on to its budget")
	}
	select {
	case <-m.Stopped():
	case <-time.After(3 * time.Second):
		t.Fatal("the worker did not stop after the cut")
	}

	if n := relay.mailsTaken(); n != 0 {
		t.Fatalf("the relay took %d mails from a send cut off before DATA", n)
	}
	row := leaseRows(t, db)[ids[0]]
	if row.Status != "processing" || row.ClaimedBy != mailer.Claimant(m) || row.Attempts != 0 {
		t.Fatalf("row cut off by shutdown = %+v, want still claimed with no attempt counted", row)
	}
	found := false
	for _, line := range logs.lines(t) {
		if line["message"] == "Retrying email" || line["message"] == "Giving up on email" {
			t.Fatalf("a send cut off by shutdown was logged as a failure: %v", line)
		}
		if msg, _ := line["message"].(string); strings.HasPrefix(msg, "Send cut off by shutdown") {
			found = true
		}
	}
	if !found {
		t.Fatal("no log line says the send was cut off by shutdown")
	}
}

// A relay that took the mail (250 after DATA) has it: the row is sent even
// when the QUIT after it stalls and shutdown cuts the connection, never left
// processing to go out a second time after the lease.
func TestAMailTheRelayTookIsSentEvenIfQUITIsCutOff(t *testing.T) {
	captureMailerLogs(t)
	db, ids := pendingQueue(t, 1)
	relay := startScriptedRelay(t, "QUIT")
	m := mailer.NewMailer(db, relayConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	ctx, cut := context.WithCancel(context.Background())
	t.Cleanup(cut)
	m.Start(ctx, 1)
	m.Wake()
	select {
	case <-relay.stalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the send did not reach QUIT")
	}
	if n := relay.mailsTaken(); n != 1 {
		t.Fatalf("the relay took %d mails before QUIT, want 1", n)
	}

	cut()
	waitFor(t, "the row to be marked sent", 5*time.Second, func() bool {
		return leaseRows(t, db)[ids[0]].Status == "sent"
	})
	if row := leaseRows(t, db)[ids[0]]; row.Attempts != 0 {
		t.Fatalf("row = %+v, want sent with no failed attempt", row)
	}
}

// The same without a database: cancelling the context a send was given ends
// its SMTP conversation at once, wherever it is (go-mail itself watches the
// context only while dialling).
func TestCancellingASendEndsItsSMTPConversation(t *testing.T) {
	for _, stallOn := range []string{"EHLO", "MAIL", "DATA"} {
		t.Run(stallOn, func(t *testing.T) {
			relay := startScriptedRelay(t, stallOn)
			m := mailer.NewMailer(nil, relayConfig(relay), mailer.SenderOn)
			plainSMTP(m)
			ctx, cancel := context.WithCancel(context.Background())
			sent := make(chan error, 1)
			go func() {
				sent <- mailer.SendOnce(ctx, m, database.MailQueue{
					ID: uuid.New(), RecipientFullName: "Alıcı", RecipientEmail: "alici@example.com",
					Subject: "Konu", Body: "Gövde",
				})
			}()
			select {
			case <-relay.stalled:
			case err := <-sent:
				t.Fatalf("the send ended (%v) before the relay stalled on %s", err, stallOn)
			case <-time.After(10 * time.Second):
				t.Fatalf("the send did not reach %s", stallOn)
			}
			cancel()
			select {
			case err := <-sent:
				if err == nil {
					t.Fatal("a send cut off before the relay took the mail reported success")
				}
			case <-time.After(3 * time.Second):
				t.Fatalf("cancelling did not end a send stalled on %s; it runs on to its budget", stallOn)
			}
			select {
			case <-relay.dropped:
			case <-time.After(3 * time.Second):
				t.Fatal("the relay connection was not dropped")
			}
			if n := relay.mailsTaken(); n != 0 {
				t.Fatalf("the relay took %d mails", n)
			}
		})
	}
}

// A mail the relay took is sent, whatever happens to the QUIT after it: a
// QUIT that fails or is cut off is no failed send (a retry would mail the
// person twice).
func TestASendIsDoneOnceTheRelayTookTheMail(t *testing.T) {
	relay := startScriptedRelay(t, "QUIT")
	m := mailer.NewMailer(nil, relayConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan error, 1)
	go func() {
		sent <- mailer.SendOnce(ctx, m, database.MailQueue{
			ID: uuid.New(), RecipientFullName: "Alıcı", RecipientEmail: "alici@example.com",
			Subject: "Konu", Body: "Gövde",
		})
	}()
	select {
	case <-relay.stalled:
	case <-time.After(10 * time.Second):
		t.Fatal("the send did not reach QUIT")
	}
	cancel()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("a send whose mail the relay took = %v, want success", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a send stalled on QUIT did not end when cancelled")
	}
	if n := relay.mailsTaken(); n != 1 {
		t.Fatalf("the relay took %d mails, want 1", n)
	}
}
