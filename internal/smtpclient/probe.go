package smtpclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/emersion/go-sasl"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// ProbeResult reports what a non-sending SMTP probe established.
type ProbeResult struct {
	// TLSVersion is the negotiated TLS version, for example "TLS 1.3".
	TLSVersion string
	// Authenticated reports that the server accepted the credential. It is
	// always true on a successful probe.
	Authenticated bool
}

// Probe performs the connection, TLS and authentication stages of a
// submission without sending a message.
//
// It dials, performs implicit TLS or mandatory STARTTLS, requires a
// verified TLS 1.2 or newer connection, checks that the server advertises
// AUTH PLAIN, and authenticates with the configured credential. It never
// issues MAIL FROM, RCPT TO or DATA, so it cannot send a message, cannot
// mutate a mailbox and leaves no envelope state on the server.
//
// The credential comes from the same bounded source Send uses: a
// password_command is executed directly, with bounded output and a bounded
// timeout, and its output is never included in an error.
//
// The returned error is one of the package's typed sentinels. Server
// response text, commands and credentials are never included.
func (c *Client) Probe(ctx context.Context, cfg config.SMTPConfig) (ProbeResult, error) {
	c = c.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ProbeResult{}, ErrTimeout
	}
	if err := validateConfig(cfg); err != nil {
		return ProbeResult{}, err
	}

	password, err := resolveCredential(ctx, cfg, c.CredentialTimeout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ProbeResult{}, ErrTimeout
		}
		return ProbeResult{}, ErrAuthFailed
	}

	raw, err := c.dial(ctx, cfg)
	if err != nil {
		return ProbeResult{}, err
	}
	defer raw.Close()

	// A context cancellation must interrupt blocked network operations. The
	// goroutine closes the raw connection, which also unblocks TLS reads and
	// writes. It stops before Probe returns.
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

	sess, err := c.connect(ctx, raw, cfg)
	if err != nil {
		return ProbeResult{}, err
	}
	defer sess.close()

	// Both connect paths return with a completed, verified TLS handshake.
	state, ok := sess.client.TLSConnectionState()
	if !ok || !state.HandshakeComplete || state.Version < tls.VersionTLS12 {
		return ProbeResult{}, fmt.Errorf("%w: TLS connection was not verified", ErrTLSFailed)
	}
	if ctx.Err() != nil {
		return ProbeResult{}, ErrTimeout
	}

	// Check the required mechanism before Auth. go-smtp's Auth would send
	// the PLAIN exchange without this explicit capability check.
	if !sess.client.SupportsAuth(sasl.Plain) {
		return ProbeResult{}, fmt.Errorf("%w: required authentication mechanism is not supported", ErrAuthFailed)
	}
	if err := sess.client.Auth(sasl.NewPlainClient("", cfg.Username, password)); err != nil {
		if isTimeoutOrCancelled(err) || ctx.Err() != nil {
			return ProbeResult{}, ErrTimeout
		}
		return ProbeResult{}, fmt.Errorf("%w: authentication rejected", ErrAuthFailed)
	}

	// The probe ends here, before any envelope command. QUIT is polite and
	// best-effort: the transaction never advances past authentication, so a
	// failed QUIT cannot change the outcome.
	_ = sess.client.Quit()

	return ProbeResult{TLSVersion: tls.VersionName(state.Version), Authenticated: true}, nil
}
