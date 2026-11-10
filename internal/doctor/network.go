package doctor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/imapclient"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/smtpclient"
)

// DefaultNetworkTimeout bounds the whole network phase.
const DefaultNetworkTimeout = 30 * time.Second

// perCheckTimeout bounds one network check inside the overall phase.
const perCheckTimeout = 15 * time.Second

// netProbe is the network surface the doctor needs.
//
// It exists so the checks can be driven deterministically in tests without
// a real network. The production implementation reuses the project's own
// IMAP and SMTP paths.
type netProbe interface {
	// LookupHost resolves a host name.
	LookupHost(ctx context.Context, host string) ([]string, error)
	// DialContext opens a TCP connection.
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSState opens a TCP connection and completes a verified TLS
	// handshake, returning the negotiated state.
	TLSState(ctx context.Context, addr, serverName string) (tls.ConnectionState, error)
	// IMAP connects, authenticates and performs a read-only mailbox
	// listing through the production IMAP backend.
	IMAP(ctx context.Context, acc config.AccountConfig) error
	// SMTPAuth connects, verifies TLS and authenticates through the
	// production SMTP client, without sending a message.
	SMTPAuth(ctx context.Context, acc config.AccountConfig) (smtpclient.ProbeResult, error)
}

// realProbe is the production network implementation.
type realProbe struct {
	dialTimeout      time.Duration
	handshakeTimeout time.Duration
}

func (p realProbe) LookupHost(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

func (p realProbe) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: p.dialTimeout}
	return dialer.DialContext(ctx, network, addr)
}

// TLSState performs a verified TLS handshake.
//
// The tls.Config mirrors the production clients exactly: ServerName is the
// configured host and the minimum version is TLS 1.2. Verification is never
// disabled, and there is no configuration that could disable it.
func (p realProbe) TLSState(ctx context.Context, addr, serverName string) (tls.ConnectionState, error) {
	raw, err := p.DialContext(ctx, "tcp", addr)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer raw.Close()
	conn := tls.Client(raw, &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12})
	hctx, cancel := context.WithTimeout(ctx, p.handshakeTimeout)
	defer cancel()
	if err := conn.HandshakeContext(hctx); err != nil {
		return tls.ConnectionState{}, err
	}
	return conn.ConnectionState(), nil
}

func (p realProbe) IMAP(ctx context.Context, acc config.AccountConfig) error {
	backend, err := imapclient.New(acc, imapclient.Options{DialTimeout: p.dialTimeout})
	if err != nil {
		return err
	}
	defer backend.Close()
	// Mailboxes authenticates and then issues a read-only LIST. It never
	// selects a mailbox for writing, fetches a body or mutates state.
	_, err = backend.Mailboxes(ctx, acc.ID)
	return err
}

func (p realProbe) SMTPAuth(ctx context.Context, acc config.AccountConfig) (smtpclient.ProbeResult, error) {
	return smtpclient.New().Probe(ctx, acc.SMTP)
}

// transportResult is the outcome of the transport checks for one endpoint.
type transportResult struct {
	checks []Check
	// usable reports that name resolution and TCP succeeded, so a
	// protocol-level check is worth attempting.
	usable bool
	// tlsHandled reports that TLS was verified here, or that it will be
	// verified inside the protocol check (STARTTLS).
	tlsHandled bool
}

// RunNetwork performs the opt-in network checks for every account that has
// a network endpoint.
func RunNetwork(ctx context.Context, cfg config.Config, probe netProbe) []Check {
	var checks []Check
	for _, acc := range cfg.Accounts {
		networked := false
		if acc.Backend == "imap" {
			networked = true
			prefix := "account/" + acc.ID + "/imap"
			tr := endpointChecks(ctx, probe, prefix, acc.IMAP.Host, acc.IMAP.Port, acc.IMAP.TLS)
			checks = append(checks, tr.checks...)
			checks = append(checks, imapAuthCheck(ctx, probe, acc, tr))
		}
		if hasPermission(acc.Permissions, "send") {
			networked = true
			prefix := "account/" + acc.ID + "/smtp"
			tr := endpointChecks(ctx, probe, prefix, acc.SMTP.Host, acc.SMTP.Port, acc.SMTP.TLS)
			checks = append(checks, tr.checks...)
			checks = append(checks, smtpAuthCheck(ctx, probe, acc, tr))
		}
		if !networked {
			checks = append(checks, Check{
				Name:   "account/" + acc.ID + "/network",
				Status: StatusSkip,
				Detail: "no network endpoint is configured for this account",
			})
		}
	}
	return checks
}

// endpointChecks resolves a host, opens a TCP connection and, for implicit
// TLS, verifies the certificate and host name.
//
// A failure short-circuits the dependent checks rather than repeating the
// same cause at every layer.
func endpointChecks(ctx context.Context, probe netProbe, prefix, host string, port int, implicitTLS bool) transportResult {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	res := transportResult{checks: make([]Check, 0, 3)}

	dnsName := prefix + "/dns"
	if c, done := precheck(ctx, dnsName); done {
		res.checks = append(res.checks, c,
			skipped(prefix+"/tcp", "not run: the network phase was cancelled or timed out"),
			skipped(prefix+"/tls", "not run: the network phase was cancelled or timed out"))
		return res
	}
	lookupCtx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	addrs, err := probe.LookupHost(lookupCtx, host)
	cancel()
	if err != nil {
		res.checks = append(res.checks, Check{
			Name:   dnsName,
			Status: StatusFail,
			Detail: fmt.Sprintf("%s did not resolve: %s", host, classifyErr(err)),
			Remedy: "check the host name and the resolver configuration",
		})
		res.checks = append(res.checks,
			skipped(prefix+"/tcp", "not attempted because name resolution failed"),
			skipped(prefix+"/tls", "not attempted because name resolution failed"))
		return res
	}
	res.checks = append(res.checks, Check{
		Name:   dnsName,
		Status: StatusOK,
		Detail: fmt.Sprintf("%s resolved to %s", host, summariseAddrs(addrs)),
	})

	tcpName := prefix + "/tcp"
	if c, done := precheck(ctx, tcpName); done {
		res.checks = append(res.checks, c, skipped(prefix+"/tls", "not run: the network phase was cancelled or timed out"))
		return res
	}
	dialCtx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	start := time.Now()
	conn, err := probe.DialContext(dialCtx, "tcp", addr)
	elapsed := time.Since(start)
	cancel()
	if err != nil {
		res.checks = append(res.checks, Check{
			Name:   tcpName,
			Status: StatusFail,
			Detail: fmt.Sprintf("%s did not accept a TCP connection: %s", addr, classifyErr(err)),
			Remedy: "check the port, the firewall and that the server is reachable",
		})
		res.checks = append(res.checks, skipped(prefix+"/tls", "not attempted because the TCP connection failed"))
		return res
	}
	_ = conn.Close()
	res.checks = append(res.checks, Check{
		Name:   tcpName,
		Status: StatusOK,
		Detail: fmt.Sprintf("connected to %s in %s", addr, elapsed.Round(time.Millisecond)),
	})
	res.usable = true

	tlsName := prefix + "/tls"
	if !implicitTLS {
		res.checks = append(res.checks, Check{
			Name:   tlsName,
			Status: StatusSkip,
			Detail: "STARTTLS: certificate verification happens during the authentication check",
		})
		// TLS is verified by the protocol check that follows.
		res.tlsHandled = true
		return res
	}
	if c, done := precheck(ctx, tlsName); done {
		res.checks = append(res.checks, c)
		return res
	}
	tlsCtx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	state, err := probe.TLSState(tlsCtx, addr, host)
	cancel()
	if err != nil {
		res.checks = append(res.checks, Check{
			Name:   tlsName,
			Status: StatusFail,
			Detail: fmt.Sprintf("TLS verification failed: %s", classifyErr(err)),
			Remedy: "the certificate must be valid for this host and signed by a trusted authority; verification is never disabled",
		})
		return res
	}
	res.checks = append(res.checks, Check{Name: tlsName, Status: StatusOK, Detail: tlsDetail(state)})
	res.tlsHandled = true
	return res
}

// imapAuthCheck authenticates and performs a read-only listing through the
// production IMAP backend.
func imapAuthCheck(ctx context.Context, probe netProbe, acc config.AccountConfig, tr transportResult) Check {
	name := "account/" + acc.ID + "/imap/auth"
	switch {
	case !tr.usable:
		return skipped(name, "not attempted because name resolution or the TCP connection failed")
	case !tr.tlsHandled:
		return skipped(name, "not attempted because TLS verification failed")
	}
	if c, done := precheck(ctx, name); done {
		return c
	}
	authCtx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	err := probe.IMAP(authCtx, acc)
	cancel()
	if err == nil {
		return Check{
			Name:   name,
			Status: StatusOK,
			Detail: "authenticated; read-only mailbox listing succeeded (no message was fetched)",
		}
	}
	detail, remedy := imapFailure(err)
	return Check{Name: name, Status: StatusFail, Detail: detail, Remedy: remedy}
}

// smtpAuthCheck connects, verifies TLS and authenticates without sending a
// message.
func smtpAuthCheck(ctx context.Context, probe netProbe, acc config.AccountConfig, tr transportResult) Check {
	name := "account/" + acc.ID + "/smtp/auth"
	switch {
	case !tr.usable:
		return skipped(name, "not attempted because name resolution or the TCP connection failed")
	case !tr.tlsHandled:
		return skipped(name, "not attempted because TLS verification failed")
	}
	if c, done := precheck(ctx, name); done {
		return c
	}
	authCtx, cancel := context.WithTimeout(ctx, perCheckTimeout)
	result, err := probe.SMTPAuth(authCtx, acc)
	cancel()
	if err == nil {
		return Check{
			Name:   name,
			Status: StatusOK,
			Detail: fmt.Sprintf("authenticated over %s; no message was sent", result.TLSVersion),
		}
	}
	detail, remedy := smtpFailure(err)
	return Check{Name: name, Status: StatusFail, Detail: detail, Remedy: remedy}
}

// imapFailure classifies an IMAP failure without echoing server text.
//
// The underlying login error can carry an arbitrary server response, so the
// doctor reports the stage rather than the message. The transport checks
// above have already established that name resolution, TCP and TLS work.
func imapFailure(err error) (detail, remedy string) {
	switch {
	case errors.Is(err, mailbox.ErrTimeout):
		return "IMAP check timed out",
			"raise -timeout, or review the account's imap operation deadlines"
	case errors.Is(err, mailbox.ErrCanceled):
		return "IMAP check was cancelled", ""
	}
	return "IMAP login did not complete; name resolution, TCP and TLS succeeded",
		"check imap.username and the configured credential source"
}

// smtpFailure classifies an SMTP failure from the package's typed errors.
func smtpFailure(err error) (detail, remedy string) {
	switch {
	case errors.Is(err, smtpclient.ErrTLSFailed):
		return "TLS verification failed",
			"the certificate must be valid for the configured host; verification is never disabled"
	case errors.Is(err, smtpclient.ErrAuthFailed):
		return "authentication failed or AUTH PLAIN is unsupported",
			"check smtp.username, the credential source and that the server advertises AUTH"
	case errors.Is(err, smtpclient.ErrConnectionFailed):
		return "the connection failed or the server rejected the session",
			"check smtp.host, smtp.port and the TLS mode"
	case errors.Is(err, smtpclient.ErrTimeout):
		return "the SMTP check timed out", "raise -timeout or check network reachability"
	case errors.Is(err, smtpclient.ErrInvalidRequest):
		return "the SMTP configuration is invalid", "correct the smtp settings and run doctor again"
	default:
		return "the SMTP check failed", "check smtp.host, smtp.port and the TLS mode"
	}
}

// skipped builds a skipped check.
func skipped(name, detail string) Check {
	return Check{Name: name, Status: StatusSkip, Detail: detail}
}

// precheck short-circuits a check when the network phase has been cancelled.
func precheck(ctx context.Context, name string) (Check, bool) {
	if err := ctx.Err(); err != nil {
		return skipped(name, "not run: the network phase was cancelled or timed out"), true
	}
	return Check{}, false
}

// tlsDetail renders a verified connection for an operator.
func tlsDetail(state tls.ConnectionState) string {
	detail := fmt.Sprintf("%s, cipher %s", tls.VersionName(state.Version), tls.CipherSuiteName(state.CipherSuite))
	if len(state.PeerCertificates) == 0 {
		return detail
	}
	leaf := state.PeerCertificates[0]
	detail += fmt.Sprintf(", certificate valid until %s", leaf.NotAfter.UTC().Format("2006-01-02"))
	if issuer := issuerName(leaf); issuer != "" {
		detail += ", issuer " + issuer
	}
	return detail
}

func issuerName(cert *x509.Certificate) string {
	if len(cert.Issuer.Organization) > 0 {
		return cert.Issuer.Organization[0]
	}
	return cert.Issuer.CommonName
}

// summariseAddrs renders resolved addresses compactly.
func summariseAddrs(addrs []string) string {
	if len(addrs) == 0 {
		return "no addresses"
	}
	const max = 4
	if len(addrs) > max {
		return fmt.Sprintf("%s and %d more", strings.Join(addrs[:max], ", "), len(addrs)-max)
	}
	return strings.Join(addrs, ", ")
}

// classifyErr turns a network error into a short, sanitised description.
//
// Certificate and resolver errors describe public facts: the host name, the
// certificate subject and the reason. No error text is copied verbatim, so
// a protocol response cannot reach the output.
func classifyErr(err error) string {
	if err == nil {
		return "ok"
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return "host not found"
		case dnsErr.IsTimeout:
			return "DNS lookup timed out"
		case dnsErr.IsTemporary:
			return "temporary DNS failure"
		default:
			return "DNS lookup failed"
		}
	}

	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return "certificate rejected: " + certReason(invalid.Reason)
	}
	var hostname x509.HostnameError
	if errors.As(err, &hostname) {
		return "certificate does not match the host name"
	}
	var authority x509.UnknownAuthorityError
	if errors.As(err, &authority) {
		return "certificate is not signed by a trusted authority"
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return "the peer did not start a TLS handshake"
	}

	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection reset by peer"
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return "host unreachable"
	}
	if errors.Is(err, syscall.ENETUNREACH) {
		return "network unreachable"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	return "failed"
}

func certReason(r x509.InvalidReason) string {
	switch r {
	case x509.Expired:
		return "certificate has expired or is not yet valid"
	case x509.NotAuthorizedToSign:
		return "certificate is not authorised to sign"
	case x509.CANotAuthorizedForThisName:
		return "certificate authority is not authorised for this name"
	case x509.TooManyIntermediates:
		return "too many intermediate certificates"
	case x509.IncompatibleUsage:
		return "certificate usage is not compatible"
	case x509.NameConstraintsWithoutSANs:
		return "name constraints require a subject alternative name"
	case x509.UnconstrainedName:
		return "name is not constrained"
	case x509.TooManyConstraints:
		return "too many name constraints"
	case x509.CANotAuthorizedForExtKeyUsage:
		return "certificate authority is not authorised for this extended key usage"
	default:
		return "certificate verification failed"
	}
}
