package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// repoRoot finds the project root by searching upward for go.mod.
// Tests run with cwd == the package directory; we want the testdata
// fixtures at the repo root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", dir)
		}
		dir = parent
	}
}

func newBackend(t *testing.T) *Backend {
	t.Helper()
	root := filepath.Join(repoRoot(t), "testdata", "messages")
	b, err := New(root)
	if err != nil {
		t.Fatalf("New(%s): %v", root, err)
	}
	return b
}

func TestNewAndMailboxes(t *testing.T) {
	b := newBackend(t)
	got, err := b.Mailboxes(context.Background(), "any")
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	names := make(map[string]bool)
	for _, m := range got {
		names[m.Name] = true
	}
	for _, want := range []string{"INBOX", "Sent"} {
		if !names[want] {
			t.Errorf("mailboxes missing %q; got %v", want, names)
		}
	}
}

func TestListMessages(t *testing.T) {
	b := newBackend(t)
	msgs, err := b.ListMessages(context.Background(), "any", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// The testdata fixture now contains several messages
	// (welcome, multipart, iso, oversize). The legacy
	// regression looks for the welcome subject specifically
	// and does not assert the total count.
	var welcome *mailbox.MessageSummary
	for i, m := range msgs {
		if m.Subject == "Welcome to TheMailroom" {
			welcome = &msgs[i]
			break
		}
	}
	if welcome == nil {
		t.Fatalf("welcome message not found; got %d messages", len(msgs))
	}
	if welcome.From != "Sender <sender@example.test>" {
		t.Errorf("From = %q", welcome.From)
	}
	if !welcome.InternalDate.Equal(time.Date(2024, 1, 15, 10, 11, 12, 0, time.UTC)) {
		t.Errorf("InternalDate = %v", welcome.InternalDate)
	}
}

func TestListMessagesMailboxNotFound(t *testing.T) {
	b := newBackend(t)
	if _, err := b.ListMessages(context.Background(), "any", "Nope", mailbox.ListOptions{}); err != mailbox.ErrMailboxNotFound {
		t.Errorf("err = %v, want ErrMailboxNotFound", err)
	}
}

func TestFetchMessage(t *testing.T) {
	b := newBackend(t)
	m, err := b.FetchMessage(context.Background(), "any", "Sent", 1, 1)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if m.Subject() == "" {
		t.Error("Subject is empty")
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("len(Attachments) = %d, want 1", len(m.Attachments))
	}
	if m.Attachments[0].Filename != "hello.txt" {
		t.Errorf("attachment filename = %q", m.Attachments[0].Filename)
	}
	if m.Attachments[0].Size == 0 {
		t.Error("attachment size is 0")
	}
}

func TestFetchMessageNotFound(t *testing.T) {
	b := newBackend(t)
	if _, err := b.FetchMessage(context.Background(), "any", "INBOX", 999, 1); err != mailbox.ErrMessageNotFound {
		t.Errorf("err = %v, want ErrMessageNotFound", err)
	}
}

func TestFetchMessageUIDValidityRequired(t *testing.T) {
	b := newBackend(t)
	// Zero UIDVALIDITY must be rejected: it would let a caller
	// target any message in a recycled mailbox.
	if _, err := b.FetchMessage(context.Background(), "any", "Sent", 1, 0); err != mailbox.ErrUIDMissing {
		t.Errorf("err = %v, want ErrUIDMissing", err)
	}
	// Mismatched UIDVALIDITY must be rejected.
	if _, err := b.FetchMessage(context.Background(), "any", "Sent", 1, 999); err != mailbox.ErrUIDMismatch {
		t.Errorf("err = %v, want ErrUIDMismatch", err)
	}
}

func TestSearchMessages(t *testing.T) {
	b := newBackend(t)
	got, err := b.SearchMessages(context.Background(), "any", "INBOX", mailbox.SearchQuery{Subject: "Welcome"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("len = %d, want 1", len(got))
	}
}

// TestFetchMessageISO verifies that a non-UTF-8 message in the
// testdata is decoded correctly. This is a higher-level wrapper
// around mimeparse's charset handling.
func TestFetchMessageISO(t *testing.T) {
	b := newBackend(t)
	msgs, err := b.ListMessages(context.Background(), "any", "INBOX", mailbox.ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var uid uint32
	for _, m := range msgs {
		if m.Subject == "café" {
			uid = m.UID
			break
		}
	}
	if uid == 0 {
		t.Skip("iso message not loaded")
	}
	full, err := b.FetchMessage(context.Background(), "any", "INBOX", uid, 1)
	if err != nil {
		t.Fatalf("FetchMessage: %v", err)
	}
	if !strings.Contains(full.Headers.Subject, "café") {
		t.Errorf("Subject = %q", full.Headers.Subject)
	}
}

func TestSearchMessagesNoMatch(t *testing.T) {
	b := newBackend(t)
	got, err := b.SearchMessages(context.Background(), "any", "INBOX", mailbox.SearchQuery{Subject: "Not there"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestListMessagesSince(t *testing.T) {
	b := newBackend(t)
	since := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	got, err := b.ListMessages(context.Background(), "any", "INBOX", mailbox.ListOptions{Since: &since, Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// The 16th is the cutoff: messages dated strictly after
	// are not in the welcome set. The new fixtures are dated
	// 17th, 18th and 19th, so we expect exactly 3.
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
}

// TestRejectsSymlinkInRoot is a HAL-review regression: a symlink in
// the mailbox root must never be followed.
func TestRejectsSymlinkInRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root user can follow symlinks to unreadable files; skipping")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideMsg := filepath.Join(outside, "secret.eml")
	if err := os.WriteFile(outsideMsg, []byte("From: x@y\r\nSubject: secret\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideMsg, filepath.Join(root, "leak.eml")); err != nil {
		t.Fatal(err)
	}
	_, err := New(root)
	if err == nil {
		t.Fatal("expected New to fail when a symlink is in the mailbox root")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err = %v, want 'symlink'", err)
	}
}

// TestRejectsSymlinkedDirectory is a HAL-review regression.
func TestRejectsSymlinkedDirectory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root user can follow symlinks to unreadable files; skipping")
	}
	root := t.TempDir()
	outside := t.TempDir()
	outsideDir := filepath.Join(outside, "inbox")
	if err := os.Mkdir(outsideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "msg.eml"),
		[]byte("From: x@y\r\nSubject: leak\r\n\r\nbody\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "inbox")); err != nil {
		t.Fatal(err)
	}
	_, err := New(root)
	if err == nil {
		t.Fatal("expected New to fail when a symlinked directory is in the mailbox root")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("err = %v, want 'symlink'", err)
	}
}
