// Package imapclient provides a mailbox.Backend implementation that talks
// IMAP4rev1 to a remote server. It is provider-agnostic; the only
// provider-specific code lives in authentication (which is delegated to
// the standard SASL mechanisms) and in folder naming (which is whatever
// the server reports).
//
// Concurrency model: a single client per account is held under a
// mutex. Read operations are short-lived and do not block on the
// network for more than a few seconds in normal conditions. A
// connection that returns an error is closed and replaced on the
// next call.
//
// This file is the Phase 2.1 slice: read-only IMAP with strict
// UIDVALIDITY handling, BODY.PEEK fetches, and MIME parsing. SMTP
// submission, destructive mailbox operations and outbound
// attachments are deliberately not implemented here.
//
// # IMAP literal memory allocation: ENFORCED AT THE PROTOCOL LAYER
//
// The upstream go-imap v1.2.1 client allocates a buffer of the
// announced literal size and reads the literal body into it
// before the application sees the data
// (read.go::ReadLiteral:
//
//	b := make([]byte, n)
//	if _, err := io.ReadFull(r, b); err != nil { ... }
//
// An application-level cap (io.LimitReader + length check) is
// therefore only an application-side backstop: it runs AFTER
// the protocol layer has allocated the buffer, so it does not
// bound the allocation.
//
// This backend uses a narrow fork of go-imap
// (github.com/flowbyte-com/go-imap, module version
// v1.2.1-themailroom.4) that adds:
//
//   - client.DialOptions.MaxLiteralSize, which the backend sets
//     to the per-account MaxMessageBytes. The fork's
//     ReadLiteral checks the announced size against this cap
//     BEFORE the make([]byte, n) call, so an over-cap
//     announcement is rejected without allocating a buffer
//     proportional to the announced size.
//   - imap.ErrLiteralSizeExceeded, an exported sentinel so the
//     violation can be distinguished from an unrelated parse
//     failure.
//   - A reader-side fix that closes the connection when a
//     parse error desynchronises the response stream, and a
//     typed connection-closed error that preserves the
//     underlying cause.
//   - A constructor that applies the cap to the IMAP reader
//     at creation time, before the server greeting is parsed
//     and before the background reader goroutine starts. The
//     cap is therefore in effect for the very first byte
//     read from the connection, with no pre-configuration
//     window and no data race between the constructor and
//     the reader goroutine.
//
// The cap is set on the IMAP *Reader*, not on the transport, so
// it applies identically to the implicit-TLS and STARTTLS
// paths: in both cases the Reader observes the decrypted IMAP
// byte stream.
//
// The application-level cap is retained as a defence in depth
// and as the check that guarantees a literal is never silently
// truncated.
//
// See docs/THREAT_MODEL.md T4 and docs/PHASE2_DESIGN.md §8/§10
// for the full threat analysis, the fork's change set, and the
// route back to upstream.
//
// # Remaining limitations
//
// Fixing the literal-allocation vulnerability does NOT make the
// application production-hardened. Other work remains, tracked in
// docs/PHASE2.2.md:
//
//   - The IMAP search correctness defects (docs/PHASE2_DESIGN.md
//     §7): Flagged and Unseen are not honoured by the IMAP
//     backend, and Subject+From+To are OR'd by the IMAP TEXT
//     keyword.
//   - There is no attachment retrieval tool.
//   - The fork is a temporary measure; it must be upstreamed or
//     replaced.
//
// See docs/PHASE2_DESIGN.md §8/§10 for the full threat analysis
// and tracked work.
package imapclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/client"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
)

// Backend is the read-only IMAP backend. It is safe for concurrent
// use by MCP tool handlers; underlying IMAP commands are serialised
// over a single client connection.
type Backend struct {
	cfg    config.AccountConfig
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

	// commandTimeout caps the time an IMAP command may take.
	// Zero means the go-imap client default (no timeout).
	commandTimeout time.Duration

	// selectedValidity is the UIDVALIDITY of the most recent
	// SELECT/EXAMINE call. It is captured and surfaced on every
	// read so callers can compare against their own cached value.
	// A zero value indicates the mailbox is not currently
	// selected.
	mu               sync.Mutex
	client           *client.Client
	selectedMailbox  string
	selectedValidity uint32
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

	// CommandTimeout caps the time an IMAP command may take. Zero
	// means the client default (no timeout). A bounded timeout
	// prevents a misbehaving server from blocking the server
	// process forever.
	CommandTimeout time.Duration
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
	// Apply defaults so callers see the effective limits at
	// construction time.
	norm := cfg.WithDefaults()
	return &Backend{
		cfg:            norm,
		email:          norm.Email,
		limits:         norm.Limits,
		dialTimeout:    dialTimeout,
		tlsConfig:      opts.TLSConfig,
		commandTimeout: opts.CommandTimeout,
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
	b.selectedMailbox = ""
	b.selectedValidity = 0
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
// The mailbox is EXAMINEd (read-only SELECT) for the duration of
// the call so the server-side state cannot accept STORE or APPEND
// through the read connection. The returned ListResult.UIDValidity
// is the value observed during the same EXAMINE that produced the
// UIDs in ListResult.Messages; callers MUST use this UIDVALIDITY
// for any subsequent FetchMessage call rather than a value obtained
// from a later selection.
func (b *Backend) ListMessages(ctx context.Context, accountID, mbx string, opts mailbox.ListOptions) (mailbox.ListResult, error) {
	if mbx == "" {
		return mailbox.ListResult{}, mailbox.ErrMailboxNotFound
	}
	limit := opts.Limit
	if limit <= 0 || limit > b.limits.MaxBatchOperations {
		limit = b.limits.MaxBatchOperations
	}
	if limit <= 0 {
		limit = 50
	}
	res := mailbox.ListResult{}
	err := b.withClient(ctx, func(c *client.Client) error {
		mboxStatus, err := c.Select(mbx, true) // true => EXAMINE (read-only)
		if err != nil {
			return err
		}
		if mboxStatus == nil {
			return errors.New("imap: nil mailbox status from EXAMINE")
		}
		if mboxStatus.UidValidity == 0 {
			return mailbox.ErrUIDMissing
		}
		// Bind the EXAMINE result to the UIDs we are about to
		// fetch. UIDVALIDITY is captured here and returned in
		// the same ListResult, so a later selection cannot be
		// combined with these UIDs.
		if err := b.setSelected(mbx, mboxStatus.UidValidity); err != nil {
			return err
		}
		res.UIDValidity = mboxStatus.UidValidity
		if mboxStatus.Messages == 0 {
			return nil
		}
		// Use UidFetch on the full sequence set; the body items
		// below are non-mutating envelope / size / flag lookups.
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
		var out []mailbox.MessageSummary
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
		res.Messages = out
		return nil
	})
	if err != nil {
		return mailbox.ListResult{}, mapErr(err)
	}
	return res, nil
}

// FetchMessage returns a single message body. The mailbox is
// EXAMINEd (read-only) for the call; the body is fetched with
// UID FETCH BODY.PEEK[] so the server does not implicitly set the
// \Seen flag.
//
// uidValidity is required to be non-zero and MUST match the
// server's current UIDVALIDITY for the selected mailbox; a
// mismatch returns ErrUIDMismatch without acting. A server that
// reports a zero UIDVALIDITY returns ErrUIDMissing.
func (b *Backend) FetchMessage(ctx context.Context, accountID, mbx string, uid uint32, uidValidity uint32) (*mailbox.Message, error) {
	var msg *mailbox.Message
	err := b.fetchRawMessage(ctx, accountID, mbx, uid, uidValidity, func(raw *rawMessage) error {
		decoded, perr := mimeparse.Parse(raw.Body, b.mimeLimits())
		if perr != nil {
			return perr
		}
		msg = &mailbox.Message{
			UID:         raw.UID,
			UIDValidity: raw.UIDValidity,
			Flags:       raw.Flags,
			Headers:     decoded.Headers,
			Text:        decoded.Text,
			HTML:        decoded.HTML,
			Attachments: convertAttachments(decoded.Attachments),
			Warnings:    decoded.Warnings,
			Truncated:   decoded.Truncated,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return msg, nil
}

// FetchAttachment returns one selected attachment from a message.
// It reuses the same read-only full-message fetch as FetchMessage
// (EXAMINE, live UIDVALIDITY validation, BODY.PEEK[]) and then asks
// mimeparse to validate the complete message and capture only the
// requested decoded part. No part path is ever interpolated into an
// IMAP command.
func (b *Backend) FetchAttachment(ctx context.Context, accountID, mbx string, uid uint32, uidValidity uint32, part string) (*mailbox.AttachmentData, error) {
	if _, err := mailbox.ParsePartPath(part, b.limits.MaxMIMENesting, b.limits.MaxMIMEParts); err != nil {
		return nil, err
	}
	var data *mailbox.AttachmentData
	err := b.fetchRawMessage(ctx, accountID, mbx, uid, uidValidity, func(raw *rawMessage) error {
		ac, perr := mimeparse.ExtractAttachment(raw.Body, part, b.mimeLimits())
		if perr != nil {
			return perr
		}
		data = &mailbox.AttachmentData{
			UID:         raw.UID,
			UIDValidity: raw.UIDValidity,
			Attachment:  convertAttachment(ac.Attachment, ac.Index),
			Content:     ac.Content,
			Warnings:    ac.Warnings,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

// mimeLimits returns the parser limits shared by FetchMessage and
// FetchAttachment.
func (b *Backend) mimeLimits() mimeparse.Limits {
	return mimeparse.Limits{
		MaxMessageBytes:         b.limits.MaxMessageBytes,
		MaxAttachmentBytes:      b.limits.MaxAttachmentBytes,
		MaxTotalAttachmentBytes: b.limits.MaxTotalAttachmentBytes,
		MaxMIMENesting:          b.limits.MaxMIMENesting,
		MaxMIMEParts:            b.limits.MaxMIMEParts,
		MaxAttachments:          b.limits.MaxAttachments,
	}
}

// rawMessage is the result of the shared read-only full-message
// fetch. Body is the complete RFC 5322 message, bounded by the
// configured message and protocol literal caps.
type rawMessage struct {
	UID         uint32
	UIDValidity uint32
	Flags       []string
	Body        []byte
}

// fetchRawMessage performs the existing read-only EXAMINE +
// BODY.PEEK[] flow and invokes inspect with the raw message while
// the backend lock is held. Keeping this in one helper ensures
// FetchMessage and FetchAttachment share the same UIDVALIDITY,
// literal-size, connection-cleanup and panic handling, including the
// existing behaviour of closing the connection when MIME decoding
// fails.
func (b *Backend) fetchRawMessage(ctx context.Context, accountID, mbx string, uid uint32, uidValidity uint32, inspect func(*rawMessage) error) error {
	if mbx == "" {
		return mailbox.ErrMailboxNotFound
	}
	if uid == 0 {
		return errors.New("imap: uid must be non-zero")
	}
	if uidValidity == 0 {
		return mailbox.ErrUIDMissing
	}
	err := b.withClient(ctx, func(c *client.Client) error {
		mboxStatus, err := c.Select(mbx, true)
		if err != nil {
			return err
		}
		if mboxStatus == nil {
			return errors.New("imap: nil mailbox status from EXAMINE")
		}
		if mboxStatus.UidValidity == 0 {
			return mailbox.ErrUIDMissing
		}
		if mboxStatus.UidValidity != uidValidity {
			return fmt.Errorf("%w: have %d, want %d", mailbox.ErrUIDMismatch, mboxStatus.UidValidity, uidValidity)
		}
		if err := b.setSelected(mbx, mboxStatus.UidValidity); err != nil {
			return err
		}
		seqset := new(imap.SeqSet)
		seqset.AddNum(uid)
		// BODY.PEEK[] never sets \Seen. We request a fixed set of
		// additional items for envelope and metadata; UID is added
		// implicitly by UidFetch.
		bodyItem := imap.FetchItem("BODY.PEEK[]")
		items := []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchInternalDate,
			imap.FetchRFC822Size,
			imap.FetchFlags,
			imap.FetchUid,
			bodyItem,
		}
		ch := make(chan *imap.Message, 1)
		done := make(chan error, 1)
		go func() {
			// Recover from any panic in c.UidFetch and
			// surface it as a generic internal error. We
			// must NOT disguise a panic as a size-limit
			// violation: the cause is unknown and the
			// tool layer should treat it as an
			// internal error, not a memory-safety
			// control that succeeded. The application
			// cap on the literal body (below) remains
			// the only size-related signal we produce.
			defer func() {
				if r := recover(); r != nil {
					done <- fmt.Errorf("imap: fetch panic: %v", r)
				}
			}()
			done <- c.UidFetch(seqset, items, ch)
		}()
		// Drain the message channel first so the goroutine
		// can complete; then check the error. The order
		// matters because a parse error (e.g. an over-cap
		// literal) closes the channel without sending any
		// messages; if we checked `raw == nil` first we
		// would silently return ErrMessageNotFound.
		var raw *imap.Message
		for m := range ch {
			if m != nil && m.Uid == uid {
				raw = m
			}
		}
		fetchErr := <-done
		if fetchErr != nil {
			return fetchErr
		}
		if raw == nil {
			return mailbox.ErrMessageNotFound
		}
		// Build a BodySectionName matching what the server sent
		// back; for BODY.PEEK[] the response key is "BODY[]"
		// (Peek is elided on the response) with no part specifier.
		section := &imap.BodySectionName{Peek: true}
		literal := raw.GetBody(section)
		if literal == nil {
			return errors.New("imap: server returned empty body")
		}
		// Cap the body to MaxMessageBytes so a hostile server
		// cannot stream gigabytes through. We use the
		// LimitReader + 1 trick: read up to cap+1 bytes; if
		// the read returns more than cap, the literal was
		// larger than the cap and we return ErrMessageTooLarge.
		// A literal of exactly cap bytes is valid.
		//
		// The literal was already read (and bounded) by the
		// go-imap reader's MaxLiteralSize cap, which the
		// backend sets to MaxMessageBytes at dial time. The
		// check below is a defence in depth: it guarantees
		// that a literal is never silently truncated, even if
		// the protocol-level cap were bypassed.
		//
		// See the package doc for the pre-allocation
		// guarantee and PHASE2_DESIGN.md §8/§10 for the full
		// threat analysis.
		cap := int64(b.limits.MaxMessageBytes)
		limited := io.LimitReader(literal, cap+1)
		rawBytes, rerr := io.ReadAll(limited)
		if rerr != nil {
			return fmt.Errorf("imap: read body: %w", rerr)
		}
		if int64(len(rawBytes)) > cap {
			return mailbox.ErrMessageTooLarge
		}
		out := &rawMessage{
			UID:         raw.Uid,
			UIDValidity: mboxStatus.UidValidity,
			Flags:       flagsToStrings(raw.Flags),
			Body:        rawBytes,
		}
		if inspect != nil {
			return inspect(out)
		}
		return nil
	})
	if err != nil {
		return mapErr(err)
	}
	return nil
}

// SearchMessages runs a UID SEARCH against the server. Empty
// criteria fields are dropped; the limit is clamped to the
// account's MaxSearchResults. The mailbox is EXAMINEd so the
// search cannot mutate state.
func (b *Backend) SearchMessages(ctx context.Context, accountID, mbx string, q mailbox.SearchQuery) (mailbox.ListResult, error) {
	if mbx == "" {
		return mailbox.ListResult{}, mailbox.ErrMailboxNotFound
	}
	limit := q.Limit
	if limit <= 0 || limit > b.limits.MaxSearchResults {
		limit = b.limits.MaxSearchResults
	}
	if limit <= 0 {
		limit = 50
	}
	res := mailbox.ListResult{}
	err := b.withClient(ctx, func(c *client.Client) error {
		mboxStatus, err := c.Select(mbx, true)
		if err != nil {
			return err
		}
		if mboxStatus == nil {
			return errors.New("imap: nil mailbox status from EXAMINE")
		}
		if mboxStatus.UidValidity == 0 {
			return mailbox.ErrUIDMissing
		}
		// Bind UIDVALIDITY to the search results: the same
		// EXAMINE that produced the UIDs is the source of
		// the validity value returned in res.UIDValidity.
		if err := b.setSelected(mbx, mboxStatus.UidValidity); err != nil {
			return err
		}
		res.UIDValidity = mboxStatus.UidValidity
		// Run one search. Subject / From use the
		// corresponding IMAP SEARCH header keys, To is an
		// explicit OR over To and Cc, and the body, flag and
		// date criteria are ANDed into the same command (see
		// internal/imapclient/search.go). The dates are
		// truncated to the day boundary because an IMAP
		// search date carries no time (see
		// internal/searchspec).
		uids, err := searchUIDs(c, q)
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
		var out []mailbox.MessageSummary
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
		res.Messages = out
		return nil
	})
	if err != nil {
		return mailbox.ListResult{}, mapErr(err)
	}
	return res, nil
}

// SelectedUIDValidity returns the UIDVALIDITY of the most recent
// successful EXAMINE. The value is cached across the lifetime of
// the connection; reconnecting resets it to zero until the next
// EXAMINE. Tools use this to obtain the live UIDVALIDITY for a
// mailbox without performing an extra round-trip.
func (b *Backend) SelectedUIDValidity() uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.selectedValidity
}

// MailboxUIDValidity EXAMINEs the named mailbox and returns its
// current UIDVALIDITY. The result is also cached as the selected
// mailbox so subsequent FetchMessage calls do not need a second
// round-trip. An ErrUIDMissing is returned if the server reports
// zero.
//
// IMPORTANT: this is a stand-alone EXAMINE. The returned
// UIDVALIDITY is paired with the EXAMINE only, not with any
// UID-listing operation. Callers that want a UIDVALIDITY bound
// to specific UIDs must use ListMessages or SearchMessages,
// which return a ListResult that pairs the value at the source.
// Do not use MailboxUIDValidity to validate a UID obtained
// from a different selection.
func (b *Backend) MailboxUIDValidity(ctx context.Context, accountID, mbx string) (uint32, error) {
	if mbx == "" {
		return 0, mailbox.ErrMailboxNotFound
	}
	var uv uint32
	err := b.withClient(ctx, func(c *client.Client) error {
		mboxStatus, err := c.Select(mbx, true)
		if err != nil {
			return err
		}
		if mboxStatus == nil {
			return errors.New("imap: nil mailbox status from EXAMINE")
		}
		if mboxStatus.UidValidity == 0 {
			return mailbox.ErrUIDMissing
		}
		if err := b.setSelected(mbx, mboxStatus.UidValidity); err != nil {
			return err
		}
		uv = mboxStatus.UidValidity
		return nil
	})
	if err != nil {
		return 0, mapErr(err)
	}
	return uv, nil
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
		c, err := b.dialAndLogin(ctx)
		if err != nil {
			return err
		}
		b.client = c
		// Force a re-EXAMINE on the next call by clearing the
		// selected mailbox: a new connection cannot inherit the
		// server-side state.
		b.selectedMailbox = ""
		b.selectedValidity = 0
	}
	err := fn(b.client)
	if err != nil {
		// On error, close the client so the next call reconnects.
		// We never silently retry a mutating operation; the
		// backend is read-only and the only operations are
		// EXAMINE, FETCH, SEARCH and LIST.
		c := b.client
		b.client = nil
		b.selectedMailbox = ""
		b.selectedValidity = 0
		_ = c.Close()
	}
	return err
}

// dialAndLogin establishes one IMAP connection and authenticates it.
//
// ctx bounds the credential command: it is the caller's operation context
// and is cancelled with it. The TLS handshake deliberately keeps using
// context.Background(), as before, so that the existing implicit-TLS and
// STARTTLS connection behaviour is unchanged.
func (b *Backend) dialAndLogin(ctx context.Context) (*client.Client, error) {
	addr := net.JoinHostPort(b.cfg.IMAP.Host, strconv.Itoa(b.cfg.IMAP.Port))
	dialer := &net.Dialer{Timeout: b.dialTimeout}
	tlsCfg := b.tlsConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: b.cfg.IMAP.Host, MinVersion: tls.VersionTLS12}
	} else if tlsCfg.ServerName == "" {
		// Always set ServerName from the configured host unless the
		// caller already provided one.
		tlsCfg.ServerName = b.cfg.IMAP.Host
	}
	raw, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("imap: dial %s: %w", redactAddr(addr), err)
	}
	var conn net.Conn = raw
	// opts carries the protocol-level IMAP literal-size cap. It
	// is applied at the *imap.Reader level, i.e. before the
	// reader allocates a buffer for an announced literal. This
	// is the pre-allocation control required by
	// docs/PHASE2_DESIGN.md §10.
	//
	// The cap is set on the Reader, not on the transport, so it
	// applies identically to the implicit-TLS and STARTTLS
	// paths: in both cases the Reader sees the *decrypted*
	// IMAP byte stream.
	opts := client.DialOptions{}
	if b.limits.MaxMessageBytes > 0 {
		if b.limits.MaxMessageBytes > math.MaxUint32 {
			return nil, fmt.Errorf("imap: max_message_bytes %d exceeds the protocol literal limit", b.limits.MaxMessageBytes)
		}
		opts.MaxLiteralSize = client.MaxLiteralSize(b.limits.MaxMessageBytes)
	}
	switch {
	case b.cfg.IMAP.TLS:
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.HandshakeContext(context.Background()); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("imap: tls handshake: %w", err)
		}
		conn = tlsConn
	case b.cfg.IMAP.StartTLS:
		c, err := client.NewWithOptions(conn, opts)
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		if b.commandTimeout > 0 {
			c.Timeout = b.commandTimeout
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("imap: starttls: %w", err)
		}
		pw, err := resolvePassword(ctx, b.cfg.IMAP)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
		if err := c.Login(b.cfg.IMAP.Username, pw); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("imap: login: %w", err)
		}
		return c, nil
	default:
		_ = raw.Close()
		return nil, errors.New("imap: tls or starttls must be enabled")
	}
	c, err := client.NewWithOptions(conn, opts)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	if b.commandTimeout > 0 {
		c.Timeout = b.commandTimeout
	}
	pw, err := resolvePassword(ctx, b.cfg.IMAP)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.Login(b.cfg.IMAP.Username, pw); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("imap: login: %w", err)
	}
	return c, nil
}

// setSelected records the mailbox we have just EXAMINEd and its
// UIDVALIDITY for later reporting. Called under the per-account
// mutex.
func (b *Backend) setSelected(name string, validity uint32) error {
	if validity == 0 {
		return mailbox.ErrUIDMissing
	}
	b.selectedMailbox = name
	b.selectedValidity = validity
	return nil
}

// limitReader is no longer used; the body cap is enforced with
// io.LimitReader(r, cap+1) and a length check on the returned
// slice. Keeping the type commented for reference.
//
// (the previous limitReader struct and Read method are removed
// to avoid the boundary ambiguity that the LimitReader + 1
// trick resolves.)

// redactAddr strips the port from an address for error messages. The
// host may legitimately identify the target; the port does not add
// value to the agent and could be confused with a credential.
func redactAddr(addr string) string {
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i] + ":***"
	}
	return addr
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
