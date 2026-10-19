package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
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

// TestPreExistingSearchFlagGap documents the known
// limitation: q.Flagged and q.Unseen are accepted by the
// SearchQuery type but the in-memory searchMatches function
// does not honour them. Unseen always returns no matches
// (memory messages are all \\Seen), and Flagged is silently
// ignored. A future change should set q.Flagged to filter on
// the \\Flagged flag in the MessageSummary. This test will
// start failing when the gap is fixed; the fix author should
// update the test to assert the new behaviour.
func TestPreExistingSearchFlagGap(t *testing.T) {
	b := newBackend(t)
	// q.Unseen: with the current semantics, every Unseen
	// query returns no matches.
	res, err := b.SearchMessages(context.Background(), "any", "INBOX", mailbox.SearchQuery{
		Subject: "Welcome",
		Unseen:  true,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("Unseen filter should match no memory messages; got %d", len(res.Messages))
	}
	// q.Flagged is currently ignored; the unfiltered count
	// for the same subject should be 1.
	res2, err := b.SearchMessages(context.Background(), "any", "INBOX", mailbox.SearchQuery{
		Subject: "Welcome",
		Flagged: true,
	})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(res2.Messages) != 1 {
		t.Errorf("Flagged filter is currently a no-op; expected 1 result, got %d", len(res2.Messages))
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
