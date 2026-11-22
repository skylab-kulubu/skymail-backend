package mailer_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
)

// Mail attachments (ADR 0082 decision 6; hub ticket bilet-qr-ve-pdf 08): a
// service's file goes with its mail to one person. Over SMTP the mail is
// multipart/mixed, the body first, then each file a base64 part with
// Content-Disposition attachment; Graph's JSON carries each file as a
// fileAttachment, still with saveToSentItems false. The file is kept only
// until the row's outcome is written.

// onePixelPNG is a 1×1 PNG.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func ticketFiles(t *testing.T) []mailer.Attachment {
	t.Helper()
	pdf, err := os.ReadFile("testdata/bilet-ornek.pdf")
	if err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	return []mailer.Attachment{
		{Filename: "bilet-gecekodu-2026.pdf", ContentType: "application/pdf", Content: pdf},
		{Filename: "bilet-qr.png", ContentType: "image/png", Content: png},
	}
}

// mimePart is one leaf part of a mail: its headers and its body, decoded.
type mimePart struct {
	contentType, disposition, encoding string
	body                               []byte
}

// partsOf reads a mail as the relay took it: its top-level media type and its
// leaf parts in order, a nested multipart opened.
func partsOf(t *testing.T, data string) (string, []mimePart) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("read the mail: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("Content-Type %q: %v", msg.Header.Get("Content-Type"), err)
	}
	var parts []mimePart
	var walk func(r io.Reader, boundary string)
	walk = func(r io.Reader, boundary string) {
		reader := multipart.NewReader(r, boundary)
		for {
			part, err := reader.NextRawPart()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("read a part: %v", err)
			}
			kind, kindParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if strings.HasPrefix(kind, "multipart/") {
				walk(part, kindParams["boundary"])
				continue
			}
			raw, _ := io.ReadAll(part)
			p := mimePart{contentType: part.Header.Get("Content-Type"), disposition: part.Header.Get("Content-Disposition"),
				encoding: part.Header.Get("Content-Transfer-Encoding"), body: raw}
			if strings.EqualFold(p.encoding, "base64") {
				decoded, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\r", "", "\n", "").Replace(string(raw)))
				if err != nil {
					t.Fatalf("a base64 part does not decode: %v", err)
				}
				p.body = decoded
			}
			parts = append(parts, p)
		}
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		walk(msg.Body, params["boundary"])
	}
	return mediaType, parts
}

// Over SMTP: multipart/mixed, the plain text and its HTML alternative first,
// then each file as a base64 part named as given, of its type, attached.
func TestAMailCarriesItsAttachmentsOverSMTP(t *testing.T) {
	captureMailerLogs(t)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	files := ticketFiles(t)
	job := database.MailQueue{ID: uuid.New(), Channel: mailer.ChannelEmail, RecipientFullName: "Ayşe Kaya",
		RecipientEmail: "ayse@example.com", Subject: "Biletin hazır", Body: "Biletin ekte.", BodyHtml: html("<p>Biletin ekte.</p>")}
	mailer.SetAttachments(m, func(_ context.Context, queueID uuid.UUID) ([]mailer.Attachment, error) {
		if queueID != job.ID {
			t.Errorf("attachments of %s read, want the row's own %s", queueID, job.ID)
		}
		return files, nil
	})
	if err := mailer.SendOnce(context.Background(), m, job); err != nil {
		t.Fatal(err)
	}
	mails := relay.taken()
	if len(mails) != 1 {
		t.Fatalf("%d mails", len(mails))
	}
	mediaType, parts := partsOf(t, mails[0])
	if mediaType != "multipart/mixed" {
		t.Fatalf("Content-Type %s, want multipart/mixed", mediaType)
	}
	if len(parts) != 4 {
		t.Fatalf("%d parts, want the text, the HTML and the two files: %+v", len(parts), parts)
	}
	if !strings.HasPrefix(parts[0].contentType, "text/plain") || !strings.HasPrefix(parts[1].contentType, "text/html") {
		t.Errorf("body parts %q, %q: want the plain text, then the HTML", parts[0].contentType, parts[1].contentType)
	}
	for i, want := range files {
		part := parts[2+i]
		if !strings.HasPrefix(part.contentType, want.ContentType) || !strings.EqualFold(part.encoding, "base64") {
			t.Errorf("file %d: Content-Type %q, Content-Transfer-Encoding %q", i, part.contentType, part.encoding)
		}
		disposition, params, err := mime.ParseMediaType(part.disposition)
		if err != nil || disposition != "attachment" || params["filename"] != want.Filename {
			t.Errorf("file %d: Content-Disposition %q, want attachment; filename=%q", i, part.disposition, want.Filename)
		}
		if !bytes.Equal(part.body, want.Content) {
			t.Errorf("file %d: %d bytes arrived, not the file (%d bytes)", i, len(part.body), len(want.Content))
		}
	}
}

// A mail without attachments is written as it always was.
func TestAMailWithoutAttachmentsIsNotMixed(t *testing.T) {
	captureMailerLogs(t)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	if err := mailer.SendOnce(context.Background(), m, database.MailQueue{ID: uuid.New(), Channel: mailer.ChannelEmail,
		RecipientFullName: "Ayşe Kaya", RecipientEmail: "ayse@example.com", Subject: "Konu", Body: "Gövde",
		BodyHtml: html("<p>Gövde</p>")}); err != nil {
		t.Fatal(err)
	}
	if mediaType, _ := partsOf(t, relay.taken()[0]); mediaType != "multipart/alternative" {
		t.Errorf("Content-Type %s, want multipart/alternative as before", mediaType)
	}
}

// Graph's JSON: each file a #microsoft.graph.fileAttachment with its name,
// type and bytes in base64; saveToSentItems stays false.
func TestAGraphMailCarriesItsAttachmentsAsFileAttachments(t *testing.T) {
	captureMailerLogs(t)
	task := uuid.New()
	l := newLaneMailer(t, decidedEnv(), map[uuid.UUID]struct {
		key    *string
		system bool
	}{task: {ptr("event.ticket-created"), true}})
	files := ticketFiles(t)
	mailer.SetAttachments(l.m, func(context.Context, uuid.UUID) ([]mailer.Attachment, error) { return files, nil })
	job := database.MailQueue{ID: uuid.New(), TaskID: task, Channel: mailer.ChannelEmail, Priority: int16(mailer.PrioritySystem),
		RecipientFullName: "Ayşe Kaya", RecipientEmail: "ayse@example.com", Subject: "Biletin hazır", Body: "Biletin ekte.",
		BodyHtml: html("<p>Biletin ekte.</p>")}
	if err := mailer.SendOnce(context.Background(), l.m, job); err != nil {
		t.Fatal(err)
	}
	calls := l.graph.taken()
	if len(calls) != 1 || calls[0].contentType != "application/json" {
		t.Fatalf("Graph took %d requests (%+v), want one JSON sendMail", len(calls), calls)
	}
	if saved, present := calls[0].body["saveToSentItems"]; !present || saved != false {
		t.Errorf("saveToSentItems %v (present %v), want false", saved, present)
	}
	message, _ := calls[0].body["message"].(map[string]any)
	got, _ := json.Marshal(message["attachments"])
	want, _ := json.Marshal([]map[string]any{
		{"@odata.type": "#microsoft.graph.fileAttachment", "name": "bilet-gecekodu-2026.pdf", "contentType": "application/pdf",
			"contentBytes": base64.StdEncoding.EncodeToString(files[0].Content)},
		{"@odata.type": "#microsoft.graph.fileAttachment", "name": "bilet-qr.png", "contentType": "image/png",
			"contentBytes": onePixelPNG},
	})
	if string(got) != string(want) {
		t.Errorf("attachments\n got %s\nwant %s", got, want)
	}
}

// A row whose attachments cannot be read is tried again later, not sent
// without them.
func TestAnAttachmentReadFailureIsTriedAgain(t *testing.T) {
	captureMailerLogs(t)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(nil, answeringConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	mailer.SetAttachments(m, func(context.Context, uuid.UUID) ([]mailer.Attachment, error) {
		return nil, context.DeadlineExceeded
	})
	err := mailer.SendOnce(context.Background(), m, database.MailQueue{ID: uuid.New(), Channel: mailer.ChannelEmail,
		RecipientFullName: "Ayşe Kaya", RecipientEmail: "ayse@example.com", Subject: "Konu", Body: "Gövde"})
	if err == nil || mailer.IsPermanent(err) {
		t.Errorf("err = %v, want a failure tried again", err)
	}
	if n := len(relay.taken()); n != 0 {
		t.Errorf("%d mails went without their attachments", n)
	}
}

type attachmentRecord struct {
	filename, contentType string
	size                  int
	kept                  bool
}

func attachmentRecords(t *testing.T, db *database.Store, task uuid.UUID) []attachmentRecord {
	t.Helper()
	rows, err := db.Conn.Query(context.Background(), `
		SELECT a.filename, a.content_type, a.size_bytes, a.content IS NOT NULL
		FROM mail_attachments a JOIN mail_queue q ON q.id = a.queue_id
		WHERE q.task_id = $1 ORDER BY a.position`, task)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []attachmentRecord
	for rows.Next() {
		var r attachmentRecord
		if err := rows.Scan(&r.filename, &r.contentType, &r.size, &r.kept); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// The whole way: the files are written in the transaction that queues the
// mail, the dispatcher's worker reads them from the database and the relay
// gets them; once the row is sent its files are gone and only their names,
// types and sizes stay.
func TestAnAttachmentIsKeptOnlyUntilItsMailIsSent(t *testing.T) {
	captureMailerLogs(t)
	db := laneQueue(t, 0)
	relay := startAnsweringRelay(t, "", "")
	m := mailer.NewMailer(db, answeringConfig(relay), mailer.SenderOn)
	plainSMTP(m)
	files := ticketFiles(t)
	ctx := mailer.WithSendHook(context.Background(), func(ctx context.Context, q *database.Queries, sent mailer.Sent) error {
		rows, err := mailer.StoreAttachments(ctx, q, sent.TaskID, files)
		if err == nil && rows != 1 {
			t.Errorf("the files went with %d rows, want the one", rows)
		}
		return err
	})
	template := laneTemplate
	task, err := m.EnqueueSingle(ctx, database.CreateSingleMailTaskParams{
		SentBy: laneSender, TemplateID: &template, BodyVariables: []byte(`{}`),
		RecipientFullName: "Ayşe Kaya", RecipientEmail: "ayse@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	records := attachmentRecords(t, db, task)
	if len(records) != 2 || !records[0].kept || !records[1].kept {
		t.Fatalf("queued: %+v, want both files kept", records)
	}

	startMailer(t, m, 1)
	m.Wake()
	waitFor(t, "the mail to be sent", 10*time.Second, func() bool { return keptRowOf(t, db, task).status == "sent" })
	mails := relay.taken()
	if len(mails) != 1 {
		t.Fatalf("%d mails", len(mails))
	}
	if _, parts := partsOf(t, mails[0]); len(parts) < 2 || !bytes.Equal(parts[len(parts)-2].body, files[0].Content) ||
		!bytes.Equal(parts[len(parts)-1].body, files[1].Content) {
		t.Errorf("the relay did not get the files: %d parts", len(parts))
	}
	want := []attachmentRecord{
		{"bilet-gecekodu-2026.pdf", "application/pdf", len(files[0].Content), false},
		{"bilet-qr.png", "image/png", len(files[1].Content), false},
	}
	if got := attachmentRecords(t, db, task); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("sent: %+v, want only the names, types and sizes %+v", got, want)
	}
}
