package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/smtpclient"
)

// fakeProbe is a deterministic network stand-in.
type fakeProbe struct {
	lookupAddrs []string
	lookupErr   error
	dialErr     error
	tlsState    tls.ConnectionState
	tlsErr      error
	imapErr     error
	smtpResult  smtpclient.ProbeResult
	smtpErr     error

	blockLookup bool
	blockDial   bool

	calls []string
}

func (f *fakeProbe) LookupHost(ctx context.Context, host string) ([]string, error) {
	f.calls = append(f.calls, "lookup:"+host)
	if f.blockLookup {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	if f.lookupAddrs != nil {
		return f.lookupAddrs, nil
	}
	return []string{"192.0.2.10"}, nil
}

func (f *fakeProbe) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	f.calls = append(f.calls, "dial:"+addr)
	if f.blockDial {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.dialErr != nil {
		return nil, f.dialErr
	}
	client, server := net.Pipe()
	_ = server.Close()
	return client, nil
}

func (f *fakeProbe) TLSState(ctx context.Context, addr, serverName string) (tls.ConnectionState, error) {
	f.calls = append(f.calls, "tls:"+serverName)
	if f.tlsErr != nil {
		return tls.ConnectionState{}, f.tlsErr
	}
	if len(f.tlsState.PeerCertificates) != 0 {
		return f.tlsState, nil
	}
	return tls.ConnectionState{
		Version:     tls.VersionTLS13,
		CipherSuite: tls.TLS_AES_128_GCM_SHA256,
		PeerCertificates: []*x509.Certificate{{
			NotAfter: time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
			Issuer:   pkix.Name{Organization: []string{"Test Authority"}},
		}},
	}, nil
}

func (f *fakeProbe) IMAP(ctx context.Context, acc config.AccountConfig) error {
	f.calls = append(f.calls, "imap:"+acc.ID)
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.imapErr
}

func (f *fakeProbe) SMTPAuth(ctx context.Context, acc config.AccountConfig) (smtpclient.ProbeResult, error) {
	f.calls = append(f.calls, "smtp:"+acc.ID)
	if err := ctx.Err(); err != nil {
		return smtpclient.ProbeResult{}, err
	}
	if f.smtpErr != nil {
		return smtpclient.ProbeResult{}, f.smtpErr
	}
	if f.smtpResult.TLSVersion == "" {
		return smtpclient.ProbeResult{TLSVersion: "TLS 1.3", Authenticated: true}, nil
	}
	return f.smtpResult, nil
}

// oneAccountConfig builds an IMAP account, optionally with submission.
func oneAccountConfig(send bool, startTLS bool) config.Config {
	acc := config.AccountConfig{
		ID:          "work",
		Email:       "work@example.test",
		Backend:     "imap",
		Permissions: []string{"read"},
		IMAP: config.IMAPConfig{
			Host: "mail.example.test", Port: 993, TLS: true,
			Username: "work", Password: "s3cret",
		},
	}
	if startTLS {
		acc.IMAP = config.IMAPConfig{
			Host: "mail.example.test", Port: 143, StartTLS: true,
			Username: "work", Password: "s3cret",
		}
	}
	if send {
		acc.Permissions = append(acc.Permissions, "send")
		acc.SMTP = config.SMTPConfig{
			Host: "smtp.example.test", Port: 587, StartTLS: true,
			Username: "work", Password: "s3cret",
		}
	}
	return config.Config{Accounts: []config.AccountConfig{acc}}
}

func statusOf(t *testing.T, checks []Check, name string) Status {
	t.Helper()
	c, ok := findCheck(checks, name)
	if !ok {
		t.Fatalf("missing check %q; have %v", name, names(checks))
	}
	return c.Status
}

func names(checks []Check) []string {
	out := make([]string, len(checks))
	for i, c := range checks {
		out[i] = c.Name
	}
	return out
}

// TestRunNetworkReportsEveryLayerOnSuccess covers the happy path for both
// protocols.
func TestRunNetworkReportsEveryLayerOnSuccess(t *testing.T) {
	probe := &fakeProbe{}
	cfg := oneAccountConfig(true, false)
	// Use implicit TLS for SMTP so every layer has a dedicated check; the
	// STARTTLS case is covered separately.
	cfg.Accounts[0].SMTP.TLS = true
	cfg.Accounts[0].SMTP.StartTLS = false
	checks := RunNetwork(context.Background(), cfg, probe)

	for _, name := range []string{
		"account/work/imap/dns", "account/work/imap/tcp", "account/work/imap/tls", "account/work/imap/auth",
		"account/work/smtp/dns", "account/work/smtp/tcp", "account/work/smtp/tls", "account/work/smtp/auth",
	} {
		if got := statusOf(t, checks, name); got != StatusOK {
			t.Errorf("%s = %s, want ok", name, got)
		}
	}
	if got := failures(checks); len(got) != 0 {
		t.Errorf("unexpected failures: %+v", got)
	}
}

// TestRunNetworkSkipsDependentChecksWhenDNSFails covers the cascade: the
// later layers are skipped rather than repeating the same cause.
func TestRunNetworkSkipsDependentChecksWhenDNSFails(t *testing.T) {
	probe := &fakeProbe{lookupErr: &net.DNSError{Err: "no such host", Name: "mail.example.test", IsNotFound: true}}
	checks := RunNetwork(context.Background(), oneAccountConfig(false, false), probe)

	if got := statusOf(t, checks, "account/work/imap/dns"); got != StatusFail {
		t.Errorf("dns = %s, want fail", got)
	}
	for _, name := range []string{"account/work/imap/tcp", "account/work/imap/tls", "account/work/imap/auth"} {
		if got := statusOf(t, checks, name); got != StatusSkip {
			t.Errorf("%s = %s, want skip", name, got)
		}
	}
	dns, _ := findCheck(checks, "account/work/imap/dns")
	if !strings.Contains(dns.Detail, "host not found") {
		t.Errorf("dns detail %q does not classify the failure", dns.Detail)
	}
}

// TestRunNetworkSkipsDependentChecksWhenTCPFails covers the TCP cascade.
func TestRunNetworkSkipsDependentChecksWhenTCPFails(t *testing.T) {
	probe := &fakeProbe{dialErr: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}}
	checks := RunNetwork(context.Background(), oneAccountConfig(false, false), probe)

	if got := statusOf(t, checks, "account/work/imap/tcp"); got != StatusFail {
		t.Errorf("tcp = %s, want fail", got)
	}
	for _, name := range []string{"account/work/imap/tls", "account/work/imap/auth"} {
		if got := statusOf(t, checks, name); got != StatusSkip {
			t.Errorf("%s = %s, want skip", name, got)
		}
	}
}

// TestRunNetworkReportsAnExpiredCertificate covers TLS verification failure
// and the sanitised reason.
func TestRunNetworkReportsAnExpiredCertificate(t *testing.T) {
	probe := &fakeProbe{tlsErr: x509.CertificateInvalidError{Reason: x509.Expired}}
	checks := RunNetwork(context.Background(), oneAccountConfig(false, false), probe)

	tlsCheck, _ := findCheck(checks, "account/work/imap/tls")
	if tlsCheck.Status != StatusFail {
		t.Fatalf("tls = %s, want fail", tlsCheck.Status)
	}
	if !strings.Contains(tlsCheck.Detail, "expired") {
		t.Errorf("detail %q does not explain the certificate failure", tlsCheck.Detail)
	}
	if got := statusOf(t, checks, "account/work/imap/auth"); got != StatusSkip {
		t.Errorf("auth = %s, want skip when TLS fails", got)
	}
}

// TestRunNetworkReportsAHostnameMismatch covers the hostname verification
// failure.
func TestRunNetworkReportsAHostnameMismatch(t *testing.T) {
	probe := &fakeProbe{tlsErr: x509.HostnameError{Host: "mail.example.test"}}
	checks := RunNetwork(context.Background(), oneAccountConfig(false, false), probe)
	tlsCheck, _ := findCheck(checks, "account/work/imap/tls")
	if tlsCheck.Status != StatusFail || !strings.Contains(tlsCheck.Detail, "does not match the host name") {
		t.Fatalf("tls = %+v, want a hostname mismatch failure", tlsCheck)
	}
}

// TestRunNetworkDoesNotEchoIMAPServerText covers sanitisation: the IMAP
// login error can carry an arbitrary server response.
func TestRunNetworkDoesNotEchoIMAPServerText(t *testing.T) {
	probe := &fakeProbe{imapErr: errors.New("imap: login: NO [AUTHENTICATIONFAILED] server-text-canary")}
	checks := RunNetwork(context.Background(), oneAccountConfig(false, false), probe)

	auth, _ := findCheck(checks, "account/work/imap/auth")
	if auth.Status != StatusFail {
		t.Fatalf("auth = %s, want fail", auth.Status)
	}
	if strings.Contains(auth.Detail, "server-text-canary") {
		t.Errorf("the detail echoes server text: %q", auth.Detail)
	}
	if !strings.Contains(auth.Detail, "login did not complete") {
		t.Errorf("detail %q does not name the failing stage", auth.Detail)
	}
}

// TestRunNetworkReportsAnIMAPTimeout covers the timeout classification.
func TestRunNetworkReportsAnIMAPTimeout(t *testing.T) {
	probe := &fakeProbe{imapErr: fmt.Errorf("%w: %w", mailbox.ErrTimeout, context.DeadlineExceeded)}
	checks := RunNetwork(context.Background(), oneAccountConfig(false, false), probe)

	auth, _ := findCheck(checks, "account/work/imap/auth")
	if auth.Status != StatusFail || !strings.Contains(auth.Detail, "timed out") {
		t.Fatalf("auth = %+v, want a timeout failure", auth)
	}
}

// TestRunNetworkReportsSMTPAuthenticationFailure covers the typed SMTP
// classification.
func TestRunNetworkReportsSMTPAuthenticationFailure(t *testing.T) {
	probe := &fakeProbe{smtpErr: fmt.Errorf("%w: authentication rejected", smtpclient.ErrAuthFailed)}
	checks := RunNetwork(context.Background(), oneAccountConfig(true, false), probe)

	auth, _ := findCheck(checks, "account/work/smtp/auth")
	if auth.Status != StatusFail {
		t.Fatalf("smtp auth = %s, want fail", auth.Status)
	}
	if !strings.Contains(auth.Detail, "authentication failed") {
		t.Errorf("detail %q does not classify the failure", auth.Detail)
	}
	if auth.Remedy == "" {
		t.Error("a failure must carry remediation guidance")
	}
}

// TestRunNetworkReportsSMTPTLSFailure covers the TLS sentinel.
func TestRunNetworkReportsSMTPTLSFailure(t *testing.T) {
	probe := &fakeProbe{smtpErr: fmt.Errorf("%w: handshake failed", smtpclient.ErrTLSFailed)}
	checks := RunNetwork(context.Background(), oneAccountConfig(true, false), probe)

	auth, _ := findCheck(checks, "account/work/smtp/auth")
	if auth.Status != StatusFail || !strings.Contains(auth.Detail, "TLS verification failed") {
		t.Fatalf("smtp auth = %+v, want a TLS failure", auth)
	}
}

// TestRunNetworkSTARTTLSDefersTLSVerification covers a STARTTLS endpoint:
// there is no plaintext TLS probe, so TLS is verified by the protocol check.
func TestRunNetworkSTARTTLSDefersTLSVerification(t *testing.T) {
	probe := &fakeProbe{}
	checks := RunNetwork(context.Background(), oneAccountConfig(false, true), probe)

	tlsCheck, _ := findCheck(checks, "account/work/imap/tls")
	if tlsCheck.Status != StatusSkip {
		t.Fatalf("tls = %s, want skip for STARTTLS", tlsCheck.Status)
	}
	if got := statusOf(t, checks, "account/work/imap/auth"); got != StatusOK {
		t.Errorf("auth = %s, want ok", got)
	}
}

// TestRunNetworkTimesOutWhenNameResolutionBlocks covers the bounded timeout.
func TestRunNetworkTimesOutWhenNameResolutionBlocks(t *testing.T) {
	probe := &fakeProbe{blockLookup: true}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	checks := RunNetwork(ctx, oneAccountConfig(false, false), probe)
	elapsed := time.Since(start)

	dns, _ := findCheck(checks, "account/work/imap/dns")
	if dns.Status != StatusFail || !strings.Contains(dns.Detail, "timed out") {
		t.Fatalf("dns = %+v, want a timeout failure", dns)
	}
	if elapsed > 5*time.Second {
		t.Errorf("RunNetwork took %s; the bound was not honoured", elapsed)
	}
}

// TestRunNetworkSkipsEverythingWhenCancelled covers cancellation before the
// phase starts.
func TestRunNetworkSkipsEverythingWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	checks := RunNetwork(ctx, oneAccountConfig(true, false), &fakeProbe{})
	if got := failures(checks); len(got) != 0 {
		t.Errorf("a cancelled run must not report failures: %+v", got)
	}
	for _, c := range checks {
		if c.Status != StatusSkip {
			t.Errorf("%s = %s, want skip", c.Name, c.Status)
		}
	}
}

// TestRunNetworkSkipsAccountsWithoutAnEndpoint covers a memory-backed
// account.
func TestRunNetworkSkipsAccountsWithoutAnEndpoint(t *testing.T) {
	cfg := config.Config{Accounts: []config.AccountConfig{{
		ID: "local", Email: "local@example.test", Backend: "memory",
		MailboxRoot: t.TempDir(), Permissions: []string{"read"},
	}}}
	checks := RunNetwork(context.Background(), cfg, &fakeProbe{})
	if len(checks) != 1 {
		t.Fatalf("got %d checks, want 1: %v", len(checks), names(checks))
	}
	if checks[0].Status != StatusSkip {
		t.Errorf("%s = %s, want skip", checks[0].Name, checks[0].Status)
	}
}

// TestRunNetworkReportsEveryResolvedAddressLimit covers address rendering.
func TestRunNetworkReportsEveryResolvedAddressLimit(t *testing.T) {
	probe := &fakeProbe{lookupAddrs: []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4", "192.0.2.5"}}
	checks := RunNetwork(context.Background(), oneAccountConfig(false, false), probe)
	dns, _ := findCheck(checks, "account/work/imap/dns")
	if !strings.Contains(dns.Detail, "and 1 more") {
		t.Errorf("detail %q does not summarise the address list", dns.Detail)
	}
}
