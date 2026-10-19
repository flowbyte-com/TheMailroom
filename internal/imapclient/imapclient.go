// Package imapclient provides a mailbox.Backend implementation that talks
// IMAP4rev1 to a remote server. It is provider-agnostic; the only
// provider-specific code lives in authentication (which is delegated to
// the standard SASL mechanisms) and in folder naming (which is whatever
// the server reports).
//
// Concurrency model: a single client per account is held under a
// mutex. Read operations are short-lived and do not block on the
// network for more than a few seconds in normal conditions. A
// connection that returns an error is closed and replaced on the next
// call.
//
// This file is the Phase 2 first slice: read-only IMAP. SMTP submission
// and destructive mailbox operations are deliberately not implemented
// here.
package imapclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// Backend is the read-only IMAP backend. It is safe for concurrent
// use by MCP tool handlers; underlying IMAP commands are serialised
// over a single client connection.
type Backend struct {
	cfg    config.IMAPConfig
	email  string
	limits config.LimitsConfig

	// dialTimeout caps the TCP dial. The IMAP protocol itself does
	// not have a meaningful per-command timeout; we rely on context
	// cancellation through the operation methods.
	dialTimeout time.Duration

	// tlsConfig, if non-nil, replaces the default *tls.Config. This
	// is intended for tests that need to trust a self-signed
	// certificate; production paths leave it nil and get a safe
	// default with ServerName set and MinVersion TLS 1.2.
	tlsConfig *tls.Config

	mu     sync.Mutex
	client *client.Client
}

// Options configure the IMAP backend at construction time.
type Options struct {
	// DialTimeout caps the TCP dial. Zero means 30s.
	DialTimeout time.Duration

	// TLSConfig, if non-nil, overrides the default *tls.Config used
	// for both implicit TLS and STARTTLS. Production code leaves
	// this nil; tests use it to inject InsecureSkipVerify against a
	// self-signed certificate.
	TLSConfig *tls.Config
}

// New returns a Backend that has not yet connected. The first
// operation triggers a connection.
func New(cfg config.AccountConfig, opts Options) (*Backend, error) {
	if cfg.IMAP.Host == "" {
		return nil, errors.New("imapclient: imap.host is required")
	}
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 30 * time.Second
	}
	return &Backend{
		cfg:         cfg.IMAP,
		email:       cfg.Email,
		limits:      cfg.Limits,
		dialTimeout: dialTimeout,
		tlsConfig:   opts.TLSConfig,
	}, nil
}

// Close releases the underlying connection. The Backend is unusable
// after Close; callers that share a long-lived Backend should ensure
// the connection state is left in a known good state.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil {
		return nil
	}
	err := b.client.Close()
	b.client = nil
	return err
}

// Mailboxes lists every mailbox returned by the server. It uses
// IMAP LIST with the wildcard pattern "*".
func (b *Backend) Mailboxes(ctx context.Context, accountID string) ([]mailbox.Mailbox, error) {
	var out []mailbox.Mailbox
	err := b.withClient(ctx, func(c *client.Client) error {
		ch := make(chan *imap.MailboxInfo, 64)
		done := make(chan error, 1)
		go func() { done <- c.List("", "*", ch) }()
		for m := range ch {
			out = append(out, mailboxFromInfo(m))
		}
		return <-done
	})
	if err != nil {
		return nil, mapErr(err)
	}
	sortMailboxes(out)
	return out, nil
}

// ListMessages returns summaries of messages in the given mailbox.
// The mailbox is SELECTed for the duration of the call. UIDVALIDITY
// is captured from the SELECT response.
func (b *Backend) ListMessages(ctx context.Context, accountID, mbx string, opts mailbox.ListOptions) ([]mailbox.MessageSummary, error) {
	if mbx == "" {
		return nil, mailbox.ErrMailboxNotFound
	}
	limit := opts.Limit
	if limit <= 0 || limit > b.limits.MaxBatchOperations {
		limit = b.limits.MaxBatchOperations
	}
	if limit <= 0 {
		limit = 50
	}
	var out []mailbox.MessageSummary
	err := b.withClient(ctx, func(c *client.Client) error {
		mboxStatus, err := c.Select(mbx, false)
		if err != nil {
			return err
		}
		if mboxStatus == nil {
			return errors.New("imap: nil mailbox status from SELECT")
		}
		if mboxStatus.Messages == 0 {
			return nil
		}
		// Use UidSearch to get all UIDs (we filter by date in the
		// envelope below, since UidSearch criteria on internal date
		// is widely supported but not universal).
		seqset, _ := imap.ParseSeqSet("1:*")
		items := []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchInternalDate,
			imap.FetchRFC822Size,
			imap.FetchFlags,
			imap.FetchUid,
		}
		ch := make(chan *imap.Message, 32)
		done := make(chan error, 1)
		go func() { done <- c.UidFetch(seqset, items, ch) }()
		for m := range ch {
			if m == nil {
				continue
			}
			if opts.Since != nil && m.InternalDate.Before(*opts.Since) {
				continue
			}
			if opts.Before != nil && !m.InternalDate.Before(*opts.Before) {
				continue
			}
			summary := mailbox.MessageSummary{
				UID:          m.Uid,
				Flags:        flagsToStrings(m.Flags),
				InternalDate: m.InternalDate,
				Size:         m.Size,
				From:         envelopeFrom(m.Envelope),
				To:           envelopeAddresses(m.Envelope, "To"),
				Subject:      envelopeSubject(m.Envelope),
			}
			out = append(out, summary)
		}
		if err := <-done; err != nil {
			return err
		}
		// Sort newest first, apply offset, truncate to limit.
		sortSummaries(out)
		if opts.Offset > 0 {
			if opts.Offset >= len(out) {
				out = nil
			} else {
				out = out[opts.Offset:]
			}
		}
		if len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// FetchMessage returns a single message body. The body is read with
// UID FETCH and the full RFC822 body is requested. UIDVALIDITY is
// captured from the SELECT response and exposed in the result.
func (b *Backend) FetchMessage(ctx context.Context, accountID, mbx string, uid uint32) (*mailbox.Message, error) {
	if mbx == "" {
		return nil, mailbox.ErrMailboxNotFound
	}
	if uid == 0 {
		return nil, errors.New("imap: uid must be non-zero")
	}
	var msg *mailbox.Message
	err := b.withClient(ctx, func(c *client.Client) error {
		mboxStatus, err := c.Select(mbx, false)
		if err != nil {
			return err
		}
		seqset := new(imap.SeqSet)
		seqset.AddNum(uid)
		items := []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchInternalDate,
			imap.FetchRFC822Size,
			imap.FetchFlags,
			imap.FetchUid,
			imap.FetchRFC822,
		}
		ch := make(chan *imap.Message, 1)
		done := make(chan error, 1)
		go func() { done <- c.UidFetch(seqset, items, ch) }()
		var raw *imap.Message
		for m := range ch {
			if m != nil && m.Uid == uid {
				raw = m
			}
		}
		if err := <-done; err != nil {
			return err
		}
		if raw == nil {
			return mailbox.ErrMessageNotFound
		}
		section := raw.GetBody(&imap.BodySectionName{BodyPartName: imap.BodyPartName{}})
		if section == nil {
			return errors.New("imap: server returned empty body")
		}
		data, err := io.ReadAll(section)
		if err != nil {
			return fmt.Errorf("imap: read body: %w", err)
		}
		// We deliberately do NOT parse the MIME here; the raw RFC822
		// is exposed and tools can do their own minimal extraction
		// later. Returning a fully parsed message is the next slice.
		headers, body := splitHeaders(data)
		msg = &mailbox.Message{
			UID:         raw.Uid,
			UIDValidity: mboxStatus.UidValidity,
			Headers:     headersFromEnvelope(raw.Envelope, headers),
			Text:        string(body),
			// HTML and Attachments require MIME parsing; deferred to
			// the MIME-integrated slice.
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return msg, nil
}

// SearchMessages runs a UID SEARCH against the server. Empty
// criteria fields are dropped; the limit is clamped to the
// account's MaxSearchResults.
func (b *Backend) SearchMessages(ctx context.Context, accountID, mbx string, q mailbox.SearchQuery) ([]mailbox.MessageSummary, error) {
	if mbx == "" {
		return nil, mailbox.ErrMailboxNotFound
	}
	limit := q.Limit
	if limit <= 0 || limit > b.limits.MaxSearchResults {
		limit = b.limits.MaxSearchResults
	}
	if limit <= 0 {
		limit = 50
	}
	crit := &imap.SearchCriteria{
		Body: []string{q.Body},
		Text: []string{q.Subject, q.From, q.To},
	}
	if q.Since != nil {
		crit.SentSince = *q.Since
	}
	if q.Before != nil {
		crit.SentBefore = *q.Before
	}
	// Empty Text slice elements confuse some servers; remove empties.
	crit.Text = compactStrings(crit.Text)
	crit.Body = compactStrings(crit.Body)
	var out []mailbox.MessageSummary
	err := b.withClient(ctx, func(c *client.Client) error {
		if _, err := c.Select(mbx, false); err != nil {
			return err
		}
		uids, err := c.UidSearch(crit)
		if err != nil {
			return err
		}
		if len(uids) == 0 {
			return nil
		}
		seqset := new(imap.SeqSet)
		seqset.AddNum(uids...)
		items := []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchInternalDate,
			imap.FetchRFC822Size,
			imap.FetchFlags,
			imap.FetchUid,
		}
		ch := make(chan *imap.Message, 32)
		done := make(chan error, 1)
		go func() { done <- c.UidFetch(seqset, items, ch) }()
		for m := range ch {
			if m == nil {
				continue
			}
			out = append(out, mailbox.MessageSummary{
				UID:          m.Uid,
				Flags:        flagsToStrings(m.Flags),
				InternalDate: m.InternalDate,
				Size:         m.Size,
				From:         envelopeFrom(m.Envelope),
				To:           envelopeAddresses(m.Envelope, "To"),
				Subject:      envelopeSubject(m.Envelope),
			})
		}
		if err := <-done; err != nil {
			return err
		}
		sortSummaries(out)
		if len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// --- internal helpers ---

// withClient acquires the per-account lock and provides a connected
// IMAP client to fn. If the client is not yet connected, has been
// logged out by the server, or the previous call returned an error,
// a fresh connection is established.
//
// fn MUST be synchronous and non-blocking; the IMAP client is not
// safe for concurrent use. The connection is closed on error.
func (b *Backend) withClient(ctx context.Context, fn func(*client.Client) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil || isLoggedOut(b.client) {
		c, err := b.dialAndLogin()
		if err != nil {
			return err
		}
		b.client = c
	}
	err := fn(b.client)
	if err != nil {
		c := b.client
		b.client = nil
		_ = c.Close()
	}
	return err
}

func (b *Backend) dialAndLogin() (*client.Client, error) {
	addr := net.JoinHostPort(b.cfg.Host, strconv.Itoa(b.cfg.Port))
	dialer := &net.Dialer{Timeout: b.dialTimeout}
	tlsCfg := b.tlsConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: b.cfg.Host, MinVersion: tls.VersionTLS12}
	} else if tlsCfg.ServerName == "" {
		// Always set ServerName from the configured host unless the
		// caller already provided one.
		tlsCfg.ServerName = b.cfg.Host
	}
	var c *client.Client
	var err error
	switch {
	case b.cfg.TLS:
		c, err = client.DialWithDialerTLS(dialer, addr, tlsCfg)
	case b.cfg.StartTLS:
		c, err = client.DialWithDialer(dialer, addr)
		if err == nil {
			if err = c.StartTLS(tlsCfg); err != nil {
				_ = c.Close()
				return nil, fmt.Errorf("imap: starttls: %w", err)
			}
		}
	default:
		// Both flags false: refuse to fall back to plaintext.
		_ = c
		err = errors.New("imap: tls or starttls must be enabled")
	}
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", redactAddr(addr), err)
	}
	pw, err := resolvePassword(b.cfg)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.Login(b.cfg.Username, pw); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: login: %w", err)
	}
	return c, nil
}

// resolvePassword returns the password from the configured source.
// It does not retain references to the returned string beyond the
// call; the caller is expected to use it promptly.
func resolvePassword(cfg config.IMAPConfig) (string, error) {
	if cfg.Password != "" {
		return cfg.Password, nil
	}
	if len(cfg.PasswordCommand) == 0 {
		return "", errors.New("imap: no password source configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.PasswordCommand[0], cfg.PasswordCommand[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("imap: password_command failed: %w (stderr: %s)", err, redactStderr(stderr.String()))
	}
	out := strings.TrimRight(stdout.String(), "\r\n")
	if out == "" {
		return "", errors.New("imap: password_command produced empty output")
	}
	return out, nil
}

// redactAddr strips the port from an address for error messages. The
// host may legitimately identify the target; the port does not add
// value to the agent and could be confused with a credential.
func redactAddr(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i] + ":***"
	}
	return addr
}

// redactStderr trims and escapes arbitrary stderr output.
func redactStderr(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) > max {
		s = s[:max] + "...[truncated]"
	}
	return strings.ReplaceAll(s, "\n", " ")
}

// isLoggedOut returns true if the client's logout channel has been
// closed (the IMAP server sent a BYE or the connection was dropped).
func isLoggedOut(c *client.Client) bool {
	select {
	case <-c.LoggedOut():
		return true
	default:
		return false
	}
}
