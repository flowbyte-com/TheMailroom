package doctor

import (
	"context"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// cancellingProbe cancels the run's context after the named account's IMAP
// check completes, so later accounts see a cancelled phase.
type cancellingProbe struct {
	*fakeProbe
	cancel       context.CancelFunc
	afterAccount string
}

func (p *cancellingProbe) IMAP(ctx context.Context, acc config.AccountConfig) error {
	err := p.fakeProbe.IMAP(ctx, acc)
	if acc.ID == p.afterAccount {
		p.cancel()
	}
	return err
}

// TestRequestedNetworkCancelledBeforeStartFails covers cancellation before
// the network phase begins. The run was explicitly asked for network
// diagnostics, so an incomplete phase must not be reported as success.
func TestRequestedNetworkCancelledBeforeStartFails(t *testing.T) {
	path := writeConfig(t, imapConfigBody(""))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report := Run(ctx, Options{ConfigPath: path, Network: true, Probe: &fakeProbe{}})
	if !report.Failed() {
		t.Errorf("Failed() = false for a cancelled requested network diagnostic; checks: %+v", report.Checks)
	}
}

// TestRequestedNetworkDeadlineBetweenAccountsFails covers a deadline that
// expires after one account completed and before the next was checked.
func TestRequestedNetworkDeadlineBetweenAccountsFails(t *testing.T) {
	dir := t.TempDir()
	body := twoIMAPAccounts(dir)
	path := writeConfig(t, body)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := &cancellingProbe{fakeProbe: &fakeProbe{}, cancel: cancel, afterAccount: "first"}

	report := Run(ctx, Options{ConfigPath: path, Network: true, Probe: probe})
	if !report.Failed() {
		t.Errorf("Failed() = false when the phase was cancelled between accounts; checks: %+v", report.Checks)
	}
	// The first account completed successfully.
	if got := statusOf(t, report.Checks, "account/first/imap/auth"); got != StatusOK {
		t.Errorf("account/first/imap/auth = %s, want ok", got)
	}
	// The second account did not run.
	if got := statusOf(t, report.Checks, "account/second/imap/dns"); got != StatusSkip {
		t.Errorf("account/second/imap/dns = %s, want skip", got)
	}
}

// TestSuccessfulRequestedNetworkDoesNotFail covers the success path.
func TestSuccessfulRequestedNetworkDoesNotFail(t *testing.T) {
	path := writeConfig(t, imapConfigBody(""))
	report := Run(context.Background(), Options{ConfigPath: path, Network: true, Probe: &fakeProbe{}})
	if report.Failed() {
		t.Errorf("Failed() = true for a fully successful run; checks: %+v", report.Checks)
	}
	if got := statusOf(t, report.Checks, "account/work/imap/auth"); got != StatusOK {
		t.Errorf("account/work/imap/auth = %s, want ok", got)
	}
}

// TestNonApplicableChecksDoNotFail covers a memory-only account with
// -network: there is nothing to check, so the skip must not fail the run.
func TestNonApplicableChecksDoNotFail(t *testing.T) {
	body := `server:
  log_level: info

accounts:
  - id: local
    backend: memory
    email: local@example.test
    mailbox_root: ` + t.TempDir() + `
    permissions: [read]
`
	path := writeConfig(t, body)
	report := Run(context.Background(), Options{ConfigPath: path, Network: true, Probe: &fakeProbe{}})
	if report.Failed() {
		t.Errorf("Failed() = true for a non-applicable network check; checks: %+v", report.Checks)
	}
}

// twoIMAPAccounts builds two independent IMAP accounts.
func twoIMAPAccounts(dir string) string {
	return `server:
  log_level: info

accounts:
  - id: first
    backend: imap
    email: first@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: first
      password: "s3cret"
  - id: second
    backend: imap
    email: second@example.test
    permissions: [read]
    imap:
      host: mail.example.test
      port: 993
      tls: true
      username: second
      password: "s3cret"
`
}
