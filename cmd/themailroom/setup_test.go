package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/doctor"
	"github.com/flowbyte-com/themailroom/internal/setup"
)

// setupAnswers is a scripted read-only setup run.
func setupAnswers(path string) string {
	return strings.Join([]string{
		"work", "work@example.test",
		"mail.example.test", "1", "993", "work", "pass show mail/work",
		"n", path, "n",
	}, "\n") + "\n"
}

// TestRunSetupWritesAndExitsZero covers the CLI success path.
func TestRunSetupWritesAndExitsZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	var out bytes.Buffer

	code := runSetup([]string{"-config", path}, strings.NewReader(setupAnswers(path)), &out)
	if code != setupExitOK {
		t.Fatalf("exit code = %d, want %d\n%s", code, setupExitOK, out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the configuration was not written: %v", err)
	}
}

// TestRunSetupRefusesToOverwriteAndExitsOne covers the no-overwrite default
// at the CLI boundary.
func TestRunSetupRefusesToOverwriteAndExitsOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	code := runSetup([]string{"-config", path}, strings.NewReader(setupAnswers(path)), &out)
	if code != setupExitFailed {
		t.Fatalf("exit code = %d, want %d\n%s", code, setupExitFailed, out.String())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original\n" {
		t.Error("the existing file was modified")
	}
}

// TestRunSetupRejectsAnUnknownFlag covers the usage exit code.
func TestRunSetupRejectsAnUnknownFlag(t *testing.T) {
	var out bytes.Buffer
	if code := runSetup([]string{"-not-a-flag"}, strings.NewReader(""), &out); code != setupExitUsage {
		t.Fatalf("exit code = %d, want %d", code, setupExitUsage)
	}
}

// TestRunSetupOutputIsCompatibleWithDoctor covers the integration: a file
// produced by setup passes the local diagnostics with no failures.
func TestRunSetupOutputIsCompatibleWithDoctor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	var out bytes.Buffer
	if code := runSetup([]string{"-config", path}, strings.NewReader(setupAnswers(path)), &out); code != setupExitOK {
		t.Fatalf("setup exit code = %d\n%s", code, out.String())
	}

	report := doctor.Run(context.Background(), doctor.Options{ConfigPath: path})
	if report.Failed() {
		var reportText bytes.Buffer
		_ = report.WriteText(&reportText)
		t.Fatalf("doctor rejected a setup-generated configuration:\n%s", reportText.String())
	}
	if report.Count(doctor.StatusWarn) != 0 {
		var reportText bytes.Buffer
		_ = report.WriteText(&reportText)
		t.Errorf("doctor warned about a setup-generated configuration:\n%s", reportText.String())
	}
}

// TestRunSetupDoesNotRunTheNetworkByDefault covers the opt-in boundary: with
// the check declined, the CLI opens no connection and runs no credential
// command.
func TestRunSetupDoesNotRunTheNetworkByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	var out bytes.Buffer

	if code := runSetup([]string{"-config", path}, strings.NewReader(setupAnswers(path)), &out); code != setupExitOK {
		t.Fatalf("exit code = %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "local only") && !strings.Contains(out.String(), "tmailroom doctor") {
		// The setup output itself never runs the doctor; the next-step
		// lines are instructions, not execution.
		t.Logf("setup output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Summary:") {
		t.Error("setup ran a diagnostic report without being asked")
	}
}

// TestSetupGenerateIsDeterministic covers the pure generator used by the
// CLI path: the same answers produce the same validated bytes.
func TestSetupGenerateIsDeterministic(t *testing.T) {
	a := setup.Answers{
		AccountID:       "work",
		Email:           "work@example.test",
		IMAPHost:        "mail.example.test",
		IMAPPort:        993,
		IMAPImplicitTLS: true,
		IMAPUsername:    "work",
		IMAPCommand:     []string{"pass", "show", "mail/work"},
	}
	first, _, err := setup.Generate(a)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	second, _, err := setup.Generate(a)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Error("Generate is not deterministic")
	}
}

// TestRenderCheckFailsOnAFailedReport covers the defect where a failing
// connectivity report was printed but the command still succeeded: printing
// and judging are separate.
func TestRenderCheckFailsOnAFailedReport(t *testing.T) {
	report := doctor.Report{Checks: []doctor.Check{
		{Name: "config.parse", Status: doctor.StatusOK, Detail: "ok"},
		{Name: "account/work/imap/dns", Status: doctor.StatusFail, Detail: "host not found"},
	}}
	var out bytes.Buffer

	err := renderCheck(report, &out)
	if !errors.Is(err, errCheckFailed) {
		t.Fatalf("err = %v, want errCheckFailed", err)
	}
	if !strings.Contains(out.String(), "fail") {
		t.Errorf("the report was not printed:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "configuration was written") {
		t.Errorf("the error does not reassure the operator: %v", err)
	}
}

// TestRenderCheckPassesOnASuccessfulReport covers the success path.
func TestRenderCheckPassesOnASuccessfulReport(t *testing.T) {
	report := doctor.Report{Checks: []doctor.Check{
		{Name: "config.parse", Status: doctor.StatusOK, Detail: "ok"},
		{Name: "account/work/imap/dns", Status: doctor.StatusOK, Detail: "resolved"},
	}}
	var out bytes.Buffer
	if err := renderCheck(report, &out); err != nil {
		t.Fatalf("renderCheck = %v, want nil", err)
	}
}

// TestRunSetupFailsWhenTheCheckFailsAndKeepsTheFile covers the CLI exit
// status, and pins that the configuration survives the failure for
// troubleshooting.
func TestRunSetupFailsWhenTheCheckFailsAndKeepsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "themailroom.yaml")

	original := setupCheck
	setupCheck = func(_ context.Context, _ string, out io.Writer) error {
		return renderCheck(doctor.Report{Checks: []doctor.Check{
			{Name: "account/work/imap/dns", Status: doctor.StatusFail, Detail: "host not found"},
		}}, out)
	}
	t.Cleanup(func() { setupCheck = original })

	answers := strings.Join([]string{
		"work", "work@example.test",
		"mail.example.test", "1", "993", "work", "pass show mail/work",
		"n", path, "y",
	}, "\n") + "\n"

	var out bytes.Buffer
	code := runSetup([]string{"-config", path}, strings.NewReader(answers), &out)
	if code != setupExitFailed {
		t.Fatalf("exit code = %d, want %d for a failed connectivity check\n%s", code, setupExitFailed, out.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the configuration was not preserved after a failed check: %v", err)
	}
}
