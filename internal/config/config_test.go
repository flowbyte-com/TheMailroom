package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
server:
  log_level: info

accounts:
  - id: personal
    backend: memory
    email: user@example.test
    mailbox_root: ./testdata/messages
    permissions:
      - read
    limits:
      max_message_bytes: 10485760
      max_attachments: 10
`

// validYAMLResolved substitutes the mailbox root with an absolute path so
// tests are stable regardless of the working directory used by `go test`.
const validYAMLResolved = `
server:
  log_level: info

accounts:
  - id: personal
    backend: memory
    email: user@example.test
    mailbox_root: __MBX__
    permissions:
      - read
    limits:
      max_message_bytes: 10485760
      max_attachments: 10
`

func TestParseValid(t *testing.T) {
	mbx := t.TempDir()
	yaml := strings.Replace(validYAMLResolved, "__MBX__", mbx, 1)
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want info", cfg.Server.LogLevel)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("len(Accounts) = %d, want 1", len(cfg.Accounts))
	}
	a := cfg.Accounts[0]
	if a.ID != "personal" {
		t.Errorf("ID = %q, want personal", a.ID)
	}
	if a.Backend != "memory" {
		t.Errorf("Backend = %q, want memory", a.Backend)
	}
	if a.Limits.MaxMessageBytes != 10485760 {
		t.Errorf("MaxMessageBytes = %d, want 10485760", a.Limits.MaxMessageBytes)
	}
	if a.Limits.MaxAttachments != 10 {
		t.Errorf("MaxAttachments = %d, want 10", a.Limits.MaxAttachments)
	}
	if a.Limits.MaxSearchResults != DefaultMaxSearchResults {
		t.Errorf("MaxSearchResults = %d, want default %d", a.Limits.MaxSearchResults, DefaultMaxSearchResults)
	}
}

func TestParseDefaults(t *testing.T) {
	yaml := `
accounts:
  - id: a
    email: u@example.test
    imap:
      host: imap.example.test
      username: u@example.test
      password: x
    smtp:
      host: smtp.example.test
      username: u@example.test
      password: x
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := cfg.Accounts[0]
	if a.IMAP.Port != DefaultIMAPPort {
		t.Errorf("imap default port = %d, want %d", a.IMAP.Port, DefaultIMAPPort)
	}
	if a.SMTP.Port != DefaultSMTPPort {
		t.Errorf("smtp default port = %d, want %d", a.SMTP.Port, DefaultSMTPPort)
	}
	if a.Limits.MaxMessageBytes != DefaultMaxMessageBytes {
		t.Errorf("limits default missing")
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "missing accounts",
			yaml: `server: {log_level: info}
accounts: []`,
			wantSub: "at least one account",
		},
		{
			name: "missing id",
			yaml: `
accounts:
  - email: u@example.test
    imap: {host: x.example.test, port: 993, username: u, password: p}
    smtp: {host: x.example.test, port: 587, username: u, password: p}
`,
			wantSub: "id is required",
		},
		{
			name: "bad email",
			yaml: `
accounts:
  - id: a
    email: not-an-email
    imap: {host: x.example.test, port: 993, username: u, password: p}
    smtp: {host: x.example.test, port: 587, username: u, password: p}
`,
			wantSub: "email",
		},
		{
			name: "id with whitespace",
			yaml: `
accounts:
  - id: " bad"
    email: u@example.test
    imap: {host: x.example.test, port: 993, username: u, password: p}
    smtp: {host: x.example.test, port: 587, username: u, password: p}
`,
			wantSub: "whitespace",
		},
		{
			name: "duplicate id",
			yaml: `
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, port: 993, username: u, password: p}
    smtp: {host: x.example.test, port: 587, username: u, password: p}
  - id: a
    email: u@example.test
    imap: {host: x.example.test, port: 993, username: u, password: p}
    smtp: {host: x.example.test, port: 587, username: u, password: p}
`,
			wantSub: "duplicate",
		},
		{
			name: "bad port",
			yaml: `
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, port: 70000, username: u, password: p}
    smtp: {host: x.example.test, port: 587, username: u, password: p}
`,
			wantSub: "port",
		},
		{
			name: "memory backend without root",
			yaml: `
accounts:
  - id: a
    backend: memory
    email: u@example.test
`,
			wantSub: "mailbox_root is required",
		},
		{
			name: "memory backend missing dir",
			yaml: `
accounts:
  - id: a
    backend: memory
    email: u@example.test
    mailbox_root: /this/path/does/not/exist/xyzzy
`,
			wantSub: "no such file or directory",
		},
		{
			name: "bad log level",
			yaml: `server: {log_level: chatty}
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, port: 993, username: u, password: p}
    smtp: {host: x.example.test, port: 587, username: u, password: p}
`,
			wantSub: "log_level",
		},
		{
			name: "relative attachment path",
			yaml: `
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, port: 993, username: u, password: p}
    smtp: {host: x.example.test, port: 587, username: u, password: p}
    attachment_send_paths:
      - relative/path
`,
			wantSub: "absolute",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestLoadFromFile(t *testing.T) {
	mbx := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := strings.Replace(validYAMLResolved, "__MBX__", mbx, 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Accounts[0].ID != "personal" {
		t.Errorf("ID = %q", cfg.Accounts[0].ID)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load("/nonexistent/xyzzy.yaml")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "/nonexistent/xyzzy.yaml") {
		t.Errorf("err = %q, expected to mention path", err.Error())
	}
}
