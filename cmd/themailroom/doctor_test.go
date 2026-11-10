package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testConfig writes a minimal valid configuration with a memory account.
func testConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "themailroom.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const validBody = `server:
  log_level: info

accounts:
  - id: main
    backend: memory
    email: main@example.test
    mailbox_root: MAILBOX_ROOT
    permissions: [read]
`

// TestRunDoctorReportsAValidConfiguration covers the success exit code.
func TestRunDoctorReportsAValidConfiguration(t *testing.T) {
	path := testConfig(t, strings.ReplaceAll(validBody, "MAILBOX_ROOT", t.TempDir()))
	var out bytes.Buffer

	code := runDoctor([]string{"-config", path}, &out)
	if code != doctorExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, doctorExitOK, out.String())
	}
	if !strings.Contains(out.String(), "themailroom doctor") {
		t.Errorf("output does not look like a report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Summary:") {
		t.Errorf("output has no summary:\n%s", out.String())
	}
}

// TestRunDoctorFailsOnAnInvalidConfiguration covers the failure exit code.
func TestRunDoctorFailsOnAnInvalidConfiguration(t *testing.T) {
	path := testConfig(t, "accounts: [\n")
	var out bytes.Buffer

	code := runDoctor([]string{"-config", path}, &out)
	if code != doctorExitFailed {
		t.Fatalf("exit code = %d, want %d\n%s", code, doctorExitFailed, out.String())
	}
	if !strings.Contains(out.String(), "fail") {
		t.Errorf("output has no failure line:\n%s", out.String())
	}
}

// TestRunDoctorFailsOnAMissingConfiguration covers a missing file.
func TestRunDoctorFailsOnAMissingConfiguration(t *testing.T) {
	var out bytes.Buffer
	code := runDoctor([]string{"-config", filepath.Join(t.TempDir(), "absent.yaml")}, &out)
	if code != doctorExitFailed {
		t.Fatalf("exit code = %d, want %d\n%s", code, doctorExitFailed, out.String())
	}
}

// TestRunDoctorRejectsAnUnknownFlag covers the usage exit code.
func TestRunDoctorRejectsAnUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	code := runDoctor([]string{"-not-a-flag"}, &out)
	if code != doctorExitUsage {
		t.Fatalf("exit code = %d, want %d", code, doctorExitUsage)
	}
}

// TestRunDoctorDoesNotReachTheNetworkByDefault covers the opt-in boundary
// at the command level: the report must say the network was not used.
func TestRunDoctorDoesNotReachTheNetworkByDefault(t *testing.T) {
	path := testConfig(t, strings.ReplaceAll(validBody, "MAILBOX_ROOT", t.TempDir()))
	var out bytes.Buffer

	if code := runDoctor([]string{"-config", path}, &out); code != doctorExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, doctorExitOK, out.String())
	}
	if !strings.Contains(out.String(), "local only") {
		t.Errorf("output does not state that the run is local only:\n%s", out.String())
	}
}
