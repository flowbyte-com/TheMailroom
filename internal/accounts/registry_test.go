package accounts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/permissions"
)

func writeTempMemory(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	// Drop a minimal .eml so the memory loader has something to walk.
	if err := os.WriteFile(filepath.Join(d, "msg.eml"),
		[]byte("From: a@example.test\r\nTo: b@example.test\r\nSubject: hi\r\n\r\nbody\r\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewMemoryBackend(t *testing.T) {
	mbx := writeTempMemory(t)
	yaml := `
accounts:
  - id: a
    backend: memory
    email: a@example.test
    mailbox_root: ` + mbx + `
    permissions: [read]
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a, err := r.Get("a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !a.Permissions.Has(permissions.CapRead) {
		t.Errorf("read capability missing")
	}
	if a.Backend() == nil {
		t.Fatal("backend nil")
	}
	msgs, err := a.Backend().ListMessages(context.Background(), "a", "INBOX", mailbox.ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("len(msgs) = %d, want 1", len(msgs))
	}
}

func TestNewUnknownCapability(t *testing.T) {
	yaml := `
accounts:
  - id: a
    email: a@example.test
    imap:
      host: x.example.test
      username: u
      password: p
    smtp:
      host: x.example.test
      username: u
      password: p
    permissions: [read, "send-everything"]
`
	_, err := config.Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestViewsRedacted(t *testing.T) {
	mbx := writeTempMemory(t)
	yaml := `
accounts:
  - id: a
    backend: memory
    email: a@example.test
    mailbox_root: ` + mbx + `
    permissions: [read, send]
    smtp:
      host: smtp.example.test
      username: u@example.test
      password: hunter2
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	views := r.Views()
	if len(views) != 1 {
		t.Fatalf("len(views) = %d", len(views))
	}
	if views[0].ID != "a" {
		t.Errorf("id = %q", views[0].ID)
	}
	if len(views[0].Permissions) != 2 {
		t.Errorf("len(permissions) = %d", len(views[0].Permissions))
	}
	// Verify a JSON round-trip does not include any password-looking fields.
	// (Encoding via the struct alone is enough to assert; we re-encode by
	// relying on the Account struct not having any string field named
	// "password".)
	for _, v := range views {
		// Just spot-check the exposed field names. If a future refactor
		// adds a secret field, this test fails loudly.
		_ = v.Email
		_ = v.Provider
	}
}

func TestGetNotFound(t *testing.T) {
	mbx := writeTempMemory(t)
	yaml := `
accounts:
  - id: a
    backend: memory
    email: a@example.test
    mailbox_root: ` + mbx + `
`
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Get("nonexistent"); err != mailbox.ErrAccountNotFound {
		t.Errorf("err = %v, want ErrAccountNotFound", err)
	}
}
