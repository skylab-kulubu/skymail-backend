package dsn_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/dsn"
)

// The fixtures are synthetic: shaped like Exchange Online's and Postfix's
// reports, with addresses under example domains. A real report from the
// sender mailbox's trial (hub ticket 02) is added, its addresses masked, when
// there is one.
const fqdn = "mail.example.org"

func parseFile(t *testing.T, name string) (*dsn.Report, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return dsn.Parse(bytes.NewReader(raw))
}

func TestParseReadsTheMachinePartOfAReport(t *testing.T) {
	for _, tc := range []struct {
		file       string
		queueID    string
		recipients []dsn.Recipient
		kinds      []dsn.Kind
	}{
		{
			// Exchange Online's own report: the original headers come back as
			// text/rfc822-headers. The human part (in Turkish, and an HTML
			// copy naming another address and code) is not read.
			file:    "exchange-recipient-not-found.eml",
			queueID: "6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b",
			recipients: []dsn.Recipient{{
				FinalRecipient: "yok@std.example.org", OriginalRecipient: "yok@std.example.org",
				Action: "failed", Status: "5.1.10", SMTPCode: 550,
			}},
			kinds: []dsn.Kind{dsn.KindHard},
		},
		{
			// Directory-Based Edge Blocking: 5.4.1 with AS(201806281), the
			// machine part base64-encoded, the original returned whole with
			// its domain in capitals.
			file:    "exchange-edge-blocking.eml",
			queueID: "0b3e6c1a-2d4f-4a7b-8c9d-e0f1a2b3c4d5",
			recipients: []dsn.Recipient{{
				FinalRecipient: "mezun@std.example.org", Action: "failed", Status: "5.4.1", SMTPCode: 550,
				ASCode: "201806281",
			}},
			kinds: []dsn.Kind{dsn.KindHard},
		},
		{
			file:    "exchange-delayed.eml",
			queueID: "2a7f0e4c-91b3-4d6e-a5f8-3c2b1a0d9e87",
			recipients: []dsn.Recipient{{
				FinalRecipient: "uye@uzak.example.net", Action: "delayed", Status: "4.4.7", SMTPCode: 400,
			}},
			kinds: []dsn.Kind{dsn.KindDelay},
		},
		{
			// A remote Postfix's report, sent later: mailbox full is not the
			// address's fault. Its Diagnostic-Code is not of type smtp.
			file:    "remote-mailbox-full.eml",
			queueID: "9c8b7a6d-5e4f-4321-8fed-cba987654321",
			recipients: []dsn.Recipient{{
				FinalRecipient: "dolu@uzak.example.net", OriginalRecipient: "dolu@uzak.example.net",
				Action: "failed", Status: "5.2.2",
			}},
			kinds: []dsn.Kind{dsn.KindSoft},
		},
		{
			// No headers returned: the report's In-Reply-To names the row.
			// Status only says the class; the Diagnostic-Code's 5.1.1 is the
			// code. A delivered recipient is in the report too.
			file:    "exchange-several-recipients.eml",
			queueID: "d4c3b2a1-0f9e-4d8c-b7a6-a5b4c3d2e1f0",
			recipients: []dsn.Recipient{
				{FinalRecipient: "teslim@uzak.example.net", Action: "delivered", Status: "2.0.0"},
				{
					FinalRecipient: "yonlendirme@uzak.example.net", OriginalRecipient: "takma-ad@std.example.org",
					Action: "failed", Status: "5.1.1", SMTPCode: 550,
				},
			},
			kinds: []dsn.Kind{dsn.KindNone, dsn.KindHard},
		},
	} {
		t.Run(tc.file, func(t *testing.T) {
			report, err := parseFile(t, tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(report.Recipients, tc.recipients) {
				t.Errorf("recipients = %+v\nwant         %+v", report.Recipients, tc.recipients)
			}
			id, ok := report.QueueID(fqdn)
			if !ok || id != uuid.MustParse(tc.queueID) {
				t.Errorf("queue id = %v %v, want %s", id, ok, tc.queueID)
			}
			for i, want := range tc.kinds {
				if i < len(report.Recipients) {
					if got := dsn.Classify(report.Recipients[i]); got != want {
						t.Errorf("recipient %d: kind %q, want %q", i, got, want)
					}
				}
			}
		})
	}
}

// What is not a delivery status notification is not read: an auto-reply
// that quotes report fields in its text, and a remote system's plain-text
// bounce (the deviation the hub spec names: no heuristic parsing).
func TestParseRefusesWhatIsNotAReport(t *testing.T) {
	for _, file := range []string{"auto-reply.eml", "plain-text-bounce.eml"} {
		t.Run(file, func(t *testing.T) {
			if _, err := parseFile(t, file); !errors.Is(err, dsn.ErrNotDSN) {
				t.Errorf("err = %v, want ErrNotDSN", err)
			}
		})
	}
	t.Run("a report without its machine part", func(t *testing.T) {
		raw := "Content-Type: multipart/report; report-type=delivery-status; boundary=b\n\n--b\nContent-Type: text/plain\n\nx\n--b--\n"
		if _, err := dsn.Parse(strings.NewReader(raw)); !errors.Is(err, dsn.ErrNotDSN) {
			t.Errorf("err = %v, want ErrNotDSN", err)
		}
	})
	t.Run("another report type", func(t *testing.T) {
		raw := "Content-Type: multipart/report; report-type=disposition-notification; boundary=b\n\n--b\nContent-Type: message/disposition-notification\n\nDisposition: manual-action/MDN-sent-manually; displayed\n--b--\n"
		if _, err := dsn.Parse(strings.NewReader(raw)); !errors.Is(err, dsn.ErrNotDSN) {
			t.Errorf("err = %v, want ErrNotDSN", err)
		}
	})
}

// A report about a mail SkyMail did not send names no queue row: the
// returned headers decide, even when the report's In-Reply-To names one.
func TestQueueIDNeedsSkyMailsMessageID(t *testing.T) {
	report, err := parseFile(t, "rotator-alarm.eml")
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := report.QueueID(fqdn); ok {
		t.Errorf("queue id %v from the rotator's mail, want none", id)
	}

	for _, tc := range []struct {
		name, header string
		ok           bool
	}{
		{"SkyMail's", "<6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b@mail.example.org>", true},
		{"domain in capitals", "<6F1D2C3B-4A59-4E8F-9B7A-0C1D2E3F4A5B@Mail.Example.Org>", true},
		{"another domain", "<6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b@mail.example.org.evil.example>", false},
		{"a subdomain", "<6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b@x.mail.example.org>", false},
		{"not a uuid", "<20261020.12345@mail.example.org>", false},
		{"a uuid in another form", "<6f1d2c3b4a594e8f9b7a0c1d2e3f4a5b@mail.example.org>", false},
		{"a urn", "<urn:uuid:6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b@mail.example.org>", false},
		{"no brackets", "6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b@mail.example.org", false},
		{"References, ours last", "<x@y.example> <6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b@mail.example.org>", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := dsn.Report{OriginalMessageID: tc.header}
			if _, ok := r.QueueID(fqdn); ok != tc.ok {
				t.Errorf("ok = %v, want %v", ok, tc.ok)
			}
		})
	}
	if _, ok := (&dsn.Report{InReplyTo: "<6f1d2c3b-4a59-4e8f-9b7a-0c1d2e3f4a5b@mail.example.org>"}).QueueID(""); ok {
		t.Error("an empty SMTP_FQDN matched")
	}
}

// Line endings are CRLF on the wire; the files are LF. Both read the same.
func TestParseReadsCRLF(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "exchange-recipient-not-found.eml"))
	if err != nil {
		t.Fatal(err)
	}
	crlf := bytes.ReplaceAll(raw, []byte("\n"), []byte("\r\n"))
	report, err := dsn.Parse(bytes.NewReader(crlf))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recipients) != 1 || report.Recipients[0].Status != "5.1.10" || report.Recipients[0].FinalRecipient != "yok@std.example.org" {
		t.Errorf("recipients = %+v", report.Recipients)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		r    dsn.Recipient
		want dsn.Kind
	}{
		{dsn.Recipient{Action: "failed", Status: "5.1.1"}, dsn.KindHard},
		{dsn.Recipient{Action: "failed", Status: "5.1.0"}, dsn.KindHard},
		{dsn.Recipient{Action: "failed", Status: "5.1.6"}, dsn.KindHard},
		{dsn.Recipient{Action: "failed", Status: "5.1.10"}, dsn.KindHard},
		{dsn.Recipient{Action: "failed", Status: "5.4.1", ASCode: "201806281"}, dsn.KindHard},
		// The sender's side, not the address.
		{dsn.Recipient{Action: "failed", Status: "5.1.7"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "5.1.8"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "5.1.90"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "5.4.1"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "5.4.1", ASCode: "201806282"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "5.2.2"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "5.7.509"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "5.7.26"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "5.4.7"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed", Status: "4.4.7"}, dsn.KindSoft},
		{dsn.Recipient{Action: "failed"}, dsn.KindSoft},
		{dsn.Recipient{Action: "delayed", Status: "4.4.7"}, dsn.KindDelay},
		{dsn.Recipient{Action: "delayed", Status: "5.1.1"}, dsn.KindDelay},
		{dsn.Recipient{Action: "delivered", Status: "2.0.0"}, dsn.KindNone},
		{dsn.Recipient{Action: "relayed", Status: "2.0.0"}, dsn.KindNone},
		{dsn.Recipient{Action: "expanded", Status: "2.0.0"}, dsn.KindNone},
		{dsn.Recipient{Status: "5.1.1"}, dsn.KindNone},
	} {
		if got := dsn.Classify(tc.r); got != tc.want {
			t.Errorf("Classify(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

// Malformed reports fail without a panic, and fields that are not what
// they claim are left empty.
func TestParseIsStrictAboutFields(t *testing.T) {
	raw := strings.Join([]string{
		"Content-Type: multipart/report; report-type=delivery-status; boundary=b",
		"",
		"--b",
		"Content-Type: message/delivery-status",
		"",
		"Reporting-MTA: dns;mx.example.net",
		"",
		"Final-Recipient: x400;c=tr;a=x",
		"Original-Recipient: rfc822;bir iki@example.net",
		"Action: FAILED",
		"Status: 5.1.1 (bad destination mailbox address)",
		"Diagnostic-Code: X-Unix; 550 5.1.2 something AS(1)",
		"",
		"Final-Recipient: rfc822;<a@example.net>",
		"Action: failed",
		"Status: five",
		"Diagnostic-Code: smtp; 550 5.1.3 bad",
		"",
		"--b--",
	}, "\n")
	report, err := dsn.Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	want := []dsn.Recipient{
		{Action: "failed", Status: "5.1.1"},
		{FinalRecipient: "a@example.net", Action: "failed", Status: "5.1.3", SMTPCode: 550},
	}
	if !reflect.DeepEqual(report.Recipients, want) {
		t.Errorf("recipients = %+v\nwant         %+v", report.Recipients, want)
	}

	for _, broken := range []string{
		"",
		"Content-Type: multipart/report; report-type=delivery-status\n\n--b\n",
		"Content-Type: multipart/report; report-type=delivery-status; boundary=b\n\n--b\nContent-Type: message/delivery-status\n\n" +
			strings.Repeat("X-Long: "+strings.Repeat("a", 1000)+"\n", 400) + "--b--\n",
	} {
		if _, err := dsn.Parse(strings.NewReader(broken)); err == nil {
			t.Errorf("Parse(%.60q…) succeeded", broken)
		}
	}
}
