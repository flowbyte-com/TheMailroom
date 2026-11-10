package setup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// readOnlyAnswers is a valid read-only IMAP configuration.
func readOnlyAnswers() Answers {
	return Answers{
		AccountID:       "work",
		Email:           "work@example.test",
		IMAPHost:        "mail.example.test",
		IMAPPort:        config.DefaultIMAPPort,
		IMAPImplicitTLS: true,
		IMAPUsername:    "work",
		IMAPCommand:     []string{"pass", "show", "mail/work"},
	}
}

// sendAnswers is a valid configuration with submission enabled.
func sendAnswers() Answers {
	a := readOnlyAnswers()
	a.EnableSend = true
	a.SMTPHost = "smtp.example.test"
	a.SMTPPort = config.DefaultSMTPPort
	a.SMTPImplicitTLS = false
	a.SMTPUsername = "work"
	a.SMTPCommand = []string{"pass", "show", "mail/work"}
	return a
}

// TestGenerateReadOnlyIMAP covers the safe default: read-only, implicit TLS,
// no SMTP, no attachments.
func TestGenerateReadOnlyIMAP(t *testing.T) {
	data, cfg, err := Generate(readOnlyAnswers())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(cfg.Accounts))
	}
	acc := cfg.Accounts[0]
	if got := strings.Join(acc.Permissions, ","); got != "read" {
		t.Errorf("permissions = %q, want read", got)
	}
	if !acc.IMAP.TLS || acc.IMAP.StartTLS {
		t.Errorf("IMAP TLS modes = tls:%v starttls:%v, want implicit TLS only", acc.IMAP.TLS, acc.IMAP.StartTLS)
	}
	if acc.IMAP.Port != config.DefaultIMAPPort {
		t.Errorf("IMAP port = %d, want %d", acc.IMAP.Port, config.DefaultIMAPPort)
	}
	if acc.SMTP.Host != "" {
		t.Errorf("SMTP host = %q, want empty for a read-only account", acc.SMTP.Host)
	}
	if len(acc.AttachmentSendPaths) != 0 {
		t.Errorf("attachment paths = %v, want none", acc.AttachmentSendPaths)
	}

	// The bytes that would be written are exactly what was validated: the
	// real loader accepts them and produces the same configuration.
	round, err := config.Parse(data)
	if err != nil {
		t.Fatalf("the generated file does not re-parse: %v", err)
	}
	if len(round.Accounts) != 1 || round.Accounts[0].ID != acc.ID {
		t.Errorf("round-trip produced a different account: %+v", round.Accounts)
	}
	if strings.Contains(string(data), "password:") {
		t.Error("the generated file contains a password field")
	}
	if strings.Contains(string(data), "attachment_send") {
		t.Error("the generated file grants attachment_send")
	}
}

// TestGenerateSendOptIn covers the explicit send capability.
func TestGenerateSendOptIn(t *testing.T) {
	_, cfg, err := Generate(sendAnswers())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	acc := cfg.Accounts[0]
	if got := strings.Join(acc.Permissions, ","); got != "read,send" {
		t.Errorf("permissions = %q, want read,send", got)
	}
	if acc.SMTP.Host != "smtp.example.test" || acc.SMTP.Port != config.DefaultSMTPPort {
		t.Errorf("SMTP = %s:%d", acc.SMTP.Host, acc.SMTP.Port)
	}
	if !acc.SMTP.StartTLS || acc.SMTP.TLS {
		t.Errorf("SMTP TLS modes = tls:%v starttls:%v, want STARTTLS only", acc.SMTP.TLS, acc.SMTP.StartTLS)
	}
	if len(acc.AttachmentSendPaths) != 0 {
		t.Error("send must not imply attachment sending")
	}
}

// TestGenerateStartTLSIMAP covers the alternative transport mode.
func TestGenerateStartTLSIMAP(t *testing.T) {
	a := readOnlyAnswers()
	a.IMAPImplicitTLS = false
	a.IMAPPort = config.DefaultIMAPPortStartTLS
	_, cfg, err := Generate(a)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if cfg.Accounts[0].IMAP.TLS {
		t.Error("implicit TLS must be off")
	}
	if !cfg.Accounts[0].IMAP.StartTLS {
		t.Error("STARTTLS must be on")
	}
}

// TestGenerateRejectsInvalidAnswers covers a value the loader will not
// accept.
func TestGenerateRejectsInvalidAnswers(t *testing.T) {
	a := readOnlyAnswers()
	a.Email = ""
	if _, _, err := Generate(a); err == nil {
		t.Fatal("Generate accepted an empty email")
	} else if !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("error does not say nothing was written: %v", err)
	}
}

// TestGenerateDoesNotEchoInvalidValues pins that a rejected value is not
// printed back.
func TestGenerateDoesNotEchoInvalidValues(t *testing.T) {
	a := readOnlyAnswers()
	a.AccountID = "bad id with spaces"
	_, _, err := Generate(a)
	if err == nil {
		t.Fatal("Generate accepted an account id with spaces")
	}
	if strings.Contains(err.Error(), "bad id with spaces") {
		t.Errorf("the error echoes the rejected value: %v", err)
	}
}

// runScript feeds a scripted answer sequence to Run.
func runScript(t *testing.T, opts Options, answers ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	opts.In = strings.NewReader(strings.Join(answers, "\n") + "\n")
	opts.Out = &out
	err := Run(context.Background(), opts)
	return out.String(), err
}

// readOnlyScript returns the answers for a read-only setup.
func readOnlyScript(path, command string) []string {
	return []string{
		"work",
		"work@example.test",
		"mail.example.test",
		"1",
		"993",
		"work",
		command,
		"n",
		path,
		"n",
	}
}

// TestRunWritesAValidConfiguration covers the full read-only flow.
func TestRunWritesAValidConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	out, err := runScript(t, Options{Path: path}, readOnlyScript(path, "pass show mail/work")...)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("the configuration was not written: %v", statErr)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the written configuration does not load: %v", err)
	}
	for _, want := range []string{"Wrote " + path, "themailroom doctor -config " + path, "permissions: read"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

// TestRunDoesNotPrintCredentialArguments covers confidentiality: the command
// is reported by its executable name only.
func TestRunDoesNotPrintCredentialArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	out, err := runScript(t, Options{Path: path}, readOnlyScript(path, "vault read secret/mail-room")...)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if strings.Contains(out, "secret/mail-room") {
		t.Errorf("the output reveals a credential command argument:\n%s", out)
	}
	if !strings.Contains(out, "vault") {
		t.Errorf("the output does not name the executable:\n%s", out)
	}

	// The file legitimately carries the command; it must not carry a
	// password field.
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "secret/mail-room") {
		t.Error("the configuration does not contain the credential command")
	}
	if strings.Contains(string(data), "password:") {
		t.Error("the configuration contains a password field")
	}
}

// TestRunDoesNotCheckUnlessAsked covers the opt-in boundary: the network
// check is not run, and no credential command is executed, by default.
func TestRunDoesNotCheckUnlessAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	called := false
	out, err := runScript(t, Options{
		Path: path,
		Check: func(context.Context, string, io.Writer) error {
			called = true
			return nil
		},
	}, readOnlyScript(path, "pass show mail/work")...)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if called {
		t.Error("the connectivity check ran without being asked")
	}
}

// TestRunRunsCheckWhenAsked covers the explicit opt-in.
func TestRunRunsCheckWhenAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	called := false
	answers := readOnlyScript(path, "pass show mail/work")
	answers[len(answers)-1] = "y"
	var out bytes.Buffer
	err := Run(context.Background(), Options{
		Path: path,
		In:   strings.NewReader(strings.Join(answers, "\n") + "\n"),
		Out:  &out,
		Check: func(_ context.Context, p string, _ io.Writer) error {
			called = true
			if p != path {
				t.Errorf("check path = %q, want %q", p, path)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Error("the connectivity check did not run when asked")
	}
}

// TestRunInputClosed covers cancellation: nothing is written.
func TestRunInputClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "themailroom.yaml")
	_, err := runScript(t, Options{Path: path}, "work", "work@example.test", "mail.example.test")
	if err == nil {
		t.Fatal("Run accepted truncated input")
	}
	if !errors.Is(err, errInputClosed) {
		t.Errorf("err = %v, want errInputClosed", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("a configuration was written from truncated input")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the directory is not empty after cancellation: %v", entries)
	}
}

// TestRunRepromptsOnABadPort covers malformed input that is recoverable.
func TestRunRepromptsOnABadPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	answers := []string{
		"work", "work@example.test", "mail.example.test", "1",
		"not-a-port", "70000", "993",
		"work", "pass show mail/work", "n", path, "n",
	}
	out, err := runScript(t, Options{Path: path}, answers...)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "between 1 and 65535") {
		t.Errorf("the port was not validated:\n%s", out)
	}
	cfg, loadErr := config.Load(path)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if cfg.Accounts[0].IMAP.Port != 993 {
		t.Errorf("port = %d, want 993", cfg.Accounts[0].IMAP.Port)
	}
}

// TestRunRefusesAnExistingFile covers the no-overwrite default.
func TestRunRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runScript(t, Options{Path: path}, readOnlyScript(path, "pass show mail/work")...)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original\n" {
		t.Error("the existing file was modified")
	}
}

// TestRunForceReplacesAnExistingFile covers the explicit override.
func TestRunForceReplacesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runScript(t, Options{Path: path, Force: true}, readOnlyScript(path, "pass show mail/work")...); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("the replaced file does not load: %v", err)
	}
}

// TestRunSendFlow covers the SMTP questions and the send capability.
func TestRunSendFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	answers := []string{
		"work", "work@example.test",
		"mail.example.test", "1", "993", "work", "pass show mail/work",
		"y",
		"smtp.example.test", "1", "587", "work", "pass show mail/work",
		path, "n",
	}
	out, err := runScript(t, Options{Path: path}, answers...)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out)
	}
	cfg, loadErr := config.Load(path)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if got := strings.Join(cfg.Accounts[0].Permissions, ","); got != "read,send" {
		t.Errorf("permissions = %q, want read,send", got)
	}
	if !cfg.Accounts[0].SMTP.StartTLS {
		t.Error("SMTP must use STARTTLS by default")
	}
}

// TestPermissionNote covers the platform-specific security statement.
func TestPermissionNote(t *testing.T) {
	posix := permissionNoteFor("linux")
	if !strings.Contains(posix, "0600") {
		t.Errorf("the POSIX note does not state the mode: %s", posix)
	}

	windows := permissionNoteFor("windows")
	if !strings.Contains(windows, "ACL") {
		t.Errorf("the Windows note does not mention ACLs: %s", windows)
	}
	if strings.Contains(windows, "mode 0600") {
		t.Errorf("the Windows note claims POSIX protection: %s", windows)
	}
}

// TestSplitCommand covers the argument splitter.
func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"pass show mail/work", []string{"pass", "show", "mail/work"}},
		{"  pass   show   mail/work  ", []string{"pass", "show", "mail/work"}},
		{`pass show "mail/work item"`, []string{"pass", "show", "mail/work item"}},
		{`pass show 'mail/work item'`, []string{"pass", "show", "mail/work item"}},
		{`cmd a\ b`, []string{"cmd", "a b"}},
		{`cmd "a\"b"`, []string{"cmd", `a"b`}},
		{`cmd ''`, []string{"cmd", ""}},
	}
	for _, tc := range cases {
		got, err := SplitCommand(tc.in)
		if err != nil {
			t.Errorf("SplitCommand(%q): %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("SplitCommand(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("SplitCommand(%q) = %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}

	for _, bad := range []string{"", "   ", `cmd "unterminated`, `cmd trailing\`} {
		if _, err := SplitCommand(bad); err == nil {
			t.Errorf("SplitCommand(%q) accepted a malformed command", bad)
		}
	}
}

// TestRunRejectsAnInvalidEmail covers a value the loader refuses: nothing is
// written and the rejected value is not echoed.
func TestRunRejectsAnInvalidEmail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "themailroom.yaml")
	answers := []string{
		"work", "not-an-email",
		"mail.example.test", "1", "993", "work", "pass show mail/work",
		"n", path, "n",
	}
	out, err := runScript(t, Options{Path: path}, answers...)
	if err == nil {
		t.Fatalf("Run accepted an invalid email address\n%s", out)
	}
	if strings.Contains(err.Error(), "not-an-email") {
		t.Errorf("the error echoes the rejected value: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("a configuration was written from invalid input")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the directory is not empty after a rejected configuration: %v", entries)
	}
}
