package mailcompose

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-message"

	"github.com/flowbyte-com/themailroom/internal/attachments"
	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
)

var fixedTime = time.Date(2026, time.October, 20, 14, 30, 0, 0, time.UTC)

var testLimits = config.LimitsConfig{
	MaxMessageBytes:         1 << 20,
	MaxAttachmentBytes:      1 << 20,
	MaxTotalAttachmentBytes: 2 << 20,
	MaxAttachments:          10,
}

var parseLimits = mimeparse.Limits{
	MaxMessageBytes:         1 << 20,
	MaxAttachmentBytes:      1 << 20,
	MaxTotalAttachmentBytes: 2 << 20,
	MaxAttachments:          20,
	MaxMIMENesting:          16,
	MaxMIMEParts:            100,
}

func fixedCompose(t *testing.T, req Request, limits config.LimitsConfig) (Result, error) {
	t.Helper()
	return compose(req, limits, func() time.Time { return fixedTime }, func(_ time.Time, domain string) (string, error) {
		return "fixed-id@" + domain, nil
	})
}

type partInfo struct {
	path             string
	mediaType        string
	charset          string
	transferEncoding string
	disposition      string
	filename         string
}

func inspectParts(t *testing.T, raw []byte) []partInfo {
	t.Helper()
	entity, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("message.Read: %v", err)
	}
	var parts []partInfo
	if err := entity.Walk(func(path []int, part *message.Entity, err error) error {
		if err != nil {
			return err
		}
		mediaType, mediaParams, _ := part.Header.ContentType()
		disposition, dispositionParams, _ := part.Header.ContentDisposition()
		parts = append(parts, partInfo{
			path:             renderPartPath(path),
			mediaType:        mediaType,
			charset:          mediaParams["charset"],
			transferEncoding: part.Header.Get("Content-Transfer-Encoding"),
			disposition:      disposition,
			filename:         dispositionParams["filename"],
		})
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return parts
}

func renderPartPath(path []int) string {
	if len(path) == 0 {
		return "1"
	}
	out := make([]string, len(path))
	for i, n := range path {
		out[i] = strconv.Itoa(n + 1)
	}
	return strings.Join(out, ".")
}

func parseDecoded(t *testing.T, raw []byte) *mimeparse.Decoded {
	t.Helper()
	decoded, err := mimeparse.Parse(raw, parseLimits)
	if err != nil {
		t.Fatalf("mimeparse.Parse: %v", err)
	}
	return decoded
}

func mustCompose(t *testing.T, req Request, limits config.LimitsConfig) Result {
	t.Helper()
	result, err := fixedCompose(t, req, limits)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(result.Message) == 0 {
		t.Fatal("Compose returned an empty message")
	}
	if result.MessageID == "" {
		t.Fatal("Compose returned an empty Message-ID")
	}
	return result
}

func baseRequest() Request {
	return Request{
		From:    "Sales@Example.TEST",
		To:      []string{"alice@example.test"},
		Subject: "Subject",
		Text:    "Hello world.",
	}
}

func TestPlainText(t *testing.T) {
	req := baseRequest()
	result := mustCompose(t, req, testLimits)
	if len(result.Message) > testLimits.MaxMessageBytes {
		t.Fatalf("message size %d exceeds limit", len(result.Message))
	}

	decoded := parseDecoded(t, result.Message)
	if decoded.Text != "Hello world." {
		t.Errorf("Text = %q", decoded.Text)
	}
	if decoded.HTML != "" {
		t.Errorf("HTML = %q, want empty", decoded.HTML)
	}
	if len(decoded.Attachments) != 0 {
		t.Errorf("Attachments = %v", decoded.Attachments)
	}
	if decoded.Headers.From != "Sales@example.test" {
		t.Errorf("From = %q", decoded.Headers.From)
	}
	if len(decoded.Headers.To) != 1 || decoded.Headers.To[0] != "alice@example.test" {
		t.Errorf("To = %v", decoded.Headers.To)
	}
	if decoded.Headers.Subject != "Subject" {
		t.Errorf("Subject = %q", decoded.Headers.Subject)
	}
	if !decoded.Headers.Date.Equal(fixedTime) {
		t.Errorf("Date = %v, want %v", decoded.Headers.Date, fixedTime)
	}
	if decoded.Headers.MessageID != "fixed-id@example.test" {
		t.Errorf("MessageID = %q", decoded.Headers.MessageID)
	}

	parts := inspectParts(t, result.Message)
	if len(parts) != 1 || parts[0].mediaType != "text/plain" {
		t.Fatalf("parts = %+v, want one text/plain part", parts)
	}
	if parts[0].charset != "utf-8" || parts[0].transferEncoding != "quoted-printable" {
		t.Errorf("text part metadata = %+v", parts[0])
	}
}

func TestHTMLOnly(t *testing.T) {
	req := baseRequest()
	req.Text = ""
	req.HTML = "<p>café</p>"
	result := mustCompose(t, req, testLimits)

	decoded := parseDecoded(t, result.Message)
	if decoded.Text != "" {
		t.Errorf("Text = %q, want empty", decoded.Text)
	}
	if decoded.HTML != "<p>café</p>" {
		t.Errorf("HTML = %q", decoded.HTML)
	}
	parts := inspectParts(t, result.Message)
	if len(parts) != 1 || parts[0].mediaType != "text/html" {
		t.Fatalf("parts = %+v, want one text/html part", parts)
	}
}

func TestAlternative(t *testing.T) {
	req := baseRequest()
	req.Text = "plain body"
	req.HTML = "<p>HTML body</p>"
	result := mustCompose(t, req, testLimits)

	decoded := parseDecoded(t, result.Message)
	if decoded.Text != "plain body" {
		t.Errorf("Text = %q", decoded.Text)
	}
	if decoded.HTML != "<p>HTML body</p>" {
		t.Errorf("HTML = %q", decoded.HTML)
	}
	parts := inspectParts(t, result.Message)
	want := []struct {
		mediaType string
		path      string
	}{
		{mediaType: "multipart/alternative", path: "1"},
		{mediaType: "text/plain", path: "1"},
		{mediaType: "text/html", path: "2"},
	}
	if len(parts) != len(want) {
		t.Fatalf("parts = %+v", parts)
	}
	for i := range want {
		if parts[i].mediaType != want[i].mediaType || parts[i].path != want[i].path {
			t.Errorf("part[%d] = %+v, want mediaType=%q path=%q", i, parts[i], want[i].mediaType, want[i].path)
		}
	}
	for _, i := range []int{1, 2} {
		if parts[i].charset != "utf-8" || parts[i].transferEncoding != "quoted-printable" {
			t.Errorf("body part[%d] metadata = %+v", i, parts[i])
		}
	}
}

func TestMixedWithAlternativeAndAttachments(t *testing.T) {
	req := baseRequest()
	req.Text = "plain body"
	req.HTML = "<p>HTML body</p>"
	req.Bcc = []string{"hidden@example.test"}
	req.Attachments = []attachments.File{
		{Filename: "data.bin", Data: []byte{0x00, 0x01, 0x02, 0xff, 0x80}},
		{Filename: "notes.txt", Data: []byte("attachment text")},
	}
	result := mustCompose(t, req, testLimits)

	parts := inspectParts(t, result.Message)
	want := []struct {
		mediaType   string
		path        string
		disposition string
	}{
		{mediaType: "multipart/mixed", path: "1"},
		{mediaType: "multipart/alternative", path: "1"},
		{mediaType: "text/plain", path: "1.1"},
		{mediaType: "text/html", path: "1.2"},
		{mediaType: "application/octet-stream", path: "2", disposition: "attachment"},
		{mediaType: "text/plain", path: "3", disposition: "attachment"},
	}
	if len(parts) != len(want) {
		t.Fatalf("parts = %+v", parts)
	}
	for i := range want {
		if parts[i].mediaType != want[i].mediaType ||
			parts[i].path != want[i].path ||
			parts[i].disposition != want[i].disposition {
			t.Errorf("part[%d] = %+v, want mediaType=%q path=%q disposition=%q",
				i, parts[i], want[i].mediaType, want[i].path, want[i].disposition)
		}
	}
	for _, i := range []int{2, 3} {
		if parts[i].charset != "utf-8" || parts[i].transferEncoding != "quoted-printable" {
			t.Errorf("body part[%d] metadata = %+v", i, parts[i])
		}
	}
	for _, i := range []int{4, 5} {
		if parts[i].transferEncoding != "base64" {
			t.Errorf("attachment part[%d] transfer encoding = %q", i, parts[i].transferEncoding)
		}
	}

	decoded := parseDecoded(t, result.Message)
	if decoded.Text != "plain body" || decoded.HTML != "<p>HTML body</p>" {
		t.Errorf("decoded bodies = Text %q HTML %q", decoded.Text, decoded.HTML)
	}
	if len(decoded.Attachments) != 2 {
		t.Fatalf("attachments = %+v", decoded.Attachments)
	}
	for i, wantFile := range req.Attachments {
		attachment := decoded.Attachments[i]
		if attachment.Filename != wantFile.Filename {
			t.Errorf("attachment[%d].Filename = %q, want %q", i, attachment.Filename, wantFile.Filename)
		}
		content, err := mimeparse.ExtractAttachment(result.Message, attachment.Part, parseLimits)
		if err != nil {
			t.Fatalf("ExtractAttachment(%q): %v", attachment.Part, err)
		}
		if !bytes.Equal(content.Content, wantFile.Data) {
			t.Errorf("attachment[%d] content = %v, want %v", i, content.Content, wantFile.Data)
		}
	}

	entity, err := message.Read(bytes.NewReader(result.Message))
	if err != nil {
		t.Fatalf("message.Read: %v", err)
	}
	if got := entity.Header.Get("Bcc"); got != "" {
		t.Errorf("Bcc header leaked: %q", got)
	}
}

func TestAttachmentsRoundTrip(t *testing.T) {
	binary := make([]byte, 0, 512)
	for i := 0; i < 512; i++ {
		binary = append(binary, byte(i))
	}
	req := baseRequest()
	req.Text = "body"
	req.HTML = ""
	req.Attachments = []attachments.File{
		{Filename: "empty.txt", Data: []byte{}},
		{Filename: "binary.bin", Data: binary},
		{Filename: "résumé 2026.txt", Data: []byte("café")},
		{Filename: `semi;colon,quote"and space.txt`, Data: []byte("special")},
		{Filename: "same.txt", Data: []byte("first")},
		{Filename: "same.txt", Data: []byte("second")},
	}
	result := mustCompose(t, req, testLimits)

	decoded := parseDecoded(t, result.Message)
	if len(decoded.Attachments) != len(req.Attachments) {
		t.Fatalf("attachments = %+v", decoded.Attachments)
	}
	for i, want := range req.Attachments {
		got := decoded.Attachments[i]
		if got.Filename != want.Filename {
			t.Errorf("attachment[%d].Filename = %q, want %q", i, got.Filename, want.Filename)
		}
		if got.Size != int64(len(want.Data)) {
			t.Errorf("attachment[%d].Size = %d, want %d", i, got.Size, len(want.Data))
		}
		content, err := mimeparse.ExtractAttachment(result.Message, got.Part, parseLimits)
		if err != nil {
			t.Fatalf("ExtractAttachment(%q): %v", got.Part, err)
		}
		if !bytes.Equal(content.Content, want.Data) {
			t.Errorf("attachment[%d] content = %v, want %v", i, content.Content, want.Data)
		}
	}

	parts := inspectParts(t, result.Message)
	for _, part := range parts {
		if part.mediaType == "application/octet-stream" && part.filename != "binary.bin" {
			t.Errorf("unexpected octet-stream filename %q", part.filename)
		}
	}
}

func TestHTMLOnlyWithAttachment(t *testing.T) {
	req := baseRequest()
	req.Text = ""
	req.HTML = "<p>HTML body</p>"
	req.Attachments = []attachments.File{{Filename: "notes.txt", Data: []byte("notes")}}
	result := mustCompose(t, req, testLimits)

	parts := inspectParts(t, result.Message)
	if len(parts) != 3 {
		t.Fatalf("parts = %+v", parts)
	}
	if parts[0].mediaType != "multipart/mixed" ||
		parts[1].mediaType != "text/html" ||
		parts[2].mediaType != "text/plain" ||
		parts[2].disposition != "attachment" {
		t.Errorf("parts = %+v", parts)
	}
	decoded := parseDecoded(t, result.Message)
	if decoded.Text != "" || decoded.HTML != "<p>HTML body</p>" {
		t.Errorf("decoded bodies = Text %q HTML %q", decoded.Text, decoded.HTML)
	}
	if len(decoded.Attachments) != 1 {
		t.Fatalf("attachments = %+v", decoded.Attachments)
	}
	content, err := mimeparse.ExtractAttachment(result.Message, decoded.Attachments[0].Part, parseLimits)
	if err != nil {
		t.Fatal(err)
	}
	if string(content.Content) != "notes" {
		t.Errorf("attachment content = %q", content.Content)
	}
}

func TestHeadersAndBccOmission(t *testing.T) {
	req := baseRequest()
	req.From = "Sales@Example.TEST"
	req.To = []string{"ToUser@Example.TEST"}
	req.Cc = []string{"CcUser@Example.TEST"}
	req.Bcc = []string{"BccUser@Example.TEST"}
	req.Subject = "こんにちは café"
	req.Text = "body"
	req.HTML = "<p>body</p>"
	req.Attachments = []attachments.File{{Filename: "a.bin", Data: []byte{1, 2, 3}}}
	result := mustCompose(t, req, testLimits)

	entity, err := message.Read(bytes.NewReader(result.Message))
	if err != nil {
		t.Fatalf("message.Read: %v", err)
	}
	if got := entity.Header.Values("MIME-Version"); len(got) != 1 || got[0] != "1.0" {
		t.Errorf("MIME-Version = %v", got)
	}
	if got := entity.Header.Get("Date"); got != "Tue, 20 Oct 2026 14:30:00 +0000" {
		t.Errorf("Date = %q", got)
	}
	if got := entity.Header.Values("Message-Id"); len(got) != 1 || got[0] != "<fixed-id@example.test>" {
		t.Errorf("Message-Id = %v", got)
	}
	if got := entity.Header.Values("Bcc"); len(got) != 0 {
		t.Errorf("Bcc header leaked: %v", got)
	}

	decoded := parseDecoded(t, result.Message)
	if decoded.Headers.Subject != req.Subject {
		t.Errorf("Subject = %q, want %q", decoded.Headers.Subject, req.Subject)
	}
	if decoded.Headers.From != "Sales@example.test" {
		t.Errorf("From = %q", decoded.Headers.From)
	}
	if len(decoded.Headers.To) != 1 || decoded.Headers.To[0] != "ToUser@example.test" {
		t.Errorf("To = %v", decoded.Headers.To)
	}
	if len(decoded.Headers.Cc) != 1 || decoded.Headers.Cc[0] != "CcUser@example.test" {
		t.Errorf("Cc = %v", decoded.Headers.Cc)
	}
}

func TestHeaderInjectionRejected(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want error
	}{
		{
			name: "subject CRLF",
			req:  Request{From: "a@example.test", To: []string{"b@example.test"}, Subject: "ok\r\nBcc: attacker@example.test", Text: "body"},
			want: ErrInvalidMessage,
		},
		{
			name: "subject bare LF",
			req:  Request{From: "a@example.test", To: []string{"b@example.test"}, Subject: "ok\nBcc: attacker@example.test", Text: "body"},
			want: ErrInvalidMessage,
		},
		{
			name: "from CRLF",
			req:  Request{From: "a@example.test\r\nBcc: attacker@example.test", To: []string{"b@example.test"}, Subject: "ok", Text: "body"},
			want: ErrInvalidMessage,
		},
		{
			name: "recipient CRLF",
			req:  Request{From: "a@example.test", To: []string{"b@example.test\r\nBcc: attacker@example.test"}, Subject: "ok", Text: "body"},
			want: ErrInvalidMessage,
		},
		{
			name: "filename CRLF",
			req: Request{
				From: "a@example.test", To: []string{"b@example.test"}, Subject: "ok", Text: "body",
				Attachments: []attachments.File{{Filename: "a\r\nb.txt", Data: []byte("x")}},
			},
			want: ErrInvalidAttachment,
		},
		{
			name: "filename traversal",
			req: Request{
				From: "a@example.test", To: []string{"b@example.test"}, Subject: "ok", Text: "body",
				Attachments: []attachments.File{{Filename: "../a.txt", Data: []byte("x")}},
			},
			want: ErrInvalidAttachment,
		},
		{
			name: "filename absolute",
			req: Request{
				From: "a@example.test", To: []string{"b@example.test"}, Subject: "ok", Text: "body",
				Attachments: []attachments.File{{Filename: "/etc/passwd", Data: []byte("x")}},
			},
			want: ErrInvalidAttachment,
		},
		{
			name: "filename empty",
			req: Request{
				From: "a@example.test", To: []string{"b@example.test"}, Subject: "ok", Text: "body",
				Attachments: []attachments.File{{Filename: "", Data: []byte("x")}},
			},
			want: ErrInvalidAttachment,
		},
		{
			name: "filename invalid utf8",
			req: Request{
				From: "a@example.test", To: []string{"b@example.test"}, Subject: "ok", Text: "body",
				Attachments: []attachments.File{{Filename: "bad\xff.txt", Data: []byte("x")}},
			},
			want: ErrInvalidAttachment,
		},
		{
			name: "subject invalid utf8",
			req:  Request{From: "a@example.test", To: []string{"b@example.test"}, Subject: "bad\xff", Text: "body"},
			want: ErrInvalidMessage,
		},
		{
			name: "body invalid utf8",
			req:  Request{From: "a@example.test", To: []string{"b@example.test"}, Subject: "ok", Text: "bad\xff"},
			want: ErrInvalidMessage,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := fixedCompose(t, tc.req, testLimits)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(result.Message) != 0 || result.MessageID != "" {
				t.Errorf("failure returned partial result: %+v", result)
			}
		})
	}
}

func TestAttachmentAndMessageSizeBoundaries(t *testing.T) {
	data := []byte("12345")
	req := baseRequest()
	req.Text = "body"
	req.Attachments = []attachments.File{{Filename: "a.bin", Data: data}}

	limits := testLimits
	limits.MaxAttachmentBytes = len(data)
	limits.MaxTotalAttachmentBytes = len(data)
	limits.MaxAttachments = 1
	mustCompose(t, req, limits)

	tooLargeData := append(append([]byte(nil), data...), 0)
	tooLargeReq := req
	tooLargeReq.Attachments = []attachments.File{{Filename: "a.bin", Data: tooLargeData}}
	if _, err := fixedCompose(t, tooLargeReq, limits); !errors.Is(err, ErrAttachmentTooLarge) {
		t.Errorf("per-file err = %v, want ErrAttachmentTooLarge", err)
	}

	totalReq := req
	totalReq.Attachments = []attachments.File{
		{Filename: "a.bin", Data: data},
		{Filename: "b.bin", Data: data},
	}
	totalLimits := testLimits
	totalLimits.MaxAttachmentBytes = len(data)
	totalLimits.MaxTotalAttachmentBytes = 2 * len(data)
	totalLimits.MaxAttachments = 2
	if _, err := fixedCompose(t, totalReq, totalLimits); err != nil {
		t.Fatalf("exact total failed: %v", err)
	}
	totalLimits.MaxTotalAttachmentBytes = 2*len(data) - 1
	if _, err := fixedCompose(t, totalReq, totalLimits); !errors.Is(err, ErrTotalAttachmentTooLarge) {
		t.Errorf("total err = %v, want ErrTotalAttachmentTooLarge", err)
	}

	tooManyReq := req
	tooManyReq.Attachments = []attachments.File{
		{Filename: "a.bin", Data: data},
		{Filename: "b.bin", Data: data},
		{Filename: "c.bin", Data: data},
	}
	tooManyLimits := testLimits
	tooManyLimits.MaxAttachments = 2
	if _, err := fixedCompose(t, tooManyReq, tooManyLimits); !errors.Is(err, ErrTooManyAttachments) {
		t.Errorf("too many err = %v, want ErrTooManyAttachments", err)
	}

	// Find the exact serialized size, then test both sides of the boundary.
	fullReq := baseRequest()
	fullReq.Text = "body"
	fullReq.Attachments = []attachments.File{{Filename: "a.bin", Data: data}}
	baseline := mustCompose(t, fullReq, testLimits)
	exactLimits := testLimits
	exactLimits.MaxMessageBytes = len(baseline.Message)
	exact := mustCompose(t, fullReq, exactLimits)
	if len(exact.Message) != len(baseline.Message) {
		t.Fatalf("deterministic message sizes differ: %d vs %d", len(exact.Message), len(baseline.Message))
	}
	exactLimits.MaxMessageBytes = len(baseline.Message) - 1
	failed, err := fixedCompose(t, fullReq, exactLimits)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("exact-minus-one err = %v, want ErrMessageTooLarge", err)
	}
	if len(failed.Message) != 0 || failed.MessageID != "" {
		t.Errorf("size failure returned partial result: %+v", failed)
	}
}

func TestBase64ExpansionExceedsMessageLimit(t *testing.T) {
	data := bytes.Repeat([]byte{0xab}, 1024)
	req := baseRequest()
	req.Text = ""
	req.Attachments = []attachments.File{{Filename: "blob.bin", Data: data}}
	limits := testLimits
	limits.MaxAttachmentBytes = len(data)
	limits.MaxTotalAttachmentBytes = len(data)
	limits.MaxAttachments = 1
	limits.MaxMessageBytes = 1200

	result, err := fixedCompose(t, req, limits)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
	if len(result.Message) != 0 || result.MessageID != "" {
		t.Errorf("failure returned partial result: %+v", result)
	}
}

func TestInvalidLimits(t *testing.T) {
	req := baseRequest()
	for name, limits := range map[string]config.LimitsConfig{
		"message zero":     {MaxAttachmentBytes: 1, MaxTotalAttachmentBytes: 1, MaxAttachments: 1},
		"message negative": {MaxMessageBytes: -1, MaxAttachmentBytes: 1, MaxTotalAttachmentBytes: 1, MaxAttachments: 1},
		"file negative":    {MaxMessageBytes: 1, MaxAttachmentBytes: -1, MaxTotalAttachmentBytes: 1, MaxAttachments: 1},
		"total negative":   {MaxMessageBytes: 1, MaxAttachmentBytes: 1, MaxTotalAttachmentBytes: -1, MaxAttachments: 1},
		"count negative":   {MaxMessageBytes: 1, MaxAttachmentBytes: 1, MaxTotalAttachmentBytes: 1, MaxAttachments: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fixedCompose(t, req, limits); !errors.Is(err, ErrInvalidLimits) {
				t.Errorf("err = %v, want ErrInvalidLimits", err)
			}
		})
	}
}

func TestInvalidMessageAndMIMEFailures(t *testing.T) {
	missingFrom := baseRequest()
	missingFrom.From = ""
	result, err := fixedCompose(t, missingFrom, testLimits)
	if !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("missing From err = %v, want ErrInvalidMessage", err)
	}
	if len(result.Message) != 0 || result.MessageID != "" {
		t.Errorf("missing From returned partial result: %+v", result)
	}

	overlong := baseRequest()
	overlong.To = []string{strings.Repeat("a", 901) + "@example.test"}
	result, err = fixedCompose(t, overlong, testLimits)
	if !errors.Is(err, ErrInvalidMessage) {
		t.Errorf("overlong address err = %v, want ErrInvalidMessage", err)
	}
	if len(result.Message) != 0 || result.MessageID != "" {
		t.Errorf("overlong address returned partial result: %+v", result)
	}

	req := baseRequest()
	req.Subject = "sensitive subject\r\nBcc: attacker@example.test"
	_, err = fixedCompose(t, req, testLimits)
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("injection err = %v, want ErrInvalidMessage", err)
	}
	if strings.Contains(err.Error(), "attacker@example.test") || strings.Contains(err.Error(), "sensitive subject") {
		t.Errorf("error leaked untrusted header input: %v", err)
	}

	idFailure, err := compose(baseRequest(), testLimits, func() time.Time { return fixedTime }, func(time.Time, string) (string, error) {
		return "", errors.New("entropy unavailable")
	})
	if !errors.Is(err, ErrMIME) {
		t.Errorf("message-id failure err = %v, want ErrMIME", err)
	}
	if len(idFailure.Message) != 0 || idFailure.MessageID != "" {
		t.Errorf("message-id failure returned partial result: %+v", idFailure)
	}

	zeroTime, err := compose(baseRequest(), testLimits, func() time.Time { return time.Time{} }, func(_ time.Time, domain string) (string, error) {
		return "fixed-id@" + domain, nil
	})
	if !errors.Is(err, ErrMIME) {
		t.Errorf("zero timestamp err = %v, want ErrMIME", err)
	}
	if len(zeroTime.Message) != 0 || zeroTime.MessageID != "" {
		t.Errorf("zero timestamp returned partial result: %+v", zeroTime)
	}
}

func TestConcurrentCompose(t *testing.T) {
	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	ids := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := baseRequest()
			req.Subject = fmt.Sprintf("subject-%d", i)
			req.Text = fmt.Sprintf("body-%d", i)
			result, err := Compose(req, testLimits)
			if err != nil {
				errs <- err
				return
			}
			if len(result.Message) == 0 || result.MessageID == "" {
				errs <- errors.New("empty result")
				return
			}
			if _, err := mimeparse.Parse(result.Message, parseLimits); err != nil {
				errs <- err
				return
			}
			ids <- result.MessageID
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Errorf("concurrent Compose: %v", err)
	}
	seen := make(map[string]struct{}, workers)
	for id := range ids {
		if _, duplicate := seen[id]; duplicate {
			t.Errorf("duplicate message-id %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestRepeatedComposeIsIndependent(t *testing.T) {
	req := baseRequest()
	first, err := fixedCompose(t, req, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixedCompose(t, req, testLimits)
	if err != nil {
		t.Fatal(err)
	}
	if first.MessageID != second.MessageID {
		t.Errorf("fixed Message-ID changed: %q vs %q", first.MessageID, second.MessageID)
	}
	if _, err := mimeparse.Parse(first.Message, parseLimits); err != nil {
		t.Errorf("first message parse: %v", err)
	}
	if _, err := mimeparse.Parse(second.Message, parseLimits); err != nil {
		t.Errorf("second message parse: %v", err)
	}
}
