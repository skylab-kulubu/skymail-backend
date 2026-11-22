package handlers

import (
	"bytes"
	"encoding/base64"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/skylab-kulubu/skymail-backend/internal/apperrors"
	"github.com/skylab-kulubu/skymail-backend/internal/mailer"
	"github.com/skylab-kulubu/skymail-backend/internal/requests"
)

// Mail attachments (ADR 0082 decision 6, CONTEXT "Mail attachment"; hub
// ticket bilet-qr-ve-pdf 08): a service's file with its mail to one person,
// today core's ticket PDF. The limits sit well under every provider's (Gmail
// and Exchange Online take about 20-25 MB, Graph's JSON request 4 MB, SES v2
// 40 MB, Postmark 10 MB) and keep a mail with them an ordinary transactional mail:
// one or two small expected files, of a type the recipient's client opens.

const (
	// MaxAttachments is how many files one mail may carry.
	MaxAttachments = 2
	// MaxAttachmentBytes is the files' size together, decoded.
	MaxAttachmentBytes = 512 << 10
	// maxAttachmentFilename is the longest name a file keeps.
	maxAttachmentFilename = 100
)

// attachmentType is a media type SkyMail takes: the extension its file name
// ends with and the bytes its file begins with.
type attachmentType struct {
	extension string
	magic     []byte
}

// attachmentTypes are the media types SkyMail takes. A type joins here with
// its extension and its signature.
var attachmentTypes = map[string]attachmentType{
	"application/pdf": {".pdf", []byte("%PDF-")},
	"image/png":       {".png", []byte("\x89PNG\r\n\x1a\n")},
}

// allowedAttachmentTypes is attachmentTypes' keys, for the error's params.
var allowedAttachmentTypes = []string{"application/pdf", "image/png"}

var (
	errAttachmentsNotAllowed = apperrors.New("attachment.not_allowed",
		"Attachments go only with a service's mail to one person (POST /mail_tasks/single, POST /notifications to one recipient), never with a list, a group, a Forms source, a Mail onayı request or a News send.",
		fiber.StatusBadRequest)
	errAttachmentsServiceOnly = apperrors.New("attachment.service_only",
		"Only a service's token may send attachments.", fiber.StatusBadRequest)
	errAttachmentsSingleRecipient = apperrors.New("attachment.single_recipient_only",
		"A notification with attachments goes to exactly one recipient.", fiber.StatusBadRequest)
	errAttachmentsTooMany = apperrors.New("attachment.too_many",
		"A mail carries at most 2 attachments.", fiber.StatusBadRequest)
	errAttachmentsTooLarge = apperrors.New("attachment.too_large",
		"A mail's attachments are at most 512 KB together, decoded.", fiber.StatusBadRequest)
	errAttachmentTypeNotAllowed = apperrors.New("attachment.type_not_allowed",
		"An attachment is application/pdf or image/png.", fiber.StatusBadRequest)
	errAttachmentContentInvalid = apperrors.New("attachment.content_invalid",
		"An attachment's content_base64 is a non-empty file in standard base64 (RFC 4648, with padding).", fiber.StatusBadRequest)
	errAttachmentContentMismatch = apperrors.New("attachment.content_type_mismatch",
		"The attachment's file is not of its content_type.", fiber.StatusBadRequest)
)

// parseAttachments checks a request's attachments and decodes them: the
// type, the file, the count and the size together; the name is reduced
// (attachmentFilename), never refused. Errors name the attachment by its
// index (params.index).
func parseAttachments(in []requests.MailAttachment) ([]mailer.Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxAttachments {
		return nil, errAttachmentsTooMany.WithParams(map[string]interface{}{"max": MaxAttachments})
	}
	tooLarge := errAttachmentsTooLarge.WithParams(map[string]interface{}{"max_bytes": MaxAttachmentBytes})
	out := make([]mailer.Attachment, 0, len(in))
	total := 0
	for i, a := range in {
		at := map[string]interface{}{"index": i}
		contentType := strings.ToLower(strings.TrimSpace(a.ContentType))
		if semi := strings.IndexByte(contentType, ';'); semi >= 0 {
			contentType = strings.TrimSpace(contentType[:semi])
		}
		kind, ok := attachmentTypes[contentType]
		if !ok {
			return nil, errAttachmentTypeNotAllowed.WithParams(map[string]interface{}{"index": i, "allowed": allowedAttachmentTypes})
		}
		// Not decoded past the limit: the encoded length says enough.
		if base64.StdEncoding.DecodedLen(len(a.ContentBase64)) > MaxAttachmentBytes+3 {
			return nil, tooLarge
		}
		content, err := base64.StdEncoding.DecodeString(a.ContentBase64)
		if err != nil || len(content) == 0 {
			return nil, errAttachmentContentInvalid.WithParams(at)
		}
		if !bytes.HasPrefix(content, kind.magic) {
			return nil, errAttachmentContentMismatch.WithParams(at)
		}
		if total += len(content); total > MaxAttachmentBytes {
			return nil, tooLarge
		}
		out = append(out, mailer.Attachment{
			Filename: attachmentFilename(a.Filename, kind.extension), ContentType: contentType, Content: content,
		})
	}
	return out, nil
}

// turkishLetters are the letters a file name keeps as their Latin ones,
// the circumflexed â, î and û of Turkish spelling (Kâğıt, Hâlâ) among them.
var turkishLetters = strings.NewReplacer(
	"ç", "c", "Ç", "C", "ğ", "g", "Ğ", "G", "ı", "i", "İ", "I",
	"ö", "o", "Ö", "O", "ş", "s", "Ş", "S", "ü", "u", "Ü", "U",
	"â", "a", "Â", "A", "î", "i", "Î", "I", "û", "u", "Û", "U",
)

// attachmentFilename reduces name to what every mail client shows and saves
// alike: letters, digits, '.', '-' and '_', beginning with a letter or a
// digit, at most 100 characters, ending with extension. A path's directories
// go; a space becomes '-'; anything else is dropped. Nothing left is "ek".
func attachmentFilename(name, extension string) string {
	if slash := strings.LastIndexAny(name, `/\`); slash >= 0 {
		name = name[slash+1:]
	}
	name = turkishLetters.Replace(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
		case r == '-', r == ' ', r == '\t':
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteByte('-')
			}
		}
	}
	stem := b.String()
	if strings.HasSuffix(strings.ToLower(stem), extension) {
		stem = stem[:len(stem)-len(extension)]
	}
	stem = strings.TrimLeft(stem, "._-")
	stem = strings.TrimRight(stem, ".-")
	if len(stem) > maxAttachmentFilename-len(extension) {
		stem = strings.TrimRight(stem[:maxAttachmentFilename-len(extension)], ".-")
	}
	if stem == "" {
		stem = "ek"
	}
	return stem + extension
}

// AttachmentInfo is a Mail attachment as a send's report shows it: its name,
// type and size, never the file.
type AttachmentInfo struct {
	Filename    string `json:"filename" example:"bilet-gecekodu-2026.pdf"`
	ContentType string `json:"content_type" example:"application/pdf"`
	// The file's size in bytes, decoded.
	Size int `json:"size" example:"84213"`
}

// logAttachmentsQueued writes the line attachments are counted from: the
// send, how many files and their bytes together. No name, no file, no
// person. stored is how many went with the row (0: none, or the row failed
// at queueing).
func logAttachmentsQueued(taskID uuid.UUID, attachments []mailer.Attachment, stored int) {
	if len(attachments) == 0 {
		return
	}
	log.Info().Str("event", "mail_attachments_queued").Str("task_id", taskID.String()).
		Int("attachments", stored).Int("bytes", mailer.AttachmentBytes(attachments[:stored])).
		Int("not_stored", len(attachments)-stored).Msg("queued a mail with attachments")
}
