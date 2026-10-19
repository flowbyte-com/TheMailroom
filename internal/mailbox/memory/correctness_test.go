package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
)

// TestParseMultipartMixed exercises the canonical case: a message
// with one text part and one attachment.
func TestParseMultipartMixed(t *testing.T) {
	b := newBackend(t)
	m, err := b.FetchMessage(context.Background(), "any", "Sent", 1, 1)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if !strings.Contains(m.Text, "Plain body") {
		t.Errorf("Text = %q", m.Text)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("len(Attachments) = %d, want 1", len(m.Attachments))
	}
	a := m.Attachments[0]
	if a.Filename != "hello.txt" {
		t.Errorf("Filename = %q", a.Filename)
	}
	if a.MimeType != "application/octet-stream" {
		t.Errorf("MimeType = %q", a.MimeType)
	}
	if a.Inline {
		t.Error("Inline = true, want false for an attachment")
	}
	if a.Size == 0 {
		t.Error("Size is 0")
	}
}

// TestParseNestedMultipart exercises a message that is
// multipart/related inside multipart/alternative inside
// multipart/mixed, with both inline and attached parts.
func TestParseNestedMultipart(t *testing.T) {
	root := filepath.Join(repoRoot(t), "testdata", "messages")
	if _, err := os.Stat(filepath.Join(root, "multipart.eml")); err != nil {
		t.Skipf("multipart.eml missing: %v", err)
	}
	b, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listRes, err := b.ListMessages(context.Background(), "any", "INBOX", mailbox.ListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// Find the multipart message by subject.
	var m *mailbox.Message
	for _, msg := range listRes.Messages {
		if strings.Contains(msg.Subject, "Multi-part") {
			uid := msg.UID
			full, err := b.FetchMessage(context.Background(), "any", "INBOX", uid, listRes.UIDValidity)
			if err != nil {
				t.Fatalf("FetchMessage(%d): %v", uid, err)
			}
			m = full
			break
		}
	}
	if m == nil {
		t.Fatal("multipart message not found in INBOX")
	}
	if !strings.Contains(m.HTML, "HTML version") {
		t.Errorf("HTML = %q, want HTML body", m.HTML)
	}
	// We expect two attachments: the inline logo and the
	// attached PDF and the CSV; the CSV may be the second or
	// third depending on how the walker visits. The
	// significant property is that at least one inline and at
	// least one non-inline attachment appear.
	var inline, attached int
	for _, a := range m.Attachments {
		if a.Inline {
			inline++
			if a.ContentID == "" {
				t.Errorf("inline attachment missing Content-ID: %+v", a)
			}
		} else {
			attached++
		}
	}
	if inline < 1 {
		t.Errorf("inline attachments = %d, want >= 1", inline)
	}
	if attached < 2 {
		t.Errorf("attached = %d, want >= 2", attached)
	}
}

// TestParseISO88591 verifies that a non-UTF-8 message is decoded
// to UTF-8 by the mimeparse package.
func TestParseISO88591(t *testing.T) {
	root := filepath.Join(repoRoot(t), "testdata", "messages")
	b, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	listRes, err := b.ListMessages(context.Background(), "any", "INBOX", mailbox.ListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var m *mailbox.Message
	for _, msg := range listRes.Messages {
		if msg.Subject == "café" {
			uid := msg.UID
			full, err := b.FetchMessage(context.Background(), "any", "INBOX", uid, listRes.UIDValidity)
			if err != nil {
				t.Fatalf("FetchMessage(%d): %v", uid, err)
			}
			m = full
			break
		}
	}
	if m == nil {
		t.Fatal("iso message not found")
	}
	if !strings.Contains(m.Text, "café") {
		t.Errorf("Text = %q, want decoded utf-8", m.Text)
	}
	if !strings.Contains(m.Headers.Subject, "café") {
		t.Errorf("Subject = %q, want decoded utf-8", m.Headers.Subject)
	}
}

// TestAttachmentSizeCap exercises the per-attachment cap. We
// inject an oversized attachment into a temp dir and assert the
// backend rejects the file at load time.
func TestAttachmentSizeCap(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("A", 2048)
	body := "From: a@example.test\r\n" +
		"Subject: Big\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n" +
		"--B\r\nContent-Type: application/octet-stream\r\n" +
		"Content-Disposition: attachment; filename=\"big.bin\"\r\n\r\n" +
		big + "\r\n" +
		"--B--\r\n"
	if err := os.WriteFile(filepath.Join(dir, "big.eml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewWithLimits(dir, config.LimitsConfig{
		MaxMessageBytes:         1 << 20,
		MaxAttachmentBytes:      256,
		MaxTotalAttachmentBytes: 1 << 20,
	})
	if err == nil {
		t.Error("expected error for oversized attachment during load")
	}
	if !strings.Contains(err.Error(), "attachment") {
		t.Errorf("err = %v, want 'attachment' in error", err)
	}
}

// TestSearchFlagFilters verifies that the Flagged and Unseen
// filters are honoured by the memory backend.
//
// The flags come from the RFC 2156 Status: header of the .eml
// file: "R" means \Seen, "F" means \Flagged, and so on. A
// message with no Status: header carries no flags, which means
// it is unseen and not flagged.
//
// This replaces the earlier TestPreExistingSearchFlagGap,
// which documented that the filters were silently ignored.
func TestSearchFlagFilters(t *testing.T) {
	b := newBackend(t)

	// The welcome.eml fixture has no Status: header, so it
	// carries no flags: it is unseen and not flagged.
	unseen, err := b.SearchMessages(context.Background(), "any", "INBOX", mailbox.SearchQuery{
		Subject: "Welcome",
		Unseen:  true,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(unseen.Messages) != 1 {
		t.Errorf("Unseen filter matched %d messages, want 1 (the fixture has no \\Seen flag)", len(unseen.Messages))
	}

	// The same message must not match the Flagged filter.
	flagged, err := b.SearchMessages(context.Background(), "any", "INBOX", mailbox.SearchQuery{
		Subject: "Welcome",
		Flagged: true,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(flagged.Messages) != 0 {
		t.Errorf("Flagged filter matched %d messages, want 0 (the fixture has no \\Flagged flag)", len(flagged.Messages))
	}

	// A message that is both seen and flagged must match
	// neither the Unseen nor the Flagged filter.
	b2 := backendWithStatus(t, "RF")
	seen, err := b2.SearchMessages(context.Background(), "any", "INBOX", mailbox.SearchQuery{
		Unseen: true,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(seen.Messages) != 0 {
		t.Errorf("Unseen filter matched %d messages, want 0 (the fixture is \\Seen)", len(seen.Messages))
	}
	fl, err := b2.SearchMessages(context.Background(), "any", "INBOX", mailbox.SearchQuery{
		Flagged: true,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(fl.Messages) != 1 {
		t.Errorf("Flagged filter matched %d messages, want 1", len(fl.Messages))
	}
}

// backendWithStatus builds a memory backend whose only message
// carries the given RFC 2156 Status: header value.
func backendWithStatus(t *testing.T, status string) *Backend {
	t.Helper()
	dir := t.TempDir()
	raw := "From: a@example.test\r\n" +
		"To: b@example.test\r\n" +
		"Subject: Status fixture\r\n" +
		"Date: Mon, 15 Jan 2024 10:11:12 +0000\r\n" +
		"Status: " + status + "\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"body\r\n"
	if err := os.WriteFile(filepath.Join(dir, "m.eml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMaxAttachmentsLimit is the end-to-end regression for
// the MaxAttachments cap on the memory backend. The backend
// must propagate the limit into mimeparse.Parse; a file with
// more than MaxAttachments attachments must fail to load
// with ErrTooManyAttachments.
func TestMaxAttachmentsLimit(t *testing.T) {
	dir := t.TempDir()
	// Build a multipart/mixed with 5 attachments.
	var b strings.Builder
	b.WriteString("From: a@example.test\r\nSubject: A\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n")
	for i := 0; i < 5; i++ {
		b.WriteString("--B\r\nContent-Type: application/octet-stream\r\n" +
			"Content-Disposition: attachment; filename=\"a.bin\"\r\n\r\nx\r\n")
	}
	b.WriteString("--B--\r\n")
	if err := os.WriteFile(filepath.Join(dir, "many.eml"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	// Cap at 3; the file has 5. The load must fail.
	_, err := NewWithLimits(dir, config.LimitsConfig{
		MaxAttachments:  3,
		MaxMessageBytes: 1 << 20,
	})
	if !errors.Is(err, mimeparse.ErrTooManyAttachments) {
		t.Errorf("err = %v, want mimeparse.ErrTooManyAttachments", err)
	}
}

// TestNestingTooDeep exercises the MIME nesting cap. The
// memory backend applies the cap at load time, so a file that
// violates the cap fails NewWithLimits rather than FetchMessage.
func TestNestingTooDeep(t *testing.T) {
	dir := t.TempDir()
	// Build a deeply nested multipart. The default cap is 16.
	inner := "From: a@example.test\r\nSubject: Deep\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n"
	for i := 0; i < 20; i++ {
		boundary := "B" + strings.Repeat("x", i+1)
		inner = "From: a@example.test\r\nSubject: Deep\r\n" +
			"Content-Type: multipart/mixed; boundary=" + boundary + "\r\n\r\n" +
			"--" + boundary + "\r\n" + inner + "\r\n--" + boundary + "--\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "deep.eml"), []byte(inner), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewWithLimits(dir, config.LimitsConfig{
		MaxMIMENesting:  4,
		MaxMessageBytes: 1 << 20,
	})
	if err == nil {
		t.Error("expected error for over-nested MIME during load")
	}
	if !strings.Contains(err.Error(), "nesting") {
		t.Errorf("err = %v, want 'nesting' in error message", err)
	}
}
