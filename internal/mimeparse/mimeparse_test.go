package mimeparse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// plainText is the simplest possible MIME message: just headers
// and a text body.
const plainText = "From: a@example.test\r\n" +
	"To: b@example.test\r\n" +
	"Subject: Plain\r\n" +
	"Date: Mon, 15 Jan 2024 10:00:00 +0000\r\n" +
	"Message-Id: <p@example.test>\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"Hello world.\r\n"

func TestParsePlainText(t *testing.T) {
	d, err := Parse([]byte(plainText), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// go-message strips the trailing CRLF that follows a
	// top-level part body; compare loosely.
	if !strings.Contains(d.Text, "Hello world") {
		t.Errorf("Text = %q", d.Text)
	}
	if d.Headers.Subject != "Plain" {
		t.Errorf("Subject = %q", d.Headers.Subject)
	}
	if d.Headers.MessageID != "p@example.test" && d.Headers.MessageID != "<p@example.test>" {
		t.Errorf("MessageID = %q", d.Headers.MessageID)
	}
	if len(d.Attachments) != 0 {
		t.Errorf("Attachments = %v", d.Attachments)
	}
}

// htmlOnly verifies that a text/html-only message lands in the HTML
// field, not Text.
func TestParseHTMLOnly(t *testing.T) {
	raw := "From: a@example.test\r\nSubject: HTML\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>Hi</p>\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.HTML != "<p>Hi</p>\r\n" {
		t.Errorf("HTML = %q", d.HTML)
	}
	if d.Text != "" {
		t.Errorf("Text = %q, want empty for HTML-only", d.Text)
	}
}

// alternative covers multipart/alternative with text + html.
func TestParseAlternative(t *testing.T) {
	raw := "From: a@example.test\r\nSubject: Alt\r\n" +
		"Content-Type: multipart/alternative; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain body\r\n" +
		"--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>html body</p>\r\n" +
		"--B--\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(d.Text, "plain body") {
		t.Errorf("Text = %q", d.Text)
	}
	if !strings.Contains(d.HTML, "<p>html body</p>") {
		t.Errorf("HTML = %q", d.HTML)
	}
}

// mixed covers a multipart/mixed with one attachment.
func TestParseMixed(t *testing.T) {
	raw := "From: a@example.test\r\nSubject: Mixed\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"hello.bin\"\r\n" +
		"Content-ID: <cid@x>\r\n\r\nbinary contents\r\n" +
		"--B--\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(d.Text, "body") {
		t.Errorf("Text = %q", d.Text)
	}
	if len(d.Attachments) != 1 {
		t.Fatalf("len(Attachments) = %d, want 1", len(d.Attachments))
	}
	a := d.Attachments[0]
	if a.Filename != "hello.bin" {
		t.Errorf("Filename = %q", a.Filename)
	}
	if a.MimeType != "application/octet-stream" {
		t.Errorf("MimeType = %q", a.MimeType)
	}
	if a.ContentID != "cid@x" && a.ContentID != "<cid@x>" {
		t.Errorf("ContentID = %q", a.ContentID)
	}
	if a.Part != "2" {
		t.Errorf("Part = %q, want \"2\" (root=1, attachment=2)", a.Part)
	}
	if a.IsInline {
		t.Error("IsInline = true, want false for an attachment")
	}
	if a.Size == 0 {
		t.Error("Size = 0")
	}
}

// nested covers multipart/related inside multipart/alternative.
func TestParseNested(t *testing.T) {
	raw := "From: a@example.test\r\nSubject: Nested\r\n" +
		"Content-Type: multipart/mixed; boundary=A\r\n\r\n" +
		"--A\r\nContent-Type: multipart/related; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>html</p>\r\n" +
		"--B\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=\"logo.png\"\r\nContent-ID: <logo>\r\n\r\nPNG-DATA\r\n" +
		"--B--\r\n" +
		"--A--\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(d.HTML, "<p>html</p>") {
		t.Errorf("HTML = %q", d.HTML)
	}
	if len(d.Attachments) != 1 {
		t.Fatalf("len(Attachments) = %d, want 1", len(d.Attachments))
	}
	a := d.Attachments[0]
	if a.Filename != "logo.png" {
		t.Errorf("Filename = %q", a.Filename)
	}
	if !a.IsInline {
		t.Errorf("IsInline = false, want true for inline image")
	}
	// Part path: root=1, multipart/related at 1, image at 2 -> 1.2
	if a.Part != "1.2" {
		t.Errorf("Part = %q, want \"1.2\"", a.Part)
	}
}

// iso88591 verifies that a non-UTF-8 charset is decoded.
func TestParseISO88591(t *testing.T) {
	// "café" in ISO-8859-1 is the bytes: c(0x63) a(0x61) f(0x66) \xe9.
	raw := "From: a@example.test\r\nSubject: ISO\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n\r\n" +
		"caf\xe9\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(d.Text, "café") {
		t.Errorf("Text = %q, want decoded utf-8", d.Text)
	}
}

// utf8Header verifies RFC 2047 encoded-word decoding in headers.
func TestParseUTF8Header(t *testing.T) {
	// =?utf-8?B?...?= decodes to "café".
	raw = "From: =?utf-8?B?Y2Fmw6k=?= <a@example.test>\r\n" +
		"Subject: =?utf-8?Q?caf=C3=A9?=\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(d.Headers.From, "café") {
		t.Errorf("From = %q", d.Headers.From)
	}
	if !strings.Contains(d.Headers.Subject, "café") {
		t.Errorf("Subject = %q", d.Headers.Subject)
	}
}

// oversizedMessage ensures the upfront size cap rejects before any
// allocation.
func TestParseMessageTooLarge(t *testing.T) {
	huge := strings.Repeat("X", 1024)
	raw := "Subject: H\r\nContent-Type: text/plain\r\n\r\n" + huge
	_, err := Parse([]byte(raw), Limits{MaxMessageBytes: 100})
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("err = %v, want ErrMessageTooLarge", err)
	}
}

// attachmentTooLarge ensures the counting reader caps a single part
// at MaxAttachmentBytes.
func TestParseAttachmentTooLarge(t *testing.T) {
	body := strings.Repeat("A", 2048)
	raw := "Subject: A\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"big.bin\"\r\n\r\n" + body + "\r\n" +
		"--B--\r\n"
	_, err := Parse([]byte(raw), Limits{
		MaxMessageBytes:    1 << 20,
		MaxAttachmentBytes: 256,
	})
	if !errors.Is(err, ErrAttachmentTooLarge) {
		t.Errorf("err = %v, want ErrAttachmentTooLarge", err)
	}
}

// totalAttachmentTooLarge ensures the rolling sum is enforced.
func TestParseTotalAttachmentTooLarge(t *testing.T) {
	build := func(attSize int) string {
		body := strings.Repeat("A", attSize)
		return "Subject: T\r\n" +
			"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
			"--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"a.bin\"\r\n\r\n" + body + "\r\n" +
			"--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"b.bin\"\r\n\r\n" + body + "\r\n" +
			"--B--\r\n"
	}
	_, err := Parse([]byte(build(1024)), Limits{
		MaxMessageBytes:         1 << 20,
		MaxAttachmentBytes:      4096,
		MaxTotalAttachmentBytes: 1500,
	})
	if !errors.Is(err, ErrTotalAttachmentTooLarge) {
		t.Errorf("err = %v, want ErrTotalAttachmentTooLarge", err)
	}
}

// nestingTooDeep caps the multipart depth.
func TestParseNestingTooDeep(t *testing.T) {
	// Build a 20-level nested multipart. Default MaxMIMENesting is
	// 16, so depth 20 is rejected.
	inner := "Subject: N\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n"
	for i := 0; i < 20; i++ {
		// Each level wraps the previous in a multipart/mixed with
		// a unique boundary.
		boundary := "B" + strings.Repeat("x", i+1)
		inner = "Subject: N\r\n" +
			"Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n" +
			"--" + boundary + "\r\n" + inner + "\r\n--" + boundary + "--\r\n"
	}
	_, err := Parse([]byte(inner), Limits{MaxMIMENesting: 4, MaxMessageBytes: 1 << 20})
	if !errors.Is(err, ErrNestingTooDeep) {
		t.Errorf("err = %v, want ErrNestingTooDeep", err)
	}
}

// tooManyParts caps the total part count.
func TestParseTooManyParts(t *testing.T) {
	// 1000 attachments in one multipart/mixed.
	const N = 1000
	var b strings.Builder
	b.WriteString("Subject: P\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < N; i++ {
		b.WriteString("--B\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"a\"\r\n\r\nx\r\n")
	}
	b.WriteString("--B--\r\n")
	_, err := Parse([]byte(b.String()), Limits{MaxMIMEParts: 50, MaxMessageBytes: 1 << 20})
	if !errors.Is(err, ErrTooManyParts) {
		t.Errorf("err = %v, want ErrTooManyParts", err)
	}
}

// malformedInput is junk; the parser must return ErrMalformed
// rather than panicking.
func TestParseMalformed(t *testing.T) {
	_, err := Parse([]byte("not a real message"), Limits{})
	if err == nil {
		t.Fatal("expected error on junk input")
	}
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("err = %v, want ErrMalformed", err)
	}
}

// SanitizeFilename strips path components and NULs.
func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"hello.txt":            "hello.txt",
		"../etc/passwd":        "passwd",
		"..\\windows\\system":  "system",
		"file with space.txt":  "file with space.txt",
		"  leading-trailing  ": "leading-trailing",
		"embedded\x00null.txt": "embeddednull.txt",
		"":                     "",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCountingReaderBoundary exercises the boundary semantics of
// countingReader at exactly the cap, one below, and one above.
// The contract is:
//
//   - Reading exactly n bytes in total is the LAST successful
//     read; exceeded remains false.
//   - The next Read after reading exactly n bytes must return
//     io.EOF with exceeded = true.
//   - Reading n+1 bytes from the underlying reader with cap n
//     returns n bytes; the next Read returns 0, io.EOF with
//     exceeded = true.
func TestCountingReaderBoundary(t *testing.T) {
	const capN = 10
	source := strings.NewReader(strings.Repeat("A", 100))
	cr := &countingReader{r: source, n: capN}

	// First read returns 5 bytes (well below the cap).
	buf := make([]byte, 5)
	n, err := cr.Read(buf)
	if err != nil {
		t.Fatalf("first read err = %v", err)
	}
	if n != 5 {
		t.Errorf("first read n = %d, want 5", n)
	}
	if cr.exceeded {
		t.Errorf("exceeded = true after under-cap read")
	}

	// Second read takes us to exactly the cap boundary.
	n, err = cr.Read(buf)
	if err != nil {
		t.Fatalf("second read err = %v", err)
	}
	if n != capN-5 {
		t.Errorf("second read n = %d, want %d", n, capN-5)
	}
	if cr.exceeded {
		t.Errorf("exceeded = true after reading exactly cap bytes")
	}

	// The next read must report EOF and set exceeded.
	n, err = cr.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("over-cap read returned n=%d, err=%v; want 0, EOF", n, err)
	}
	if !cr.exceeded {
		t.Errorf("exceeded = false after refusing over-cap read")
	}
}

// TestCountingReaderUnderCap reads strictly below the cap and
// must never set exceeded.
func TestCountingReaderUnderCap(t *testing.T) {
	cr := &countingReader{r: strings.NewReader("hello"), n: 100}
	buf := make([]byte, 10)
	n, err := cr.Read(buf)
	if err != nil || n != 5 {
		t.Errorf("read n=%d err=%v, want 5, nil", n, err)
	}
	if cr.exceeded {
		t.Errorf("exceeded = true on a clean under-cap read")
	}
}

// TestCountingReaderAboveCap reads strictly above the cap; the
// reader returns cap bytes and the next read sets exceeded.
func TestCountingReaderAboveCap(t *testing.T) {
	cr := &countingReader{r: strings.NewReader(strings.Repeat("X", 1000)), n: 50}
	all, err := io.ReadAll(cr)
	if err != nil {
		t.Fatalf("ReadAll err = %v", err)
	}
	if len(all) != 50 {
		t.Errorf("read %d bytes, want exactly 50 (the cap)", len(all))
	}
	if !cr.exceeded {
		t.Errorf("exceeded = false after the cap was hit")
	}
}

// TestParseAttachmentTooLargeReturnsPartial verifies that a hard
// failure during Parse returns BOTH the partial decode (the parts
// we already parsed before hitting the cap) AND the typed error.
// The Truncated flag is set so callers cannot mistake the
// partial decode for a complete read.
func TestParseAttachmentTooLargeReturnsPartial(t *testing.T) {
	// Build a multipart/mixed with two attachments: a small
	// one (ok) and a large one (rejected by the cap).
	big := strings.Repeat("A", 4096)
	raw := "From: a@example.test\r\nSubject: T\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"small.bin\"\r\n\r\n" +
		"small\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"big.bin\"\r\n\r\n" +
		big + "\r\n" +
		"--B--\r\n"
	d, err := Parse([]byte(raw), Limits{
		MaxMessageBytes:    1 << 20,
		MaxAttachmentBytes: 256,
	})
	if !errors.Is(err, ErrAttachmentTooLarge) {
		t.Fatalf("err = %v, want ErrAttachmentTooLarge", err)
	}
	if d == nil {
		t.Fatal("Parse returned nil decode on hard failure; want partial")
	}
	if !d.Truncated {
		t.Error("Truncated = false on partial decode; want true")
	}
	// The first attachment was captured; the second was the
	// point of failure.
	if len(d.Attachments) != 1 {
		t.Errorf("len(Attachments) = %d, want 1 (partial)", len(d.Attachments))
	}
	if len(d.Attachments) >= 1 && d.Attachments[0].Filename != "small.bin" {
		t.Errorf("partial Filename = %q, want small.bin", d.Attachments[0].Filename)
	}
}

// TestParseTooManyAttachmentsReturnsPartial is the count-cap
// counterpart: the parser stops at MaxAttachments and returns
// the partial decode with a typed error.
func TestParseTooManyAttachmentsReturnsPartial(t *testing.T) {
	var b strings.Builder
	b.WriteString("From: a@example.test\r\nSubject: A\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < 5; i++ {
		b.WriteString("--B\r\nContent-Type: application/octet-stream\r\n" +
			"Content-Disposition: attachment; filename=\"x.bin\"\r\n\r\nx\r\n")
	}
	b.WriteString("--B--\r\n")
	d, err := Parse([]byte(b.String()), Limits{
		MaxMessageBytes: 1 << 20,
		MaxAttachments:  2,
	})
	if !errors.Is(err, ErrTooManyAttachments) {
		t.Fatalf("err = %v, want ErrTooManyAttachments", err)
	}
	if d == nil {
		t.Fatal("nil partial decode")
	}
	if !d.Truncated {
		t.Error("Truncated = false; want true")
	}
	if len(d.Attachments) != 2 {
		t.Errorf("len(Attachments) = %d, want 2 (partial)", len(d.Attachments))
	}
}

// TestParseNestingTooDeepReturnsPartial verifies the partial
// behaviour for the nesting cap.
func TestParseNestingTooDeepReturnsPartial(t *testing.T) {
	// Build a 20-level nested multipart. We then set
	// MaxMIMENesting to 2; the first 2 levels will be walked,
	// the third returns ErrNestingTooDeep. The result must be
	// a partial decode with Truncated.
	inner := "From: a@example.test\r\nSubject: D\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n"
	for i := 0; i < 5; i++ {
		boundary := "B" + strings.Repeat("x", i+1)
		inner = "From: a@example.test\r\nSubject: D\r\n" +
			"Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n" +
			"--" + boundary + "\r\n" + inner + "\r\n--" + boundary + "--\r\n"
	}
	d, err := Parse([]byte(inner), Limits{
		MaxMessageBytes: 1 << 20,
		MaxMIMENesting:  2,
	})
	if !errors.Is(err, ErrNestingTooDeep) {
		t.Fatalf("err = %v, want ErrNestingTooDeep", err)
	}
	if d == nil {
		t.Fatal("nil partial decode")
	}
	if !d.Truncated {
		t.Error("Truncated = false; want true")
	}
}

// TestParseInlineContentIDWithoutFilename is a classification
// regression: a multipart/related part with Content-Disposition:
// inline but no filename and a Content-ID must be retained as
// an inline attachment so the agent can resolve cid: references
// in the HTML body. Before the fix, such parts were silently
// dropped.
func TestParseInlineContentIDWithoutFilename(t *testing.T) {
	raw := "From: a@example.test\r\nSubject: CID\r\n" +
		"Content-Type: multipart/related; boundary=R\r\n\r\n" +
		"--R\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>see <img src=\"cid:logo\"/></p>\r\n" +
		"--R\r\nContent-Type: image/png\r\n" +
		"Content-ID: <logo>\r\n" +
		"Content-Disposition: inline\r\n\r\nPNG-DATA\r\n" +
		"--R--\r\n"
	d, err := Parse([]byte(raw), Limits{MaxMessageBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Attachments) != 1 {
		t.Fatalf("len(Attachments) = %d, want 1", len(d.Attachments))
	}
	a := d.Attachments[0]
	if !a.IsInline {
		t.Errorf("IsInline = false; want true")
	}
	if a.ContentID != "logo" {
		t.Errorf("ContentID = %q, want %q", a.ContentID, "logo")
	}
	if a.Filename != "" {
		t.Errorf("Filename = %q, want empty (the part has no filename)", a.Filename)
	}
	if a.MimeType != "image/png" {
		t.Errorf("MimeType = %q", a.MimeType)
	}
}

// TestParseTextPlainAttachment is a classification regression:
// text/plain with Content-Disposition: attachment and a filename
// must be classified as an attachment, not the message body.
// Before the fix, such parts landed in the Text field.
func TestParseTextPlainAttachment(t *testing.T) {
	raw := "From: a@example.test\r\nSubject: ATT\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nreal body\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n\r\nattached note\r\n" +
		"--B--\r\n"
	d, err := Parse([]byte(raw), Limits{MaxMessageBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(d.Text, "real body") {
		t.Errorf("Text = %q, want it to contain %q", d.Text, "real body")
	}
	if strings.Contains(d.Text, "attached note") {
		t.Errorf("Text leaked attached content: %q", d.Text)
	}
	if len(d.Attachments) != 1 {
		t.Fatalf("len(Attachments) = %d, want 1", len(d.Attachments))
	}
	if d.Attachments[0].Filename != "note.txt" {
		t.Errorf("Filename = %q", d.Attachments[0].Filename)
	}
	if d.Attachments[0].MimeType != "text/plain" {
		t.Errorf("MimeType = %q", d.Attachments[0].MimeType)
	}
	if !d.Attachments[0].IsText {
		t.Errorf("IsText = false; want true for text/plain attachment")
	}
}

// TestParseHTMLAttachment is the HTML counterpart of
// TestParseTextPlainAttachment.
func TestParseHTMLAttachment(t *testing.T) {
	raw := "From: a@example.test\r\nSubject: HTMLATT\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>real body</p>\r\n" +
		"--B\r\nContent-Type: text/html; charset=utf-8\r\n" +
		"Content-Disposition: attachment; filename=\"page.html\"\r\n\r\n<p>attached</p>\r\n" +
		"--B--\r\n"
	d, err := Parse([]byte(raw), Limits{MaxMessageBytes: 1 << 20})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(d.HTML, "real body") {
		t.Errorf("HTML = %q, want it to contain %q", d.HTML, "real body")
	}
	if len(d.Attachments) != 1 || d.Attachments[0].Filename != "page.html" {
		t.Errorf("Attachments = %+v, want one entry with Filename=page.html", d.Attachments)
	}
}

// The cap is "applied" only when c.n > 0; a cap of 0 must
// behave like an unlimited read.
func TestCountingReaderZeroCap(t *testing.T) {
	cr := &countingReader{r: strings.NewReader("xyz"), n: 0}
	all, err := io.ReadAll(cr)
	if err != nil {
		t.Fatalf("ReadAll err = %v", err)
	}
	if string(all) != "xyz" {
		t.Errorf("read %q, want %q", string(all), "xyz")
	}
	if cr.exceeded {
		t.Errorf("exceeded = true with cap 0")
	}
}

// unknownCharset produces a soft warning, not an error.
func TestParseUnknownCharset(t *testing.T) {
	raw := "Subject: U\r\nContent-Type: text/plain; charset=zz-not-real\r\n\r\nbody\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		// Some versions of go-message may surface a hard error; we
		// accept either a soft warning in d.Warnings or a typed
		// error here, but never a panic.
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("err = %v, want ErrMalformed or nil", err)
		}
		return
	}
	if len(d.Warnings) == 0 {
		t.Errorf("expected at least one warning for unknown charset, got none")
	}
}

// missingContentType is treated as text/plain in us-ascii.
func TestParseMissingContentType(t *testing.T) {
	raw := "Subject: M\r\n\r\nbody only\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Text != "body only\r\n" {
		t.Errorf("Text = %q", d.Text)
	}
}

// MIMEInfo returns the top-level Content-Type without walking the
// body.
func TestMIMEInfo(t *testing.T) {
	raw := []byte("Subject: I\r\nContent-Type: text/html; charset=utf-8\r\n\r\nbody\r\n")
	mt, params, err := MIMEInfo(raw)
	if err != nil {
		t.Fatalf("MIMEInfo: %v", err)
	}
	if mt != "text/html" {
		t.Errorf("mediaType = %q", mt)
	}
	if params["charset"] != "utf-8" {
		t.Errorf("charset = %q", params["charset"])
	}
}

// TestNoRawRFC822AsText ensures that the parser never returns raw
// RFC822 bytes (headers + boundary markers) as the Text or HTML
// field. The Text field must contain the decoded body, not the
// entire RFC 5322 message.
func TestNoRawRFC822AsText(t *testing.T) {
	raw := "From: a@example.test\r\nSubject: Mixed\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nreal body content\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"x.bin\"\r\n\r\nBINARY-DATA\r\n" +
		"--B--\r\n"
	d, err := Parse([]byte(raw), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// The Text field must contain only the text body, not the
	// headers or the multipart boilerplate.
	if strings.Contains(d.Text, "Content-Type") ||
		strings.Contains(d.Text, "Content-Disposition") ||
		strings.Contains(d.Text, "multipart/mixed") ||
		strings.Contains(d.Text, "BINARY-DATA") ||
		strings.Contains(d.Text, "--B") {
		t.Errorf("Text = %q, want only the decoded text body", d.Text)
	}
	// The HTML field must also be free of the raw boilerplate.
	if strings.Contains(d.HTML, "Content-Type") || strings.Contains(d.HTML, "--B") {
		t.Errorf("HTML = %q, want empty or only HTML body", d.HTML)
	}
}

// raw is reassigned across tests; this keeps the linter happy and
// documents that the field is intentionally mutable.
var raw = ""
