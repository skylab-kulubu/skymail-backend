package handlers

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/skylab-kulubu/skynotify-backend/internal/apperrors"
	"github.com/skylab-kulubu/skynotify-backend/internal/requests"
)

var (
	tinyPDF = []byte("%PDF-1.4\n%%EOF\n")
	tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
)

func attachmentOf(name, contentType string, content []byte) requests.MailAttachment {
	return requests.MailAttachment{Filename: name, ContentType: contentType, ContentBase64: base64.StdEncoding.EncodeToString(content)}
}

func codeOf(err error) string {
	var app *apperrors.AppError
	if errors.As(err, &app) {
		return app.Code
	}
	return ""
}

// What SkyNotify takes: a PDF or a PNG whose file begins as its type does, at
// most two, 512 KB decoded together; the type may carry parameters and any
// case.
func TestParseAttachments(t *testing.T) {
	pdf := attachmentOf("bilet.pdf", "application/pdf", tinyPDF)
	png := attachmentOf("qr.png", "image/png", tinyPNG)
	big := make([]byte, MaxAttachmentBytes/2+1)
	copy(big, "%PDF-")

	for _, c := range []struct {
		name  string
		in    []requests.MailAttachment
		code  string
		index any
	}{
		{"none", nil, "", nil},
		{"a PDF and a PNG", []requests.MailAttachment{pdf, png}, "", nil},
		{"a type with parameters and capitals", []requests.MailAttachment{attachmentOf("b.pdf", "Application/PDF; name=b.pdf", tinyPDF)}, "", nil},
		{"three", []requests.MailAttachment{pdf, png, pdf}, "attachment.too_many", nil},
		{"a Word file", []requests.MailAttachment{attachmentOf("a.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", tinyPDF)}, "attachment.type_not_allowed", 0},
		{"an HTML file", []requests.MailAttachment{pdf, attachmentOf("a.html", "text/html", []byte("<p>x</p>"))}, "attachment.type_not_allowed", 1},
		{"not base64", []requests.MailAttachment{{Filename: "a.pdf", ContentType: "application/pdf", ContentBase64: "%%%"}}, "attachment.content_invalid", 0},
		{"unpadded base64", []requests.MailAttachment{{Filename: "a.pdf", ContentType: "application/pdf", ContentBase64: strings.TrimRight(base64.StdEncoding.EncodeToString([]byte("%PDF-1.4\n%%EOF\n\n")), "=")}}, "attachment.content_invalid", 0},
		{"empty", []requests.MailAttachment{{Filename: "a.pdf", ContentType: "application/pdf"}}, "attachment.content_invalid", 0},
		{"a PNG called a PDF", []requests.MailAttachment{attachmentOf("a.pdf", "application/pdf", tinyPNG)}, "attachment.content_type_mismatch", 0},
		{"an executable called a PNG", []requests.MailAttachment{pdf, attachmentOf("a.png", "image/png", []byte("MZ\x90\x00"))}, "attachment.content_type_mismatch", 1},
		{"two halves over 512 KB", []requests.MailAttachment{attachmentOf("a.pdf", "application/pdf", big), attachmentOf("b.pdf", "application/pdf", big)}, "attachment.too_large", nil},
		{"one over 512 KB", []requests.MailAttachment{attachmentOf("a.pdf", "application/pdf", append(big, big...))}, "attachment.too_large", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseAttachments(c.in)
			if code := codeOf(err); code != c.code {
				t.Fatalf("code %q (%v), want %q", code, err, c.code)
			}
			if c.code == "" {
				if err != nil || len(got) != len(c.in) {
					t.Fatalf("got %d attachments, %v", len(got), err)
				}
				return
			}
			var app *apperrors.AppError
			errors.As(err, &app)
			if c.index != nil && app.Params["index"] != c.index {
				t.Errorf("params %v, want index %v", app.Params, c.index)
			}
		})
	}

	got, err := parseAttachments([]requests.MailAttachment{attachmentOf("Bilet Ayşe.PDF", " application/pdf ", tinyPDF), png})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ContentType != "application/pdf" || string(got[0].Content) != string(tinyPDF) || got[0].Filename != "Bilet-Ayse.pdf" {
		t.Errorf("first = %q %q %q", got[0].Filename, got[0].ContentType, got[0].Content)
	}
	if got[1].Filename != "qr.png" || got[1].ContentType != "image/png" {
		t.Errorf("second = %q %q", got[1].Filename, got[1].ContentType)
	}
}

// A file name is reduced, never refused: what every client shows and saves
// alike, ending with the type's extension.
func TestAttachmentFilename(t *testing.T) {
	for _, c := range []struct{ in, ext, want string }{
		{"bilet-gecekodu-2026.pdf", ".pdf", "bilet-gecekodu-2026.pdf"},
		{"Gece Kodu Bileti.pdf", ".pdf", "Gece-Kodu-Bileti.pdf"},
		{"Şölen için ÇIĞ bileti.PDF", ".pdf", "Solen-icin-CIG-bileti.pdf"},
		{"bilet", ".pdf", "bilet.pdf"},
		{"Kâğıt bilet.pdf", ".pdf", "Kagit-bilet.pdf"},
		{"Hâlâ Îmar Ûmran.pdf", ".pdf", "Hala-Imar-Umran.pdf"},
		{"qr.jpg", ".png", "qr.jpg.png"},
		{"../../etc/passwd", ".pdf", "passwd.pdf"},
		{`C:\Users\x\bilet.pdf`, ".pdf", "bilet.pdf"},
		{".gizli.pdf", ".pdf", "gizli.pdf"},
		{"bilet<script>.pdf", ".pdf", "biletscript.pdf"},
		{"a  -  b.pdf", ".pdf", "a-b.pdf"},
		{"", ".pdf", "ek.pdf"},
		{"😀.png", ".png", "ek.png"},
		{"\r\nContent-Type: text.pdf", ".pdf", "Content-Type-text.pdf"},
		{strings.Repeat("a", 150) + ".pdf", ".pdf", strings.Repeat("a", 96) + ".pdf"},
	} {
		got := attachmentFilename(c.in, c.ext)
		if got != c.want {
			t.Errorf("attachmentFilename(%q) = %q, want %q", c.in, got, c.want)
		}
		if len(got) > maxAttachmentFilename {
			t.Errorf("%q is %d characters", got, len(got))
		}
	}
}
