package mimeparse

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/messages/multipart.eml")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func attachmentByFilename(t *testing.T, d *Decoded, filename string) Attachment {
	t.Helper()
	for _, a := range d.Attachments {
		if a.Filename == filename {
			return a
		}
	}
	t.Fatalf("attachment %q not found in %+v", filename, d.Attachments)
	return Attachment{}
}

func TestExtractAttachmentNestedInlineAndMetadata(t *testing.T) {
	raw := loadFixture(t)
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	logo := attachmentByFilename(t, d, "logo.png")
	if logo.Part != "1.2" || logo.MimeType != "image/png" || !logo.IsInline {
		t.Errorf("logo metadata = %+v", logo)
	}
	if logo.ContentID != "logo@example.test" || logo.Size != 99 {
		t.Errorf("logo content metadata = %+v", logo)
	}
	got, err := ExtractAttachment(raw, logo.Part, Limits{})
	if err != nil {
		t.Fatalf("ExtractAttachment(logo): %v", err)
	}
	if !bytes.HasPrefix(got.Content, []byte("\x89PNG\r\n\x1a\n")) {
		t.Errorf("logo content does not start with a PNG signature: %q", got.Content[:8])
	}
	if got.Index != 0 {
		t.Errorf("logo index = %d, want 0", got.Index)
	}
	if len(got.Content) != int(logo.Size) || got.Filename != logo.Filename || got.MimeType != logo.MimeType {
		t.Errorf("extracted logo metadata/content mismatch: %+v len=%d", got.Attachment, len(got.Content))
	}

	pdf := attachmentByFilename(t, d, "report.pdf")
	if pdf.Part != "2" || pdf.MimeType != "application/pdf" || pdf.Size != 296 {
		t.Errorf("pdf metadata = %+v", pdf)
	}
	got, err = ExtractAttachment(raw, pdf.Part, Limits{})
	if err != nil {
		t.Fatalf("ExtractAttachment(pdf): %v", err)
	}
	if !bytes.HasPrefix(got.Content, []byte("%PDF-1.4")) || len(got.Content) != 296 {
		t.Errorf("pdf content = %q len=%d", got.Content[:8], len(got.Content))
	}
	if got.Index != 1 {
		t.Errorf("pdf index = %d, want 1", got.Index)
	}

	csv := attachmentByFilename(t, d, "data.csv")
	if csv.Part != "3" || csv.MimeType != "text/csv" || !csv.IsText {
		t.Errorf("csv metadata = %+v", csv)
	}
	got, err = ExtractAttachment(raw, csv.Part, Limits{})
	if err != nil {
		t.Fatalf("ExtractAttachment(csv): %v", err)
	}
	if !strings.Contains(string(got.Content), "name,count") ||
		!strings.Contains(string(got.Content), "a,1") ||
		!strings.Contains(string(got.Content), "b,2") {
		t.Errorf("csv content = %q", got.Content)
	}
	if got.Index != 2 {
		t.Errorf("csv index = %d, want 2", got.Index)
	}
}

func TestExtractAttachmentExactDecodedContentAndCharset(t *testing.T) {
	raw := "From: a@example.test\r\n" +
		"Subject: Nested\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\n" +
		"Content-Type: multipart/related; boundary=REL\r\n\r\n" +
		"--REL\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"body\r\n" +
		"--REL\r\n" +
		"Content-Type: image/png\r\n" +
		"Content-ID: <logo@example.test>\r\n" +
		"Content-Disposition: inline; filename=\"logo.png\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"AAECAwQ=\r\n" +
		"--REL--\r\n" +
		"--OUT\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"caf=E9\r\n" +
		"--OUT--\r\n"

	got, err := ExtractAttachment([]byte(raw), "1.2", Limits{})
	if err != nil {
		t.Fatalf("ExtractAttachment(logo): %v", err)
	}
	if !bytes.Equal(got.Content, []byte{0, 1, 2, 3, 4}) {
		t.Errorf("logo content = %v, want [0 1 2 3 4]", got.Content)
	}
	if got.Part != "1.2" || got.Filename != "logo.png" || got.MimeType != "image/png" || !got.IsInline {
		t.Errorf("logo metadata = %+v", got.Attachment)
	}

	got, err = ExtractAttachment([]byte(raw), "2", Limits{})
	if err != nil {
		t.Fatalf("ExtractAttachment(note): %v", err)
	}
	if string(got.Content) != "café" {
		t.Errorf("note content = %q, want UTF-8 caf\u00e9", got.Content)
	}
	if got.Filename != "note.txt" || got.MimeType != "text/plain" || !got.IsText {
		t.Errorf("note metadata = %+v", got.Attachment)
	}
}

func TestExtractAttachmentOpaqueMessageRFC822(t *testing.T) {
	raw := "From: a@example.test\r\n" +
		"Subject: Outer\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"outer body\r\n" +
		"--B\r\n" +
		"Content-Type: message/rfc822\r\n" +
		"Content-Disposition: attachment; filename=\"inner.eml\"\r\n\r\n" +
		"From: inner@example.test\r\n" +
		"Subject: Inner\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"inner secret\r\n" +
		"--B--\r\n"

	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want one opaque message/rfc822 attachment", d.Attachments)
	}
	a := d.Attachments[0]
	if a.Part != "2" || a.MimeType != "message/rfc822" || a.Filename != "inner.eml" {
		t.Errorf("attachment metadata = %+v", a)
	}
	got, err := ExtractAttachment([]byte(raw), "2", Limits{})
	if err != nil {
		t.Fatalf("ExtractAttachment(2): %v", err)
	}
	if !strings.Contains(string(got.Content), "Subject: Inner") ||
		!strings.Contains(string(got.Content), "inner secret") {
		t.Errorf("opaque content = %q, want the embedded message", got.Content)
	}
	if _, err := ExtractAttachment([]byte(raw), "2.1", Limits{}); err != ErrAttachmentNotFound {
		t.Errorf("hidden inner part err = %v, want ErrAttachmentNotFound", err)
	}
}

func TestExtractAttachmentRejectsNonAttachments(t *testing.T) {
	raw := loadFixture(t)
	for _, part := range []string{"1", "1.1", "1.1.1", "1.1.2", "1.2.1", "9", ""} {
		got, err := ExtractAttachment(raw, part, Limits{})
		if err != ErrAttachmentNotFound {
			t.Errorf("part %q: err = %v, want ErrAttachmentNotFound", part, err)
		}
		if got != nil {
			t.Errorf("part %q returned content on error", part)
		}
	}
}

func TestExtractAttachmentLimitsUseDecodedSize(t *testing.T) {
	raw := "From: a@example.test\r\n" +
		"Subject: Encoded\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"a.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"QUJDREVG\r\n" + // decoded: ABCDEF (6 bytes), encoded: 8 bytes
		"--B--\r\n"

	got, err := ExtractAttachment([]byte(raw), "1", Limits{MaxAttachmentBytes: 6})
	if err != nil {
		t.Fatalf("ExtractAttachment at decoded cap: %v", err)
	}
	if string(got.Content) != "ABCDEF" || got.Size != 6 {
		t.Errorf("content = %q, size = %d", got.Content, got.Size)
	}
	if _, err := ExtractAttachment([]byte(raw), "1", Limits{MaxAttachmentBytes: 5}); err != ErrAttachmentTooLarge {
		t.Errorf("decoded cap err = %v, want ErrAttachmentTooLarge", err)
	}
}

func TestExtractAttachmentLimits(t *testing.T) {
	makeAttachment := func(name, body string) string {
		return "Content-Type: application/octet-stream\r\n" +
			"Content-Disposition: attachment; filename=\"" + name + "\"\r\n\r\n" +
			body + "\r\n"
	}
	raw := "From: a@example.test\r\n" +
		"Subject: Limits\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" + makeAttachment("a.bin", "AAA") +
		"--B\r\n" + makeAttachment("b.bin", "BBBB") +
		"--B--\r\n"

	if got, err := ExtractAttachment([]byte(raw), "1", Limits{MaxAttachmentBytes: 2}); err != ErrAttachmentTooLarge || got != nil {
		t.Errorf("per-attachment: got=%v err=%v, want nil/ErrAttachmentTooLarge", got, err)
	}
	if got, err := ExtractAttachment([]byte(raw), "1", Limits{MaxTotalAttachmentBytes: 4}); err != ErrTotalAttachmentTooLarge || got != nil {
		t.Errorf("total after capture: got=%v err=%v, want nil/ErrTotalAttachmentTooLarge", got, err)
	}
	if got, err := ExtractAttachment([]byte(raw), "1", Limits{MaxAttachments: 1}); err != ErrTooManyAttachments || got != nil {
		t.Errorf("too many attachments after capture: got=%v err=%v, want nil/ErrTooManyAttachments", got, err)
	}
	if got, err := ExtractAttachment([]byte(raw), "1", Limits{MaxMIMEParts: 2}); err != ErrTooManyParts || got != nil {
		t.Errorf("too many parts: got=%v err=%v, want nil/ErrTooManyParts", got, err)
	}
	if got, err := ExtractAttachment([]byte(raw), "1", Limits{MaxMessageBytes: len(raw) - 1}); err != ErrMessageTooLarge || got != nil {
		t.Errorf("message too large: got=%v err=%v, want nil/ErrMessageTooLarge", got, err)
	}

	nested := "From: a@example.test\r\n" +
		"Subject: Nested\r\n" +
		"Content-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\n" +
		"Content-Type: multipart/mixed; boundary=IN\r\n\r\n" +
		"--IN\r\n" + makeAttachment("a.bin", "AAA") +
		"--IN--\r\n" +
		"--OUT--\r\n"
	if got, err := ExtractAttachment([]byte(nested), "1.1", Limits{MaxMIMENesting: 1}); err != ErrNestingTooDeep || got != nil {
		t.Errorf("nesting: got=%v err=%v, want nil/ErrNestingTooDeep", got, err)
	}
}
