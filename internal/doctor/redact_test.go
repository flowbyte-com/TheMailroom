package doctor

import (
	"bytes"
	"strings"
	"testing"
)

// callout waits for the credential the constructor is meant to cover but the
// pattern matcher demonstrably does not.
func renderLocal(t *testing.T, body string) string {
	t.Helper()
	path := writeConfig(t, body)
	result := RunLocal(path)
	var buf bytes.Buffer
	if err := (Report{Checks: result.Checks}).WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return buf.String()
}

// TestCredentialRedactionOneAndTwoCharacters covers short credentials. A
// short secret is still a secret.
func TestCredentialRedactionOneAndTwoCharacters(t *testing.T) {
	for _, secret := range []string{"z", "ab"} {
		t.Run(secret, func(t *testing.T) {
			body := `server:
  log_level: info

accounts:
  - id: work
    backend: imap
    email: work@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: work
      password: "` + secret + `"
    limits:
      max_message_bytes: "` + secret + `"
`
			out := renderLocal(t, body)
			if strings.Contains(out, `"`+secret+`"`) || strings.Contains(out, "`"+secret+"`") {
				t.Errorf("the report exposes the %d-character credential %q:\n%s", len(secret), secret, out)
			}
		})
	}
}

// TestCredentialRedactionThroughAnAnchor covers a credential defined by an
// anchor and referred to by an alias.
func TestCredentialRedactionThroughAnAnchor(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: work
    backend: imap
    email: work@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: work
      password: &pw "anchor-canary-value"
    limits:
      max_message_bytes: *pw
`
	out := renderLocal(t, body)
	if strings.Contains(out, "anchor-canary-value") {
		t.Errorf("the report exposes an anchored credential:\n%s", out)
	}
}

// TestCredentialRedactionWithEscapes covers an escaped credential.
func TestCredentialRedactionWithEscapes(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: work
    backend: imap
    email: work@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: work
      password: "escape\"canary"
    limits:
      max_message_bytes: "escape\"canary"
`
	out := renderLocal(t, body)
	for _, canary := range []string{`escape"canary`, `escape\"canary`} {
		if strings.Contains(out, canary) {
			t.Errorf("the report exposes an escaped credential %q:\n%s", canary, out)
		}
	}
}

// TestCredentialRedactionFlowList covers a flow-style password_command.
func TestCredentialRedactionFlowList(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: work
    backend: imap
    email: work@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: work
      password_command: ["cat", "/run/secrets/flow-canary"]
    limits:
      max_message_bytes: "flow-canary"
`
	out := renderLocal(t, body)
	if strings.Contains(out, "flow-canary") {
		t.Errorf("the report exposes a flow-list credential command argument:\n%s", out)
	}
}

// TestCredentialRedactionBlockList covers a block-style password_command.
func TestCredentialRedactionBlockList(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: work
    backend: imap
    email: work@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: work
      password_command:
        - cat
        - /run/secrets/block-canary
    limits:
      max_message_bytes: "block-canary"
`
	out := renderLocal(t, body)
	if strings.Contains(out, "block-canary") {
		t.Errorf("the report exposes a block-list credential command argument:\n%s", out)
	}
}

// TestCredentialRedactionInValidationErrors covers a credential echoed by a
// validation failure rather than a YAML type failure.
func TestCredentialRedactionInValidationErrors(t *testing.T) {
	// The invalid log level is echoed by the loader with %q. Placing the
	// credential there must not echo it back.
	body := `server:
  log_level: "validation-canary-value"

accounts:
  - id: work
    backend: imap
    email: work@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: work
      password: "validation-canary-value"
`
	out := renderLocal(t, body)
	if strings.Contains(out, "validation-canary-value") {
		t.Errorf("the report exposes a credential echoed by a validation error:\n%s", out)
	}
}

// TestNoRawYAMLErrorIsPrinted pins the fail-closed rule: a YAML decode
// error quotes arbitrary document content, so it must never be printed.
func TestNoRawYAMLErrorIsPrinted(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: work
    backend: imap
    email: work@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: work
      password: "decode-canary-value"
    limits:
      max_message_bytes: [decode-canary-value]
`
	out := renderLocal(t, body)
	if strings.Contains(out, "decode-canary-value") {
		t.Errorf("the report prints raw YAML error content:\n%s", out)
	}
}
