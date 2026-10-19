package memory

import (
	"context"
	"os"
	"path/filepath"
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
	if len(msgs) != 1 {
		t.Fatalf("len = %d, want 1", len(msgs))
	}
	if msgs[0].Subject != "Welcome to TheMailroom" {
		t.Errorf("Subject = %q", msgs[0].Subject)
	}
	if msgs[0].From != "Sender <sender@example.test>" {
		t.Errorf("From = %q", msgs[0].From)
	}
	if !msgs[0].InternalDate.Equal(time.Date(2024, 1, 15, 10, 11, 12, 0, time.UTC)) {
		t.Errorf("InternalDate = %v", msgs[0].InternalDate)
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
	m, err := b.FetchMessage(context.Background(), "any", "Sent", 1)
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
	if _, err := b.FetchMessage(context.Background(), "any", "INBOX", 999); err != mailbox.ErrMessageNotFound {
		t.Errorf("err = %v, want ErrMessageNotFound", err)
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
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
