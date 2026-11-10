package doctor

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunIsLocalOnlyByDefault pins the security-relevant default: without
// -network the run must not open a socket, resolve a name or execute a
// credential command.
func TestRunIsLocalOnlyByDefault(t *testing.T) {
	probe := &fakeProbe{}
	path := writeConfig(t, imapConfigBody(""))

	report := Run(context.Background(), Options{ConfigPath: path, Probe: probe})

	if len(probe.calls) != 0 {
		t.Errorf("the default run used the network: %v", probe.calls)
	}
	if report.Network {
		t.Error("Network = true, want false")
	}
	for _, c := range report.Checks {
		if strings.HasPrefix(c.Name, "account/work/imap/") && c.Status != StatusOK {
			t.Errorf("%s = %s, want ok for a valid local configuration", c.Name, c.Status)
		}
	}
}

// TestRunNetworkOptInUsesTheProbe covers the opt-in path.
func TestRunNetworkOptInUsesTheProbe(t *testing.T) {
	probe := &fakeProbe{}
	path := writeConfig(t, imapConfigBody(""))

	report := Run(context.Background(), Options{ConfigPath: path, Network: true, Probe: probe})
	if !report.Network {
		t.Error("Network = false, want true")
	}
	if len(probe.calls) == 0 {
		t.Error("the opt-in run did not use the network probe")
	}
	if !containsCheck(report.Checks, "account/work/imap/auth") {
		t.Errorf("missing the IMAP authentication check; have %v", names(report.Checks))
	}
}

// TestRunSkipsNetworkWhenTheConfigurationIsInvalid covers a broken config:
// network checks would be meaningless, so none run.
func TestRunSkipsNetworkWhenTheConfigurationIsInvalid(t *testing.T) {
	probe := &fakeProbe{}
	path := writeConfig(t, "accounts: [\n")

	report := Run(context.Background(), Options{ConfigPath: path, Network: true, Probe: probe})
	if len(probe.calls) != 0 {
		t.Errorf("the network was used despite an invalid configuration: %v", probe.calls)
	}
	check, ok := findCheck(report.Checks, "network")
	if !ok || check.Status != StatusSkip {
		t.Fatalf("network = %+v, want a skip", check)
	}
	if !report.Failed() {
		t.Error("Failed() = false for an invalid configuration")
	}
}

// TestRunBoundsTheNetworkPhase covers the overall timeout.
func TestRunBoundsTheNetworkPhase(t *testing.T) {
	probe := &fakeProbe{blockLookup: true}
	path := writeConfig(t, imapConfigBody(""))

	start := time.Now()
	report := Run(context.Background(), Options{
		ConfigPath: path,
		Network:    true,
		Timeout:    150 * time.Millisecond,
		Probe:      probe,
	})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("Run took %s; the network bound was not honoured", elapsed)
	}
	dns, ok := findCheck(report.Checks, "account/work/imap/dns")
	if !ok || dns.Status != StatusFail {
		t.Fatalf("dns = %+v, want a timeout failure", dns)
	}
}

// TestReportFailedOnlyOnFailures pins that warnings and skips do not fail
// the run.
func TestReportFailedOnlyOnFailures(t *testing.T) {
	report := Report{Checks: []Check{
		{Name: "a", Status: StatusOK},
		{Name: "b", Status: StatusWarn},
		{Name: "c", Status: StatusSkip},
	}}
	if report.Failed() {
		t.Error("Failed() = true for warnings and skips")
	}
	report.Checks = append(report.Checks, Check{Name: "d", Status: StatusFail})
	if !report.Failed() {
		t.Error("Failed() = false with a failure present")
	}
}

// TestReportWriteTextIsStable covers the operator-facing rendering.
func TestReportWriteTextIsStable(t *testing.T) {
	report := Report{
		ConfigPath: "/etc/themailroom/themailroom.yaml",
		Network:    true,
		Checks: []Check{
			{Name: "config.file", Status: StatusOK, Detail: "readable"},
			{Name: "config.permissions", Status: StatusWarn, Detail: "mode 0644", Remedy: "chmod 600"},
			{Name: "config.parse", Status: StatusFail, Detail: "invalid", Remedy: "fix it"},
			{Name: "network", Status: StatusSkip, Detail: "not requested"},
		},
	}
	var buf bytes.Buffer
	if err := report.WriteText(&buf); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Config:  /etc/themailroom/themailroom.yaml",
		"Mode:    local and network diagnostics",
		"  ok    config.file",
		"  warn  config.permissions",
		"chmod 600",
		"  fail  config.parse",
		"fix it",
		"  skip  network",
		"Summary: 4 checks, 1 ok, 1 warning(s), 1 failure(s), 1 skipped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report does not contain %q:\n%s", want, out)
		}
	}
}

func containsCheck(checks []Check, name string) bool {
	_, ok := findCheck(checks, name)
	return ok
}
