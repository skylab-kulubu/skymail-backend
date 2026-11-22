package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// Mail attachments (ADR 0082 decision 6; hub ticket bilet-qr-ve-pdf 08)
// through the chain production registers: a service's single mail and a
// notification to one person take a PDF or a PNG; every other send refuses
// one. The file is kept with the queue row only until its outcome is
// written; the send's report shows its name, type and size.

var (
	ticketPDF      = []byte("%PDF-1.4\n1 0 obj << /Type /Catalog >> endobj\ntrailer << /Root 1 0 R >>\n%%EOF\n")
	ticketPDFAgain = []byte("%PDF-1.4\n% drawn again: another creation date\ntrailer << >>\n%%EOF\n")
)

func pdfAttachment(name string, content []byte) map[string]any {
	return map[string]any{"filename": name, "content_type": "application/pdf", "content_base64": base64.StdEncoding.EncodeToString(content)}
}

type storedAttachment struct {
	queueID               uuid.UUID
	filename, contentType string
	size                  int
	content               []byte
}

func (w *approvalWorld) attachmentsOf(task uuid.UUID) []storedAttachment {
	w.t.Helper()
	rows, err := w.store.Conn.Query(context.Background(), `
		SELECT a.queue_id, a.filename, a.content_type, a.size_bytes, a.content
		FROM mail_attachments a JOIN mail_queue q ON q.id = a.queue_id
		WHERE q.task_id = $1 ORDER BY a.position`, task)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []storedAttachment
	for rows.Next() {
		var a storedAttachment
		if err := rows.Scan(&a.queueID, &a.filename, &a.contentType, &a.size, &a.content); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func attachmentsAnswered(t *testing.T, raw []byte) int {
	t.Helper()
	var answer struct {
		Attachments int `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatalf("answer %s: %v", raw, err)
	}
	return answer.Attachments
}

// A service's single mail with a PDF: the file is kept with the row, named
// safely, and the answer says it went with the mail. A retry under the same
// Idempotency-Key with the PDF drawn again is the same mail: the first answer,
// no second file. Once the row is sent the file is gone and the send's report
// shows its name, type and size.
func TestAServicesSingleMailCarriesItsAttachment(t *testing.T) {
	app, w := singleSendWorld(t)
	core := uuid.NewString()

	send := w.singleSendTo(elif.email, "Elif", false)
	send["attachments"] = []any{pdfAttachment("GECEKODU Bileti.pdf", ticketPDF)}
	first := postSingle(t, app, core, true, "core:event.ticket-created:t1:v1", send)
	if first.status != fiber.StatusCreated || attachmentsAnswered(t, first.raw) != 1 {
		t.Fatalf("first = %d %s, want 201 with attachments 1", first.status, first.raw)
	}
	kept := w.attachmentsOf(first.created.ID)
	if len(kept) != 1 || kept[0].filename != "GECEKODU-Bileti.pdf" || kept[0].contentType != "application/pdf" ||
		kept[0].size != len(ticketPDF) || !bytes.Equal(kept[0].content, ticketPDF) {
		t.Fatalf("kept %+v, want the PDF as GECEKODU-Bileti.pdf", kept)
	}

	again := w.singleSendTo(elif.email, "Elif", false)
	again["attachments"] = []any{pdfAttachment("GECEKODU Bileti.pdf", ticketPDFAgain)}
	retry := postSingle(t, app, core, true, "core:event.ticket-created:t1:v1", again)
	if retry.status != fiber.StatusCreated || retry.replayed != "true" || string(retry.raw) != string(first.raw) {
		t.Fatalf("retry = %d %s (replayed %q), want the first answer %s", retry.status, retry.raw, retry.replayed, first.raw)
	}
	if n := w.count(`SELECT count(*) FROM mail_attachments`); n != 1 {
		t.Errorf("%d files kept after the retry, want the first one only", n)
	}
	if got := w.attachmentsOf(first.created.ID); !bytes.Equal(got[0].content, ticketPDF) {
		t.Errorf("the retry's file replaced the first")
	}

	// Sent: the file goes in the transaction that writes the outcome.
	if _, err := w.store.Conn.Exec(context.Background(), `UPDATE mail_queue SET status = 'sent', sent_at = now() WHERE task_id = $1`, first.created.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.attachmentsOf(first.created.ID); len(got) != 1 || got[0].content != nil || got[0].filename != "GECEKODU-Bileti.pdf" || got[0].size != len(ticketPDF) {
		t.Errorf("after sending %+v, want the name and size without the file", got)
	}

	// The report: name, type and size, never the file.
	request := httptest.NewRequest(http.MethodGet, "/v1/mail_tasks/"+first.created.ID.String()+"/queue", nil)
	request.Header.Set("Authorization", "Bearer "+bearer(t, uuid.NewString(), false, "skymail:access", "skymail:mails:read"))
	response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil || response.StatusCode != fiber.StatusOK || len(items) != 1 {
		t.Fatalf("queue = %d %s (%v)", response.StatusCode, raw, err)
	}
	report, _ := json.Marshal(items[0]["attachments"])
	if want := `[{"content_type":"application/pdf","filename":"GECEKODU-Bileti.pdf","size":` + strconv.Itoa(len(ticketPDF)) + `}]`; string(report) != want {
		t.Errorf("report attachments %s, want %s", report, want)
	}
	if items[0]["recipient_email"] != elif.email {
		t.Errorf("the queue row's own fields went: %s", raw)
	}

	// A failure for good empties the file as well.
	other := postSingle(t, app, core, true, "", send)
	if other.status != fiber.StatusCreated {
		t.Fatalf("second send = %d %s", other.status, other.raw)
	}
	if _, err := w.store.Conn.Exec(context.Background(), `UPDATE mail_queue SET status = 'failed', error = 'smtp_rejected: DATA 554 5.6.0' WHERE task_id = $1`, other.created.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.attachmentsOf(other.created.ID); len(got) != 1 || got[0].content != nil {
		t.Errorf("after failing for good %+v, want the file gone", got)
	}

	// The content sweep takes the record with the send's content.
	if _, err := w.store.Conn.Exec(context.Background(), `UPDATE mail_tasks SET content_cleared_at = now() WHERE id = $1`, first.created.ID); err != nil {
		t.Fatal(err)
	}
	if deleted, err := w.store.DeleteMailAttachmentRecords(context.Background(), 1); err != nil || deleted != 1 {
		t.Fatalf("swept %d records (%v), want the emptied send's one", deleted, err)
	}
	if n := len(w.attachmentsOf(other.created.ID)); n != 1 {
		t.Errorf("a send not emptied lost its record")
	}
}

// Refused attachments queue nothing: a person's token, too many, a type
// SkyMail does not take, a file not of its type, over 512 KB.
func TestRefusedAttachmentsQueueNothing(t *testing.T) {
	app, w := singleSendWorld(t)
	png := map[string]any{"filename": "qr.png", "content_type": "image/png",
		"content_base64": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}
	large := make([]byte, 512<<10+1)
	copy(large, "%PDF-")
	for _, c := range []struct {
		name        string
		service     bool
		attachments []any
		code        string
	}{
		{"a person's token", false, []any{pdfAttachment("bilet.pdf", ticketPDF)}, "attachment.service_only"},
		{"three files", true, []any{pdfAttachment("a.pdf", ticketPDF), png, pdfAttachment("b.pdf", ticketPDF)}, "attachment.too_many"},
		{"a zip", true, []any{map[string]any{"filename": "a.zip", "content_type": "application/zip", "content_base64": "UEsDBA=="}}, "attachment.type_not_allowed"},
		{"a PNG called a PDF", true, []any{map[string]any{"filename": "a.pdf", "content_type": "application/pdf", "content_base64": png["content_base64"]}}, "attachment.content_type_mismatch"},
		{"over 512 KB", true, []any{pdfAttachment("a.pdf", large)}, "attachment.too_large"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := w.count(`SELECT count(*) FROM mail_tasks`)
			send := w.singleSendTo(elif.email, "Elif", false)
			send["attachments"] = c.attachments
			p := postSingle(t, app, uuid.NewString(), c.service, "", send)
			if p.status != fiber.StatusBadRequest || p.error.Code != c.code {
				t.Errorf("= %d %s, want 400 %s", p.status, p.raw, c.code)
			}
			if got := w.count(`SELECT count(*) FROM mail_tasks`) - before; got != 0 {
				t.Errorf("%d sends queued", got)
			}
		})
	}
	if n := w.count(`SELECT count(*) FROM mail_attachments`); n != 0 {
		t.Errorf("%d files kept", n)
	}
}

// A bulk send carries no person's file: a list or a group, a Forms source, a
// Mail onayı request.
func TestABulkSendTakesNoAttachment(t *testing.T) {
	app, w := singleSendWorld(t)
	post := func(path string, body map[string]any) (int, []byte) {
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+bearer(t, uuid.NewString(), false, "skymail:access", "skymail:mails:write"))
		request.Header.Set("Content-Type", "application/json")
		response, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(response.Body)
		return response.StatusCode, answer
	}
	attachments := []any{pdfAttachment("bilet.pdf", ticketPDF)}
	before := w.count(`SELECT count(*) FROM mail_tasks`)

	list := w.listSend()
	list["attachments"] = attachments
	if status, raw := post("/v1/mail_tasks", list); status != fiber.StatusBadRequest || !bytes.Contains(raw, []byte(`"attachment.not_allowed"`)) {
		t.Errorf("a list send = %d %s, want 400 attachment.not_allowed", status, raw)
	}
	source := map[string]any{"form_source_id": uuid.New(), "template_id": w.freeBasic.ID, "attachments": attachments}
	if status, raw := post("/v1/mail_tasks/form-source", source); status != fiber.StatusBadRequest {
		t.Errorf("a Forms source send = %d %s, want 400", status, raw)
	}
	request := w.listSend()
	request["attachments"] = attachments
	if status, raw := w.call("elif", fiber.MethodPost, "/v1/mail_approvals", request, nil); status != fiber.StatusBadRequest ||
		!bytes.Contains(raw, []byte(`"attachment.not_allowed"`)) {
		t.Errorf("a Mail onayı request = %d %s, want 400 attachment.not_allowed", status, raw)
	}
	if got := w.count(`SELECT count(*) FROM mail_tasks`) - before; got != 0 {
		t.Errorf("%d sends queued", got)
	}
	if n := w.count(`SELECT count(*) FROM mail_approvals`); n != 0 {
		t.Errorf("%d requests written", n)
	}
}

// A notification to one person carries its PDF on the e-mail; to several it
// is refused; a retry under its key writes no second file.
func TestANotificationToOnePersonCarriesItsAttachment(t *testing.T) {
	w := newNotificationWorld(t)
	token := bearer(t, coreService, true, coreRoles...)
	body := notification("core.certificate", recipient(personAyse, ""))
	body["attachments"] = []any{pdfAttachment("sertifika.pdf", ticketPDF)}

	first := w.post(token, "core:certificate:1", body)
	email := first.result.Channels["email"]
	if first.status != fiber.StatusCreated || email.Attachments != 1 || email.TaskID == nil {
		t.Fatalf("= %d %s, want 201 with the e-mail's attachments 1", first.status, first.raw)
	}
	countFiles := func() int { return w.count(`SELECT count(*) FROM mail_attachments WHERE content IS NOT NULL`) }
	if n := countFiles(); n != 1 {
		t.Fatalf("%d files kept, want 1", n)
	}
	again := notification("core.certificate", recipient(personAyse, ""))
	again["attachments"] = []any{pdfAttachment("sertifika.pdf", ticketPDFAgain)}
	retry := w.post(token, "core:certificate:1", again)
	if retry.status != fiber.StatusCreated || retry.replayed != "true" || string(retry.raw) != string(first.raw) {
		t.Errorf("retry = %d %s (replayed %q), want the first answer", retry.status, retry.raw, retry.replayed)
	}
	if n := countFiles(); n != 1 {
		t.Errorf("%d files kept after the retry, want 1", n)
	}

	before := w.count(`SELECT count(*) FROM notifications`)
	two := notification("core.certificate", recipient(personAyse, ""), recipient(personMehmet, ""))
	two["attachments"] = []any{pdfAttachment("sertifika.pdf", ticketPDF)}
	refused := w.post(token, "core:certificate:2", two)
	if refused.status != fiber.StatusBadRequest || refused.error.Code != "attachment.single_recipient_only" {
		t.Errorf("two recipients = %d %s, want 400 attachment.single_recipient_only", refused.status, refused.raw)
	}
	if got := w.count(`SELECT count(*) FROM notifications`) - before; got != 0 {
		t.Errorf("%d notifications written", got)
	}
	if n := countFiles(); n != 1 {
		t.Errorf("%d files kept, want 1", n)
	}
}
