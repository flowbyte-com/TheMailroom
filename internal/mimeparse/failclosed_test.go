package mimeparse

import (
	"errors"
	"strings"
	"testing"
)

func extractSiblingMessage(second string) []byte {
	return []byte("From: a@example.test\r\n" +
		"Subject: Fail closed\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"good.bin\"\r\n\r\n" +
		"GOOD\r\n" +
		"--B\r\n" + second +
		"--B--\r\n")
}

func TestExtractAttachmentFailsClosedOnMalformedSibling(t *testing.T) {
	raw := extractSiblingMessage("Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"bad.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"!!!!\r\n")
	got, err := ExtractAttachment(raw, "1", Limits{})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if got != nil {
		t.Fatalf("content returned despite malformed sibling: %+v", got)
	}
}

func TestExtractAttachmentFailsClosedOnOversizedTextSibling(t *testing.T) {
	raw := extractSiblingMessage("Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		strings.Repeat("X", 32) + "\r\n")
	got, err := ExtractAttachment(raw, "1", Limits{MaxAttachmentBytes: 8})
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
	if got != nil {
		t.Fatalf("content returned despite oversized text sibling: %+v", got)
	}
}

func TestExtractAttachmentFailsClosedOnTruncatedSibling(t *testing.T) {
	// The second sibling is cut off before the closing boundary. Its
	// body read fails after the valid first attachment was captured.
	raw := []byte("From: a@example.test\r\n" +
		"Subject: Truncated sibling\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"good.bin\"\r\n\r\n" +
		"GOOD\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"sibling without a closing boundary\r\n")
	got, err := ExtractAttachment(raw, "1", Limits{})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if got != nil {
		t.Fatalf("content returned despite truncated sibling: %+v", got)
	}
}

func TestExtractAttachmentFailsClosedOnRootUnknownEncoding(t *testing.T) {
	raw := []byte("From: a@example.test\r\n" +
		"Subject: Root encoding\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"bad.bin\"\r\n" +
		"Content-Transfer-Encoding: x-not-a-real-encoding\r\n\r\n" +
		"BAD\r\n")
	got, err := ExtractAttachment(raw, "1", Limits{})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if got != nil {
		t.Fatalf("content returned for unknown root encoding: %+v", got)
	}
}

func TestExtractAttachmentFailsClosedOnRootTextUnknownCharset(t *testing.T) {
	raw := []byte("From: a@example.test\r\n" +
		"Subject: Root charset\r\n" +
		"Content-Type: text/plain; charset=zz-not-real\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n\r\n" +
		"text that cannot be decoded\r\n")
	got, err := ExtractAttachment(raw, "1", Limits{})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if got != nil {
		t.Fatalf("content returned for unknown root charset: %+v", got)
	}
}

func TestExtractAttachmentFailsClosedOnSelectedTextUnknownCharset(t *testing.T) {
	raw := []byte("From: a@example.test\r\n" +
		"Subject: Selected charset\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"body\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=zz-not-real\r\n" +
		"Content-Disposition: attachment; filename=\"note.txt\"\r\n\r\n" +
		"text that cannot be decoded\r\n" +
		"--B--\r\n")
	got, err := ExtractAttachment(raw, "2", Limits{})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if got != nil {
		t.Fatalf("content returned for selected text with unknown charset: %+v", got)
	}
}

func TestExtractAttachmentUnknownCharsetBodyIsNotAnAttachment(t *testing.T) {
	raw := []byte("From: a@example.test\r\n" +
		"Subject: Body charset\r\n" +
		"Content-Type: text/plain; charset=zz-not-real\r\n\r\n" +
		"body with an unknown charset\r\n")
	got, err := ExtractAttachment(raw, "1", Limits{})
	if !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("err = %v, want ErrAttachmentNotFound", err)
	}
	if got != nil {
		t.Fatalf("content returned for an ordinary body part: %+v", got)
	}
}

func TestExtractAttachmentFailsClosedOnUnknownEncodingSibling(t *testing.T) {
	raw := extractSiblingMessage("Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"bad.bin\"\r\n" +
		"Content-Transfer-Encoding: x-not-a-real-encoding\r\n\r\n" +
		"BAD\r\n")
	got, err := ExtractAttachment(raw, "1", Limits{})
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if got != nil {
		t.Fatalf("content returned despite unknown encoding sibling: %+v", got)
	}
}

func TestParseKeepsBestEffortOnMalformedSibling(t *testing.T) {
	raw := extractSiblingMessage("Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"bad.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"!!!!\r\n")
	d, err := Parse(raw, Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(d.Attachments) != 1 || d.Attachments[0].Filename != "good.bin" {
		t.Errorf("attachments = %+v, want the valid sibling", d.Attachments)
	}
	if len(d.Warnings) == 0 {
		t.Error("expected Parse to retain the best-effort warning")
	}
}

func TestExtractAttachmentAllowsNonFatalCharsetWarning(t *testing.T) {
	raw := []byte("From: a@example.test\r\n" +
		"Subject: Charset warning\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=zz-not-real\r\n\r\n" +
		"body with an unknown charset\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"good.bin\"\r\n\r\n" +
		"GOOD\r\n" +
		"--B--\r\n")
	got, err := ExtractAttachment(raw, "2", Limits{})
	if err != nil {
		t.Fatalf("ExtractAttachment: %v", err)
	}
	if string(got.Content) != "GOOD" {
		t.Errorf("content = %q, want GOOD", got.Content)
	}
	if len(got.Warnings) == 0 {
		t.Error("expected a non-fatal charset warning")
	}
}
