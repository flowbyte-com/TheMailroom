package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// writeConfig writes a configuration file with owner-only permissions and
// returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "themailroom.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// memoryConfigBody is a minimal valid configuration using the in-memory
// backend, so the local checks need no network and no credentials.
func memoryConfigBody(mailboxRoot string) string {
	return `server:
  log_level: info

accounts:
  - id: main
    backend: memory
    email: main@example.test
    mailbox_root: ` + mailboxRoot + `
    permissions: [read]
`
}

// imapConfigBody is a configuration with one IMAP account and a credential
// command.
func imapConfigBody(extra string) string {
	return `server:
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
        - vault
        - read
        - mail/production-secret
` + extra
}

func findCheck(checks []Check, name string) (Check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func failures(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if c.Status == StatusFail {
			out = append(out, c)
		}
	}
	return out
}

// TestRunLocalAcceptsAValidConfiguration covers the fully valid path.
func TestRunLocalAcceptsAValidConfiguration(t *testing.T) {
	path := writeConfig(t, memoryConfigBody(t.TempDir()))
	result := RunLocal(path)
	if !result.Loaded {
		t.Fatalf("Loaded = false; checks: %+v", result.Checks)
	}
	if got := failures(result.Checks); len(got) != 0 {
		t.Fatalf("unexpected failures: %+v", got)
	}
	for _, name := range []string{"config.file", "config.parse", "config.server", "registry", "account/main"} {
		if _, ok := findCheck(result.Checks, name); !ok {
			t.Errorf("missing check %q", name)
		}
	}
}

// TestRunLocalReportsAMissingFile covers discovery failure.
func TestRunLocalReportsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.yaml")
	result := RunLocal(path)
	if result.Loaded {
		t.Fatal("Loaded = true for a missing file")
	}
	file, ok := findCheck(result.Checks, "config.file")
	if !ok || file.Status != StatusFail {
		t.Fatalf("config.file = %+v, want a failure", file)
	}
	parse, _ := findCheck(result.Checks, "config.parse")
	if parse.Status != StatusSkip {
		t.Errorf("config.parse = %s, want skip", parse.Status)
	}
}

// TestRunLocalReportsMalformedYAML covers a parse failure.
func TestRunLocalReportsMalformedYAML(t *testing.T) {
	path := writeConfig(t, "accounts: [\n")
	result := RunLocal(path)
	if result.Loaded {
		t.Fatal("Loaded = true for malformed YAML")
	}
	parse, ok := findCheck(result.Checks, "config.parse")
	if !ok || parse.Status != StatusFail {
		t.Fatalf("config.parse = %+v, want a failure", parse)
	}
	if parse.Remedy == "" {
		t.Error("a failure must carry remediation guidance")
	}
}

// TestRunLocalReportsMissingAccountFields covers an account that omits a
// required field.
func TestRunLocalReportsMissingAccountFields(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: main
    backend: memory
    mailbox_root: ` + t.TempDir() + `
    permissions: [read]
`
	result := RunLocal(writeConfig(t, body))
	if result.Loaded {
		t.Fatal("Loaded = true for an account without an email")
	}
	parse, _ := findCheck(result.Checks, "config.parse")
	if parse.Status != StatusFail || !strings.Contains(parse.Detail, "email") {
		t.Fatalf("config.parse = %+v, want a failure naming the email field", parse)
	}
}

// TestRunLocalWarnsOnUnsafePermissions covers a configuration file readable
// by group or other.
func TestRunLocalWarnsOnUnsafePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	path := writeConfig(t, memoryConfigBody(t.TempDir()))
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	result := RunLocal(path)
	check, ok := findCheck(result.Checks, "config.permissions")
	if !ok {
		t.Fatal("missing config.permissions check")
	}
	if check.Status != StatusWarn {
		t.Fatalf("config.permissions = %s, want warn", check.Status)
	}
	if !strings.Contains(check.Detail, "0644") {
		t.Errorf("detail %q does not name the mode", check.Detail)
	}
	if check.Remedy == "" {
		t.Error("a warning must carry remediation guidance")
	}
	if len(failures(result.Checks)) != 0 {
		t.Error("an unsafe permission must be a warning, not a failure")
	}
}

// TestRunLocalAcceptsOwnerOnlyPermissions covers the safe mode.
func TestRunLocalAcceptsOwnerOnlyPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	path := writeConfig(t, memoryConfigBody(t.TempDir()))
	result := RunLocal(path)
	check, ok := findCheck(result.Checks, "config.permissions")
	if !ok || check.Status != StatusOK {
		t.Fatalf("config.permissions = %+v, want ok", check)
	}
}

// TestRunLocalDoesNotRevealCredentialCommandArguments covers redaction of a
// credential command: only the executable's name is reported.
func TestRunLocalDoesNotRevealCredentialCommandArguments(t *testing.T) {
	path := writeConfig(t, imapConfigBody(""))
	result := RunLocal(path)
	if !result.Loaded {
		t.Fatalf("Loaded = false; checks: %+v", result.Checks)
	}

	check, ok := findCheck(result.Checks, "account/work/credentials/imap")
	if !ok {
		t.Fatal("missing the IMAP credential check")
	}
	if !strings.Contains(check.Detail, "vault") {
		t.Errorf("detail %q does not name the executable", check.Detail)
	}
	if strings.Contains(check.Detail, "mail/production-secret") {
		t.Errorf("detail %q reveals a command argument", check.Detail)
	}

	var buf bytes.Buffer
	if err := (Report{Checks: result.Checks}).WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(buf.String(), "mail/production-secret") {
		t.Error("the rendered report reveals a credential command argument")
	}
}

// TestRunLocalWarnsAboutAnInlinePassword covers the password-in-file
// warning.
func TestRunLocalWarnsAboutAnInlinePassword(t *testing.T) {
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
      password: "inline-secret-value"
`
	result := RunLocal(writeConfig(t, body))
	check, ok := findCheck(result.Checks, "account/work/credentials/imap")
	if !ok {
		t.Fatal("missing the IMAP credential check")
	}
	if check.Status != StatusWarn {
		t.Errorf("status = %s, want warn", check.Status)
	}
	if strings.Contains(check.Detail, "inline-secret-value") {
		t.Error("the credential check reveals the password")
	}

	var buf bytes.Buffer
	if err := (Report{Checks: result.Checks}).WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if strings.Contains(buf.String(), "inline-secret-value") {
		t.Error("the rendered report reveals the password")
	}
}

// TestRunLocalFailsOnAmbiguousIMAPTLSMode covers the two configurations the
// loader accepts but the server cannot honour.
func TestRunLocalFailsOnAmbiguousIMAPTLSMode(t *testing.T) {
	cases := map[string]string{
		"both modes": `      tls: true
      starttls: true
`,
		"neither mode": ``,
	}
	for name, modes := range cases {
		t.Run(name, func(t *testing.T) {
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
` + modes + `      username: work
      password: "s3cret-value"
`
			result := RunLocal(writeConfig(t, body))
			if !result.Loaded {
				t.Fatalf("Loaded = false; checks: %+v", result.Checks)
			}
			check, ok := findCheck(result.Checks, "account/work/imap/tls-mode")
			if !ok {
				t.Fatal("missing the IMAP TLS mode check")
			}
			if check.Status != StatusFail {
				t.Errorf("status = %s, want fail", check.Status)
			}
			if check.Remedy == "" {
				t.Error("a failure must carry remediation guidance")
			}
		})
	}
}

// TestRunLocalWarnsWhenSMTPIsConfiguredWithoutSend covers an account that
// carries SMTP settings it cannot use.
func TestRunLocalWarnsWhenSMTPIsConfiguredWithoutSend(t *testing.T) {
	extra := `    smtp:
      host: smtp.example.test
      port: 587
      starttls: true
      username: work
      password: "smtp-secret-value"
`
	result := RunLocal(writeConfig(t, imapConfigBody(extra)))
	check, ok := findCheck(result.Checks, "account/work/smtp")
	if !ok {
		t.Fatal("missing the SMTP check")
	}
	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn", check.Status)
	}
	if !strings.Contains(check.Detail, "send capability is not granted") {
		t.Errorf("detail %q does not explain why", check.Detail)
	}
}

// TestRunLocalWarnsOnAttachmentSendWithoutPaths covers a capability that is
// granted but unusable.
func TestRunLocalWarnsOnAttachmentSendWithoutPaths(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: main
    backend: memory
    email: main@example.test
    mailbox_root: ` + t.TempDir() + `
    permissions: [read, attachment_send]
`
	result := RunLocal(writeConfig(t, body))
	check, ok := findCheck(result.Checks, "account/main/attachments")
	if !ok {
		t.Fatal("missing the attachment check")
	}
	if check.Status != StatusWarn {
		t.Fatalf("status = %s, want warn", check.Status)
	}
}

// TestCredentialScalarsCollectsEveryForm covers every YAML shape a
// credential can take: a scalar, a block list, a flow list, a quoted and
// escaped scalar, and a value reached through an anchor and alias.
func TestCredentialScalarsCollectsEveryForm(t *testing.T) {
	body := `accounts:
  - id: a
    imap:
      password: "scalar-secret-value"
    smtp:
      password_command:
        - vault
        - read
        - block-secret-value
  - id: b
    imap:
      password: &pw "anchored-secret-value"
      username: *pw
      password_command: ["printf", "flow-secret-value"]
  - id: c
    imap:
      password: "escaped\"secret-value"
`
	values := credentialScalars([]byte(body))
	for _, want := range []string{
		"scalar-secret-value",
		"vault", "read", "block-secret-value",
		"printf", "flow-secret-value",
		"anchored-secret-value",
		`escaped"secret-value`,
	} {
		found := false
		for _, v := range values {
			if v == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("credentialScalars() did not collect %q; got %v", want, values)
		}
	}
}

// TestDescribeConfigErrorSuppressesUntrustedContent pins the fail-closed
// rule: a YAML syntax, type or duplicate-key error quotes document content,
// so none of it is printed.
func TestDescribeConfigErrorSuppressesUntrustedContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		// buildErr produces the error the loader would return for this
		// body, so the test exercises the real classification.
	}{
		{
			name: "syntax error",
			body: "accounts: [\n  - id: syntax-canary\n",
		},
		{
			name: "type error",
			body: `accounts:
  - id: a
    limits:
      max_message_bytes: "type-canary"
`,
		},
		{
			name: "unknown field",
			body: `accounts:
  - id: a
    unknown_field_canary: true
`,
		},
		{
			name: "duplicate key",
			body: `accounts:
  - id: a
    id: duplicate-canary
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.body))
			if err == nil {
				t.Fatalf("config.Parse accepted the body; the case is not a parse failure")
			}
			got := describeConfigError(err, []byte(tc.body))
			if got == "" {
				t.Fatal("describeConfigError returned an empty description")
			}
			for _, canary := range []string{"syntax-canary", "type-canary", "unknown_field_canary", "duplicate-canary"} {
				if strings.Contains(got, canary) {
					t.Errorf("description contains untrusted document content %q: %s", canary, got)
				}
			}
			if strings.Contains(got, "cannot unmarshal") {
				t.Errorf("description contains raw parser output: %s", got)
			}
		})
	}
}

// TestSanitiseMessageRemovesConfiguredValues covers the validation branch:
// field paths and explanations survive, configured values do not.
func TestSanitiseMessageRemovesConfiguredValues(t *testing.T) {
	body := []byte(`accounts:
  - id: a
    imap:
      password: "value-canary"
      password_command:
        - vault
        - read
        - /run/secrets/command-canary
`)
	msg := `server.log_level "value-canary" invalid; want one of debug, info, warn, error; ` +
		`accounts[0].mailbox_root: stat /home/alice/Mail: no such file; ` +
		`unknown capability names: value-canary`
	got := sanitiseMessage(msg, body)

	for _, secret := range []string{"value-canary", "/home/alice/Mail", "/run/secrets/command-canary"} {
		if strings.Contains(got, secret) {
			t.Errorf("sanitised message still contains %q: %s", secret, got)
		}
	}
	for _, keep := range []string{"server.log_level", "invalid; want one of", "accounts[0].mailbox_root"} {
		if !strings.Contains(got, keep) {
			t.Errorf("sanitised message lost the useful part %q: %s", keep, got)
		}
	}
}
