package mailer

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/skymail-backend/internal/database"
	"github.com/wneessen/go-mail"
)

// A Mail attachment (ADR 0082 decision 6, CONTEXT "Mail attachment"): a file
// a service hands with one mail, today core's ticket PDF. The handlers decide
// who may send one and check it (handlers.parseAttachments); the mailer keeps
// it with the queue row until the row's outcome is written (mail_attachments,
// migration 20261122150000, whose trigger empties the file then) and puts it
// into the mail: a part of a multipart/mixed mail, base64, Content-Disposition
// attachment, on the SMTP path; a fileAttachment on Graph's JSON path.

// Attachment is one file of one mail.
type Attachment struct {
	// Filename is the name the recipient sees, already reduced to
	// [A-Za-z0-9._-] (handlers.attachmentFilename).
	Filename string
	// ContentType is the file's media type, one SkyMail takes
	// (handlers.attachmentTypes).
	ContentType string
	Content     []byte
}

// StoreAttachments writes attachments on the queue row of send taskID still to
// go out by e-mail, in q's transaction, the one that queued the send (a
// SendHook): the send and its file exist together or not at all. It returns
// how many rows got them: 1 for a mail to one person, 0 when the row was
// written failed (a reserved or suppressed address) and goes nowhere.
func StoreAttachments(ctx context.Context, q *database.Queries, taskID uuid.UUID, attachments []Attachment) (int64, error) {
	var rows int64
	for i, a := range attachments {
		n, err := q.InsertMailAttachment(ctx, database.InsertMailAttachmentParams{
			Position: int16(i + 1), Filename: a.Filename, ContentType: a.ContentType, Content: a.Content, TaskID: taskID,
		})
		if err != nil {
			return 0, fmt.Errorf("store attachment %d: %w", i+1, err)
		}
		rows = n
	}
	return rows, nil
}

// AttachmentBytes is the size of attachments together, the figure the logs
// count (never a name or a file).
func AttachmentBytes(attachments []Attachment) int {
	total := 0
	for _, a := range attachments {
		total += len(a.Content)
	}
	return total
}

// attachmentsOf reads a queue row's attachments; the mailer's reads
// mail_attachments.
type attachmentsOf func(ctx context.Context, queueID uuid.UUID) ([]Attachment, error)

// databaseAttachments is attachmentsOf from the database; without one (a
// test's SendOnce), none. A file already emptied (the row was sent or failed
// for good, then put back to go out again) is left out: the mail goes
// without it, and a line says so.
func (m *mailerImpl) databaseAttachments(ctx context.Context, queueID uuid.UUID) ([]Attachment, error) {
	if m.db == nil {
		return nil, nil
	}
	rows, err := m.db.ListMailAttachmentsToSend(ctx, queueID)
	if err != nil {
		return nil, err
	}
	attachments := make([]Attachment, 0, len(rows))
	for _, row := range rows {
		if row.Content == nil {
			m.logger.Warn().Str("event", "mail_attachment_missing").Str("job_id", queueID.String()).
				Int("position", int(row.Position)).Msg("the attachment's file was emptied before this send: the mail goes without it")
			continue
		}
		attachments = append(attachments, Attachment{Filename: row.Filename, ContentType: row.ContentType, Content: row.Content})
	}
	return attachments, nil
}

// attach puts attachments into msg: go-mail then writes multipart/mixed, the
// body (the plain text and its HTML alternative) first, each file a base64
// part with Content-Disposition: attachment; filename="…".
func attach(msg *mail.Msg, attachments []Attachment) error {
	for _, a := range attachments {
		if err := msg.AttachReader(a.Filename, bytes.NewReader(a.Content),
			mail.WithFileContentType(mail.ContentType(a.ContentType)), mail.WithFileEncoding(mail.EncodingB64)); err != nil {
			return fmt.Errorf("send failed: attach %s: %w", a.ContentType, err)
		}
	}
	return nil
}

// graphAttachment is a file of a Graph sendMail's JSON message
// (microsoft.graph.fileAttachment).
type graphAttachment struct {
	ODataType    string `json:"@odata.type"`
	Name         string `json:"name"`
	ContentType  string `json:"contentType"`
	ContentBytes string `json:"contentBytes"`
}

func graphAttachments(attachments []Attachment) []graphAttachment {
	if len(attachments) == 0 {
		return nil
	}
	files := make([]graphAttachment, len(attachments))
	for i, a := range attachments {
		files[i] = graphAttachment{
			ODataType: "#microsoft.graph.fileAttachment", Name: a.Filename, ContentType: a.ContentType,
			ContentBytes: base64.StdEncoding.EncodeToString(a.Content),
		}
	}
	return files
}
