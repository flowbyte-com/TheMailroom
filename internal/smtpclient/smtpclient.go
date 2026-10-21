// Package smtpclient submits a fully prepared RFC 5322 message through one
// short-lived authenticated SMTP connection.
//
// The package deliberately exposes the low-level go-smtp client primitives
// (MAIL, RCPT, DATA) instead of using SendMail. That lets it abort before
// DATA if any recipient is rejected and distinguish a definite rejection from
// an ambiguous failure after message-data transmission has begun.
package smtpclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/recipients"
)

// Default stage timeouts. They are finite safety bounds, not user-facing
// configuration. Tests may override the fields on Client directly.
const (
	DefaultConnectTimeout      = 15 * time.Second
	DefaultTLSHandshakeTimeout = 15 * time.Second
	DefaultCommandTimeout      = 30 * time.Second
	DefaultDataTimeout         = 2 * time.Minute
	DefaultSubmissionTimeout   = 2 * time.Minute
	DefaultCredentialTimeout   = 10 * time.Second
)

// Sender is the small submission interface used by the tool layer. The
// mailbox backend is intentionally unrelated to this interface.
type Sender interface {
	Send(ctx context.Context, req Request) (Result, error)
}

// Request is one prepared submission. From and Recipients are SMTP envelope
// addresses, not header values. Message is the immutable serialized message
// produced by mailcompose.Compose.
type Request struct {
	AccountID  string
	Config     config.SMTPConfig
	From       string
	Recipients []string
	Message    []byte
	MessageID  string
}

// Result is returned only after the SMTP server has explicitly accepted the
// message after DATA completion. A successful submission still does not prove
// final mailbox delivery.
type Result struct {
	MessageID string
}

// Client implements Sender over one connection per Send call. It has no
// shared mutable state and is safe for concurrent use across accounts.
type Client struct {
	ConnectTimeout      time.Duration
	TLSHandshakeTimeout time.Duration
	CommandTimeout      time.Duration
	DataTimeout         time.Duration
	SubmissionTimeout   time.Duration
	CredentialTimeout   time.Duration

	// RootCAs is an optional test/embedding seam. Production leaves it nil
	// and uses the host root store. Certificate verification is never
	// disabled.
	RootCAs *x509.CertPool

	// DialContext is an optional test/embedding seam. Production leaves it
	// nil and uses a bounded net.Dialer.
	DialContext func(ctx context.Context, network, address string) (net.Conn, error)
}

// New returns a Client with bounded default timeouts.
func New() *Client {
	return &Client{
		ConnectTimeout:      DefaultConnectTimeout,
		TLSHandshakeTimeout: DefaultTLSHandshakeTimeout,
		CommandTimeout:      DefaultCommandTimeout,
		DataTimeout:         DefaultDataTimeout,
		SubmissionTimeout:   DefaultSubmissionTimeout,
		CredentialTimeout:   DefaultCredentialTimeout,
	}
}

func (c *Client) withDefaults() *Client {
	if c == nil {
		return New()
	}
	copy := *c
	if copy.ConnectTimeout <= 0 {
		copy.ConnectTimeout = DefaultConnectTimeout
	}
	if copy.TLSHandshakeTimeout <= 0 {
		copy.TLSHandshakeTimeout = DefaultTLSHandshakeTimeout
	}
	if copy.CommandTimeout <= 0 {
		copy.CommandTimeout = DefaultCommandTimeout
	}
	if copy.DataTimeout <= 0 {
		copy.DataTimeout = DefaultDataTimeout
	}
	if copy.SubmissionTimeout <= 0 {
		copy.SubmissionTimeout = DefaultSubmissionTimeout
	}
	if copy.CredentialTimeout <= 0 {
		copy.CredentialTimeout = DefaultCredentialTimeout
	}
	return &copy
}

// Send performs one complete SMTP transaction. It validates the request and
// resolves the credential before dialing. All failures return a zero Result.
//
// Confirmed acceptance after DATA is the only success path. A rejection before
// DATA returns a definite typed sentinel. Any failure after DATA begins but
// before a definitive final response returns *AmbiguousError with the original
// Message-ID. Send never retries.
func (c *Client) Send(ctx context.Context, req Request) (Result, error) {
	c = c.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, ErrTimeout
	}

	normalized, err := validateRequest(req)
	if err != nil {
		return Result{}, err
	}

	password, err := resolveCredential(ctx, normalized.Config, c.CredentialTimeout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Result{}, ErrTimeout
		}
		return Result{}, ErrAuthFailed
	}

	raw, err := c.dial(ctx, normalized.Config)
	if err != nil {
		return Result{}, err
	}
	defer raw.Close()

	// A context cancellation must interrupt blocked network operations. The
	// goroutine closes the raw connection, which also unblocks TLS reads and
	// writes. It stops before Send returns.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.SetDeadline(time.Now())
			_ = raw.Close()
		case <-done:
		}
	}()

	sess, err := c.connect(ctx, raw, normalized.Config)
	if err != nil {
		return Result{}, err
	}
	defer sess.close()

	// Establish verified TLS and EHLO before any authentication. Both
	// connect paths return with a completed TLS handshake and a cached EHLO.
	state, ok := sess.client.TLSConnectionState()
	if !ok || !state.HandshakeComplete || state.Version < tls.VersionTLS12 {
		return Result{}, fmt.Errorf("%w: TLS connection was not verified", ErrTLSFailed)
	}
	if ctx.Err() != nil {
		return Result{}, ErrTimeout
	}

	// Check the required mechanism before Auth. go-smtp's Auth would send
	// the PLAIN exchange without this explicit capability check.
	if !sess.client.SupportsAuth(sasl.Plain) {
		return Result{}, fmt.Errorf("%w: required authentication mechanism is not supported", ErrAuthFailed)
	}
	if err := sess.client.Auth(sasl.NewPlainClient("", normalized.Config.Username, password)); err != nil {
		if isTimeoutOrCancelled(err) || ctx.Err() != nil {
			return Result{}, ErrTimeout
		}
		return Result{}, fmt.Errorf("%w: authentication rejected", ErrAuthFailed)
	}

	if err := sess.client.Mail(normalized.From, nil); err != nil {
		if smtpErrorCode(err) != 0 {
			return Result{}, fmt.Errorf("%w: MAIL FROM rejected", ErrSenderRejected)
		}
		return Result{}, preDataFailure(ctx, err, "MAIL FROM")
	}

	for _, rcpt := range normalized.Recipients {
		if err := sess.client.Rcpt(rcpt, nil); err != nil {
			// A rejected recipient aborts the transaction before DATA.
			// RSET is best-effort cleanup; its failure cannot turn the
			// definite rejection into an ambiguous one.
			_ = sess.client.Reset()
			if smtpErrorCode(err) != 0 {
				return Result{}, fmt.Errorf("%w: RCPT TO rejected", ErrRecipientRejected)
			}
			return Result{}, preDataFailure(ctx, err, "RCPT TO")
		}
	}

	data, err := sess.client.Data()
	if err != nil {
		if smtpErrorCode(err) != 0 {
			return Result{}, fmt.Errorf("%w: DATA command rejected", ErrMessageRejected)
		}
		return Result{}, preDataFailure(ctx, err, "DATA")
	}

	// From this point onward, message data may be on the wire. Any failure
	// without a definitive final SMTP response is ambiguous.
	sess.setDataDeadline(c.DataTimeout)
	if err := writeAll(data, normalized.Message); err != nil {
		return Result{}, &AmbiguousError{MessageID: normalized.MessageID}
	}

	// CloseWithResponse writes the final dot and waits for the server's
	// final response. A timeout or lost connection here is ambiguous; an
	// explicit SMTP 4xx/5xx response is a definite rejection.
	sess.prepareFinalResponse(c.SubmissionTimeout)
	if _, err := data.CloseWithResponse(); err != nil {
		if smtpErrorCode(err) != 0 {
			return Result{}, fmt.Errorf("%w: final DATA response rejected", ErrMessageRejected)
		}
		return Result{}, &AmbiguousError{MessageID: normalized.MessageID}
	}

	return Result{MessageID: normalized.MessageID}, nil
}

func validateRequest(req Request) (Request, error) {
	cfg := req.Config
	if cfg.TLS == cfg.StartTLS {
		return Request{}, invalidRequest("exactly one of TLS or STARTTLS must be enabled")
	}
	if strings.TrimSpace(cfg.Host) == "" || recipients.HasControl(cfg.Host) ||
		strings.ContainsAny(cfg.Host, " /") {
		return Request{}, invalidRequest("invalid SMTP host")
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return Request{}, invalidRequest("invalid SMTP port")
	}
	if strings.TrimSpace(cfg.Username) == "" || recipients.HasControl(cfg.Username) {
		return Request{}, invalidRequest("invalid SMTP username")
	}
	if (cfg.Password == "") == (len(cfg.PasswordCommand) == 0) {
		return Request{}, invalidRequest("exactly one credential source is required")
	}

	from, err := recipients.Canonical(req.From)
	if err != nil || !isASCII(from) {
		return Request{}, invalidRequest("invalid or non-ASCII envelope sender")
	}
	normalizedRecipients := make([]string, 0, len(req.Recipients))
	seen := make(map[string]struct{}, len(req.Recipients))
	for _, raw := range req.Recipients {
		rcpt, err := recipients.Canonical(raw)
		if err != nil || !isASCII(rcpt) {
			return Request{}, invalidRequest("invalid or non-ASCII envelope recipient")
		}
		if _, duplicate := seen[rcpt]; duplicate {
			continue
		}
		seen[rcpt] = struct{}{}
		normalizedRecipients = append(normalizedRecipients, rcpt)
	}
	if len(normalizedRecipients) == 0 {
		return Request{}, invalidRequest("at least one envelope recipient is required")
	}
	if len(req.Message) == 0 {
		return Request{}, invalidRequest("message is empty")
	}
	if req.MessageID == "" || recipients.HasControl(req.MessageID) ||
		strings.ContainsAny(req.MessageID, " \t") {
		return Request{}, invalidRequest("invalid Message-ID")
	}

	req.From = from
	req.Recipients = normalizedRecipients
	return req, nil
}

func (c *Client) dial(ctx context.Context, cfg config.SMTPConfig) (net.Conn, error) {
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialContext := c.DialContext
	if dialContext == nil {
		dialer := &net.Dialer{Timeout: c.ConnectTimeout}
		dialContext = dialer.DialContext
	}
	dctx, cancel := context.WithTimeout(ctx, c.ConnectTimeout)
	defer cancel()
	conn, err := dialContext(dctx, "tcp", address)
	if err != nil {
		if isTimeoutOrCancelled(err) || dctx.Err() != nil || ctx.Err() != nil {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("%w: could not connect", ErrConnectionFailed)
	}
	return conn, nil
}

func (c *Client) connect(ctx context.Context, raw net.Conn, cfg config.SMTPConfig) (*session, error) {
	if cfg.TLS {
		return c.connectImplicit(ctx, raw, cfg)
	}
	return c.connectStartTLS(ctx, raw, cfg)
}

func (c *Client) connectImplicit(ctx context.Context, raw net.Conn, cfg config.SMTPConfig) (*session, error) {
	tlsConn := tls.Client(raw, c.tlsConfig(cfg.Host))
	hctx, cancel := context.WithTimeout(ctx, c.TLSHandshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hctx); err != nil {
		if ctx.Err() != nil {
			return nil, ErrTimeout
		}
		return nil, tlsFailure(err)
	}
	state := tlsConn.ConnectionState()
	if !state.HandshakeComplete || state.Version < tls.VersionTLS12 {
		return nil, fmt.Errorf("%w: TLS connection was not verified", ErrTLSFailed)
	}

	client := gosmtp.NewClient(tlsConn)
	client.CommandTimeout = c.CommandTimeout
	client.SubmissionTimeout = c.SubmissionTimeout
	if err := client.Hello("localhost"); err != nil {
		return nil, preDataFailure(ctx, err, "SMTP greeting")
	}
	return &session{client: client, base: tlsConn, raw: raw}, nil
}

func (c *Client) connectStartTLS(ctx context.Context, raw net.Conn, cfg config.SMTPConfig) (*session, error) {
	// NewClientStartTLS performs the greeting, EHLO and STARTTLS command but
	// does not itself complete the TLS handshake. capConn keeps its default
	// 5-minute command deadline bounded by our command timeout. The marker
	// lets us distinguish a greeting/EHLO failure from a STARTTLS rejection
	// when the library collapses both into one error. The explicit Hello below
	// drives the handshake under the TLS-handshake deadline.
	marker := &startTLSMarker{Conn: raw}
	capped := newCapConn(marker, c.CommandTimeout)
	client, err := gosmtp.NewClientStartTLS(capped, c.tlsConfig(cfg.Host))
	if err != nil {
		if isTimeoutOrCancelled(err) || ctx.Err() != nil {
			return nil, ErrTimeout
		}
		if marker.sawSTARTTLS.Load() || strings.Contains(strings.ToLower(err.Error()), "starttls") {
			return nil, fmt.Errorf("%w: mandatory STARTTLS was not negotiated", ErrTLSFailed)
		}
		return nil, fmt.Errorf("%w: SMTP greeting or EHLO failed", ErrConnectionFailed)
	}

	client.CommandTimeout = c.TLSHandshakeTimeout
	client.SubmissionTimeout = c.SubmissionTimeout
	capped.SetCap(c.TLSHandshakeTimeout)
	if err := client.Hello("localhost"); err != nil {
		state, ok := client.TLSConnectionState()
		if isTimeoutOrCancelled(err) || ctx.Err() != nil {
			return nil, ErrTimeout
		}
		if !ok || !state.HandshakeComplete {
			return nil, fmt.Errorf("%w: TLS handshake failed", ErrTLSFailed)
		}
		return nil, preDataFailure(ctx, err, "post-STARTTLS EHLO")
	}
	state, ok := client.TLSConnectionState()
	if !ok || !state.HandshakeComplete || state.Version < tls.VersionTLS12 {
		return nil, fmt.Errorf("%w: TLS connection was not verified", ErrTLSFailed)
	}
	client.CommandTimeout = c.CommandTimeout
	capped.SetCap(c.CommandTimeout)
	return &session{client: client, base: capped, raw: raw, capped: capped}, nil
}

func (c *Client) tlsConfig(host string) *tls.Config {
	return &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		RootCAs:    c.RootCAs,
	}
}

type session struct {
	client *gosmtp.Client
	base   net.Conn
	raw    net.Conn
	capped *capConn
}

func (s *session) close() {
	if s == nil {
		return
	}
	// Close the raw connection first. This is bounded and avoids a
	// close_notify write on a peer that has stopped reading. No SMTP QUIT is
	// required after the final DATA response.
	if s.raw != nil {
		_ = s.raw.Close()
	}
	if s.client != nil {
		_ = s.client.Close()
	}
}

func (s *session) setDataDeadline(timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	if s.capped != nil {
		s.capped.SetCap(timeout)
	}
	_ = s.base.SetWriteDeadline(time.Now().Add(timeout))
}

func (s *session) prepareFinalResponse(timeout time.Duration) {
	if timeout <= 0 {
		timeout = DefaultSubmissionTimeout
	}
	if s.capped != nil {
		s.capped.SetCap(timeout)
	}
	// CloseWithResponse writes the terminating dot before it sets its own
	// response deadline. Give that write a finite bound too, instead of
	// clearing the DATA write deadline and potentially blocking forever on a
	// peer that has stopped reading.
	_ = s.base.SetDeadline(time.Now().Add(timeout))
}

func preDataFailure(ctx context.Context, err error, phase string) error {
	if ctx.Err() != nil || isTimeoutOrCancelled(err) {
		return fmt.Errorf("%w: %s timed out or was cancelled", ErrTimeout, phase)
	}
	return fmt.Errorf("%w: %s failed", ErrConnectionFailed, phase)
}

func tlsFailure(err error) error {
	if isTimeoutOrCancelled(err) {
		return ErrTimeout
	}
	return fmt.Errorf("%w: TLS handshake or certificate verification failed", ErrTLSFailed)
}

func isTimeoutOrCancelled(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func smtpErrorCode(err error) int {
	var smtpErr *gosmtp.SMTPError
	if errors.As(err, &smtpErr) {
		return smtpErr.Code
	}
	return 0
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// startTLSMarker records whether the STARTTLS command reached the wire. It is
// used only to classify errors from go-smtp's combined greeting/EHLO/STARTTLS
// setup path.
type startTLSMarker struct {
	net.Conn
	sawSTARTTLS atomic.Bool
}

func (m *startTLSMarker) Write(p []byte) (int, error) {
	if !m.sawSTARTTLS.Load() && bytes.Contains(p, []byte("STARTTLS")) {
		m.sawSTARTTLS.Store(true)
	}
	return m.Conn.Write(p)
}

// capConn clamps every deadline set by the SMTP client to a current stage
// cap. go-smtp creates its own Client with long default deadlines; this
// wrapper lets the caller enforce known finite bounds without forking the
// dependency. It does not alter payload bytes.
type capConn struct {
	net.Conn
	mu  chan struct{}
	cap time.Duration
}

func newCapConn(conn net.Conn, maximum time.Duration) *capConn {
	c := &capConn{Conn: conn, mu: make(chan struct{}, 1), cap: maximum}
	c.mu <- struct{}{}
	return c
}

func (c *capConn) SetCap(maximum time.Duration) {
	<-c.mu
	c.cap = maximum
	c.mu <- struct{}{}
}

func (c *capConn) clamp(t time.Time) time.Time {
	<-c.mu
	maximum := c.cap
	c.mu <- struct{}{}
	if maximum <= 0 || t.IsZero() {
		return t
	}
	limit := time.Now().Add(maximum)
	if t.After(limit) {
		return limit
	}
	return t
}

func (c *capConn) SetDeadline(t time.Time) error {
	return c.Conn.SetDeadline(c.clamp(t))
}

func (c *capConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(c.clamp(t))
}

func (c *capConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(c.clamp(t))
}
