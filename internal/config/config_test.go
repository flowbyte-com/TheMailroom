package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// jsonMarshal is a thin wrapper used by the redaction regression test.
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

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
	if redacted := cfg.Redacted(); !strings.Contains(redacted, "max_request_bytes=33554432") {
		t.Errorf("Redacted() missing server max_request_bytes; got: %s", redacted)
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

func TestServerMaxRequestBytes(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    int
		wantErr string
	}{
		{name: "default", want: DefaultMaxRequestBytes},
		{name: "zero selects default", value: "0", want: DefaultMaxRequestBytes},
		{name: "explicit", value: "1024", want: 1024},
		{name: "upper bound", value: strconv.Itoa(MaxRequestBytesLimit), want: MaxRequestBytesLimit},
		{name: "above upper bound", value: strconv.Itoa(MaxRequestBytesLimit + 1), wantErr: "max_request_bytes"},
		{name: "negative", value: "-1", wantErr: "max_request_bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := validYAML
			if tc.value != "" {
				yaml = strings.Replace(yaml, "  log_level: info", "  log_level: info\n  max_request_bytes: "+tc.value, 1)
			}
			cfg, err := Parse([]byte(yaml))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %q, want substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if cfg.Server.MaxRequestBytes != tc.want {
				t.Errorf("MaxRequestBytes = %d, want %d", cfg.Server.MaxRequestBytes, tc.want)
			}
		})
	}
}

func TestMaxRequestBytesIndependentOfAccountMessageLimit(t *testing.T) {
	yaml := strings.Replace(validYAML, "  log_level: info", "  log_level: info\n  max_request_bytes: 1024", 1)
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Server.MaxRequestBytes != 1024 {
		t.Errorf("MaxRequestBytes = %d, want 1024", cfg.Server.MaxRequestBytes)
	}
	if cfg.Accounts[0].Limits.MaxMessageBytes != 10485760 {
		t.Errorf("MaxMessageBytes = %d, want 10485760", cfg.Accounts[0].Limits.MaxMessageBytes)
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

// --- HAL review regressions ---

func TestParseUnknownField(t *testing.T) {
	// yaml.v3 with KnownFields(true) must reject typos.
	yaml := `
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, username: u, password: p}
    smtp: {host: x.example.test, username: u, password: p}
    permisions: [read]   # typo
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "permisions") {
		t.Errorf("err = %q, want to mention 'permisions'", err.Error())
	}
}

func TestParseDuplicateKey(t *testing.T) {
	yaml := `
server:
  log_level: info
  log_level: debug
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, username: u, password: p}
    smtp: {host: x.example.test, username: u, password: p}
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for duplicate key")
	}
	if !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("err = %q, want 'duplicate key'", err.Error())
	}
}

func TestParseMissingAuth(t *testing.T) {
	yaml := `
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, username: u}
    smtp: {host: x.example.test, username: u, password: p}
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for missing auth")
	}
	if !strings.Contains(err.Error(), "password or password_command") {
		t.Errorf("err = %q, want 'password or password_command'", err.Error())
	}
}

func TestParseBothAuthMethods(t *testing.T) {
	yaml := `
accounts:
  - id: a
    email: u@example.test
    imap:
      host: x.example.test
      username: u
      password: hunter2
      password_command: ["pass", "show"]
    smtp: {host: x.example.test, username: u, password: p}
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error for both auth methods")
	}
	if !strings.Contains(err.Error(), "only one") {
		t.Errorf("err = %q, want 'only one'", err.Error())
	}
}

func TestInsecureSkipVerifyRemoved(t *testing.T) {
	// The field used to exist on IMAPConfig; this test asserts it is
	// gone. If a future refactor re-introduces it, the field won't
	// unmarshal anything, so the struct stays zero-valued.
	yaml := `
accounts:
  - id: a
    email: u@example.test
    imap:
      host: x.example.test
      username: u
      password: p
      insecure_skip_verify: true
    smtp: {host: x.example.test, username: u, password: p}
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected error: insecure_skip_verify is no longer a recognised field")
	}
	if !strings.Contains(err.Error(), "insecure_skip_verify") {
		t.Errorf("err = %q, expected to mention insecure_skip_verify", err.Error())
	}
}

func TestRedactedHasNoSecret(t *testing.T) {
	yaml := `
accounts:
  - id: a
    email: u@example.test
    imap:
      host: x.example.test
      username: u
      password: verysecretdontlogthis
    smtp:
      host: x.example.test
      username: u
      password: alsoverysecret
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := cfg.Redacted()
	for _, sub := range []string{"verysecretdontlogthis", "alsoverysecret"} {
		if strings.Contains(s, sub) {
			t.Errorf("Redacted() leaked %q", sub)
		}
	}
	if !strings.Contains(s, "***") {
		t.Error("Redacted() did not include any '***' marker")
	}
}

func TestConfigJSONOmitsPassword(t *testing.T) {
	// Even if a future caller serialises the config to JSON, the
	// password fields must be elided.
	yaml := `
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, username: u, password: topsecret}
    smtp: {host: x.example.test, username: u, password: topsecret2}
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := jsonMarshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "topsecret") || strings.Contains(string(b), "topsecret2") {
		t.Errorf("JSON leaked password: %s", b)
	}
}

// TestRedactedIncludesNewLimits is a regression for the Phase 2.1
// safety limit fields. The redacted form is the only canonical
// printable view; if any new limit field is added without being
// included here, a future maintainer may add it to the schema
// without re-reviewing the redaction path.
func TestRedactedIncludesNewLimits(t *testing.T) {
	yaml := `
accounts:
  - id: a
    email: u@example.test
    imap: {host: x.example.test, username: u, password: topsecret}
    smtp: {host: x.example.test, username: u, password: topsecret2}
    limits:
      max_attachment_bytes: 1024
      max_total_attachment_bytes: 2048
      max_mime_nesting: 8
      max_mime_parts: 200
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := cfg.Redacted()
	for _, want := range []string{
		"max_attachment_bytes=1024",
		"max_total_attachment_bytes=2048",
		"max_mime_nesting=8",
		"max_mime_parts=200",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Redacted() missing %q; got: %s", want, s)
		}
	}
	// The credential value must still be elided.
	if strings.Contains(s, "topsecret") || strings.Contains(s, "topsecret2") {
		t.Errorf("Redacted() leaked credential: %s", s)
	}
}

// --- Phase 2.3 PR 1: outbound foundations ---

func TestReadOnlyAccountDoesNotRequireSMTP(t *testing.T) {
	mbx := t.TempDir()
	yaml := `
accounts:
  - id: read-only
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [read]
`
	if _, err := Parse([]byte(yaml)); err != nil {
		t.Fatalf("read-only account without SMTP rejected: %v", err)
	}
}

func TestReadOnlyAccountIgnoresIncompleteSMTP(t *testing.T) {
	mbx := t.TempDir()
	yaml := `
accounts:
  - id: read-only
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [read]
    smtp:
      host: smtp.example.test
      port: 25
`
	if _, err := Parse([]byte(yaml)); err != nil {
		t.Fatalf("read-only account with incomplete SMTP rejected: %v", err)
	}
}

func TestSendAccountRequiresSMTP(t *testing.T) {
	mbx := t.TempDir()
	yaml := `
accounts:
  - id: sender
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [send]
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected SMTP validation error")
	}
	if !strings.Contains(err.Error(), "smtp.host") {
		t.Errorf("err = %q, want smtp.host", err.Error())
	}
}

func TestSendAccountValidSMTP(t *testing.T) {
	mbx := t.TempDir()
	yaml := `
accounts:
  - id: sender
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [send]
    smtp:
      host: smtp.example.test
      username: user@example.test
      password: secret
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	smtp := cfg.Accounts[0].SMTP
	if !smtp.StartTLS || smtp.TLS {
		t.Errorf("TLS mode = tls:%v starttls:%v, want mandatory STARTTLS", smtp.TLS, smtp.StartTLS)
	}
	if smtp.Port != DefaultSMTPPort {
		t.Errorf("port = %d, want %d", smtp.Port, DefaultSMTPPort)
	}
}

func TestSendAccountRejectsUnsafeTLSModes(t *testing.T) {
	mbx := t.TempDir()
	cases := []struct {
		name    string
		smtp    string
		wantSub string
	}{
		{
			name: "both modes",
			smtp: `
      host: smtp.example.test
      tls: true
      starttls: true
      username: user@example.test
      password: secret`,
			wantSub: "only one of tls or starttls",
		},
		{
			name: "neither mode",
			smtp: `
      host: smtp.example.test
      port: 587
      username: user@example.test
      password: secret`,
			wantSub: "one of tls or starttls",
		},
		{
			name: "missing auth",
			smtp: `
      host: smtp.example.test
      username: user@example.test`,
			wantSub: "password or password_command",
		},
		{
			name: "bad port",
			smtp: `
      host: smtp.example.test
      port: 70000
      starttls: true
      username: user@example.test
      password: secret`,
			wantSub: "port",
		},
		{
			name: "host with port",
			smtp: `
      host: smtp.example.test:587
      starttls: true
      username: user@example.test
      password: secret`,
			wantSub: "host and port must be separate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
accounts:
  - id: sender
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [send]
    smtp:` + tc.smtp + `
`
			_, err := Parse([]byte(yaml))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestSMTPValidationDoesNotLeakCredentials(t *testing.T) {
	mbx := t.TempDir()
	const secret = "very-secret-password-value"
	yaml := `
accounts:
  - id: sender
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [send]
    smtp:
      host: smtp.example.test
      tls: true
      starttls: true
      username: user@example.test
      password: ` + secret + `
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected TLS mode error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked SMTP password: %q", err.Error())
	}
	cfg, err := Parse([]byte(strings.Replace(yaml, "      starttls: true\n", "", 1)))
	if err != nil {
		t.Fatalf("Parse valid config: %v", err)
	}
	if redacted := cfg.Redacted(); strings.Contains(redacted, secret) {
		t.Errorf("redacted config leaked SMTP password: %q", redacted)
	}
}

func TestRecipientPolicyConfig(t *testing.T) {
	mbx := t.TempDir()
	yaml := `
accounts:
  - id: sender
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [send]
    allowed_recipients:
      - Alice@Example.Test
      - bob@example.test
    smtp:
      host: smtp.example.test
      username: user@example.test
      password: secret
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Accounts[0].Limits.MaxRecipients; got != DefaultMaxRecipients {
		t.Errorf("MaxRecipients = %d, want default %d", got, DefaultMaxRecipients)
	}
	if got := len(cfg.Accounts[0].AllowedRecipients); got != 2 {
		t.Errorf("AllowedRecipients length = %d, want 2", got)
	}
}

func TestBadRecipientPolicyConfig(t *testing.T) {
	mbx := t.TempDir()
	cases := []struct {
		name    string
		policy  string
		wantSub string
	}{
		{name: "malformed allow-list", policy: "    allowed_recipients: [not-an-address]\n", wantSub: "allowed_recipients"},
		{name: "negative limit", policy: "    limits: {max_recipients: -1}\n", wantSub: "max_recipients"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := `
accounts:
  - id: sender
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [send]
` + tc.policy + `    smtp:
      host: smtp.example.test
      username: user@example.test
      password: secret
`
			_, err := Parse([]byte(yaml))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestAttachmentRootValidation(t *testing.T) {
	mbx := t.TempDir()
	root := t.TempDir()
	validYAML := func(paths string) string {
		return `
accounts:
  - id: sender
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [attachment_send]
    attachment_send_paths:
` + paths + `
`
	}
	if _, err := Parse([]byte(validYAML("      - " + root))); err != nil {
		t.Fatalf("valid attachment root rejected: %v", err)
	}

	cases := []struct {
		name    string
		paths   string
		wantSub string
	}{
		{name: "missing", paths: "      - " + filepath.Join(root, "missing"), wantSub: "attachment_send_paths[0]"},
		{name: "file", paths: "      - " + filepath.Join(root, "file"), wantSub: "not a directory"},
		{name: "duplicate", paths: "      - " + root + "\n      - " + root, wantSub: "duplicates"},
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(validYAML(tc.paths)))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestReadOnlyAccountMayHaveUnusedMissingAttachmentRoot(t *testing.T) {
	mbx := t.TempDir()
	yaml := `
accounts:
  - id: read-only
    backend: memory
    email: user@example.test
    mailbox_root: ` + mbx + `
    permissions: [read]
    attachment_send_paths:
      - /does/not/exist/but/is/unused
`
	if _, err := Parse([]byte(yaml)); err != nil {
		t.Fatalf("read-only account with unused attachment root rejected: %v", err)
	}
}

func TestSenderIdentityRejectsControlCharacters(t *testing.T) {
	mbx := t.TempDir()
	yaml := `
accounts:
  - id: a
    backend: memory
    email: "user@example.test\nBcc: evil@example.test"
    mailbox_root: ` + mbx + `
    permissions: [read]
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Fatal("expected sender identity error")
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("err = %q, want email field", err.Error())
	}
	if strings.Contains(err.Error(), "evil") {
		t.Errorf("error reflected header-injection input: %q", err.Error())
	}
}
