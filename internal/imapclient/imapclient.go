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
// v1.2.1-themailroom.6) that adds:
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
//   - client.DialOptions.MaxResponseFields, MaxAtomSize,
//     MaxQuotedStringSize, MaxResponseBytes and MaxListDepth, which
//     bound the memory a single response can make the reader
//     allocate. The reader counts every byte it consumes, so the
//     limits apply to the whole response and not only to its
//     literals. imap.ErrResponseTooLarge is the exported sentinel
//     for a violation; it is a fatal parse error, so the connection
//     is torn down and the cause is preserved.
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
// # IMAP list workload: ENFORCED AT THE APPLICATION LEVEL
//
// Listing is ordered by internal date, which is unrelated to UID
// order, so an exact page can only be produced by examining every
// message in the mailbox. The per-account
// limits.max_list_scan_messages (default 10 000, hard maximum
// 1 000 000) bounds that work.
//
// The bound is enforced twice:
//
//   - Before the fetch, from the mailbox size the server reports in
//     the EXAMINE response. An oversized mailbox is refused with
//     mailbox.ErrListTooLarge before any FETCH is sent.
//   - During the fetch, from the messages the FETCH handler actually
//     delivers. On delivery of message budget+1 the client records
//     the overrun and aborts the transfer by closing the underlying
//     TCP socket, then drains the response channel and awaits the
//     completion of the in-flight FETCH.
//
// The second check does not depend on the server's own count, so a
// server that under-reports its mailbox size is caught while the
// listing runs. The abort is a socket close because the go-imap
// client has no way to cancel an in-flight command, and issuing
// another command (CLOSE, LOGOUT) while a command is active would
// write to the connection concurrently with the command's own write.
//
// The guarantee is precise: the limit bounds the messages delivered
// by the FETCH handler and processed at the application level, and
// detection necessarily observes message budget+1. It is not a
// strict bound on all protocol work. It does not bound the raw
// responses the server sends, the responses the fork discards
// internally (a FETCH response without a UID is dropped by the
// response handler and is never counted), the total bytes
// transferred or buffered, the allocation for a single oversized
// envelope, or the wall-clock duration of the call. The residual
// decoder and byte-budget risks are tracked in
// docs/THREAT_MODEL.md §3.1. Explicit IMAP command timeouts are a
// separate control.
//
// The search path uses the same budget. Its criteria are evaluated by
// the server, so the result set can be arbitrarily large. The
// application issues the UID SEARCH itself, with a response handler
// of its own, and counts the UIDs the server returns; a search that
// matches more messages than the budget allows is refused before any
// FETCH is sent. The UID FETCH response is bounded by a second count,
// and the transfer is aborted by closing the underlying socket when the
// server delivers more messages than the budget allows.
//
// The go-imap reader materialises the whole SEARCH response before any
// response handler runs, so the application cannot bound that
// allocation; it bounds everything downstream of it. Closing that gap
// needs a decoder-level limit in the fork. See docs/THREAT_MODEL.md
// §3.1 for the measurement and the recommended dependency work.
//
// # IMAP aggregate work: ENFORCED AT THE TRANSPORT LAYER
//
// The decoder's limits and the scan budget are both per operation, but
// the number of responses a server sends on one connection is not
// bounded by either. A server that sends an unlimited sequence of
// individually valid responses consumes an unlimited number of input
// bytes, parsing work, heap allocations and CPU time in the client's
// reader goroutine.
//
// The server data budget (budget.go) closes that gap. It wraps the
// socket below the TLS layer, so it accounts every byte the server
// sends, in both TLS modes and after a STARTTLS upgrade, and it
// refuses to read past the allowance, so an over-budget response is
// never parsed and never allocated.
//
// The accounting is coordinated, not approximate:
//
//   - One mutex governs the byte counter, the window in force and the
//     violation record, so an allowance transition is a coordinated
//     transition and not a set of independent atomic loads and stores.
//   - The counter is monotonic and is never reset. A window measures
//     the consumption since it opened, so a transition cannot lose
//     accounting.
//   - The read is capped under the mutex, performed outside it - so a
//     transition is never blocked behind a pending read - and then
//     committed and re-validated under the mutex again. That commit is
//     the linearization point: a read's bytes are committed against one
//     window before they are returned to the decoder, and a read that
//     no longer fits the allowance in force returns no payload bytes.
//   - The bytes consumed from the socket and the bytes handed to the
//     decoder are accounted separately, because a refused read consumes
//     bytes without delivering them.
//
// The boundary is exact in both directions. An exhausted allowance
// performs a one-byte probe instead of refusing, so a server that sent
// exactly the permitted amount and is then silent is not mistaken for
// an excess, while the first byte that arrives is the violation.
//
// The violation belongs to the connection: it survives every window
// transition and is cleared only when the connection is discarded, so
// an idle violation is reported by the next operation rather than
// silently erased. The client, the socket that backs it, the budget
// and the selection metadata are always dropped together.
//
// See docs/ARCHITECTURE.md and docs/THREAT_MODEL.md for the full
// analysis, the allowances and the residual risks.
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
	mu     sync.Mutex
	client *client.Client
	// budget bounds the inbound protocol bytes the current connection
	// may consume. It is the connection's serverDataBudget; it is nil
	// when there is no connection.
	budget           *serverDataBudget
	selectedMailbox  string
	selectedValidity uint32

	// conn is the raw TCP socket that backs client, so that an
	// in-flight command can be aborted by closing the socket instead
	// of issuing another IMAP command.
	//
	// It is set and cleared under mu, in lockstep with client: it is
	// non-nil exactly when client is non-nil, and it always refers to
	// the socket of the current client, never to a previous one. The
	// go-imap client has no way to cancel an in-flight command, and
	// issuing another command (CLOSE, LOGOUT) while a command is
	// active would write to the connection concurrently with the
	// command's own write. Closing the socket aborts the transfer
	// without writing anything, so it cannot race the command
	// writer.
	conn net.Conn
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
		b.releaseConn()
		return nil
	}
	err := b.client.Close()
	b.client = nil
	b.budget = nil
	b.selectedMailbox = ""
	b.selectedValidity = 0
	// The IMAP session is over, so the socket is released as well. It
	// is closed without writing anything, and no command is in flight
	// at this point.
	b.releaseConn()
	return err
}

// Mailboxes lists every mailbox returned by the server. It uses
// IMAP LIST with the wildcard pattern "*".
func (b *Backend) Mailboxes(ctx context.Context, accountID string) ([]mailbox.Mailbox, error) {
	var out []mailbox.Mailbox
	err := b.withClient(ctx, opMailboxList, func(c *client.Client) error {
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
	// The scan budget bounds how many messages one list or search
	// operation may fetch and process. It does not bound the work the
	// server does to evaluate the search criteria.
	budget := b.scanBudget()
	res := mailbox.ListResult{}
	err := b.withClient(ctx, opList, func(c *client.Client) error {
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
		// Listing is ordered by internal date, which is unrelated to
		// UID order, so an exact page can only be produced by examining
		// every message in the mailbox. The scan budget bounds that
		// work. The server reports the message count in the EXAMINE
		// response, so an over-budget mailbox is refused before any
		// FETCH is sent.
		if mboxStatus.Messages > uint32(budget) {
			return listTooLargeError(mbx, mboxStatus.Messages, budget)
		}
		// The number of summaries that can appear in the result is the
		// offset plus the limit. At most that many are retained, so
		// the working set does not grow with the size of the mailbox.
		// The arithmetic saturates rather than overflowing, so a very
		// large offset cannot wrap.
		keep := limit
		if keep > budget {
			keep = budget
		}
		if opts.Offset > 0 {
			switch {
			case opts.Offset > budget:
				keep = budget
			case limit > budget-opts.Offset:
				keep = budget
			default:
				keep = opts.Offset + limit
			}
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
		selector := &summarySelector{limit: keep}
		// delivered counts every message the FETCH handler hands to
		// this process, independently of the date filters and of the
		// selector's retention. It is the quantity the scan budget
		// bounds: the budget limits how many messages one call may
		// examine, and the filters run afterwards, so they cannot make
		// an oversized mailbox cheap.
		delivered := 0
		overrun := false
		for m := range ch {
			if m == nil {
				continue
			}
			if overrun {
				// Keep draining. The fork's response handler blocks
				// on this channel, and a blocked handler stops the
				// reader, so the command would never complete and
				// the goroutine would leak. The channel is closed by
				// the fork, never by us.
				continue
			}
			delivered++
			if delivered > budget {
				// The server delivered more messages than the budget
				// allows. Record the overrun and abort the transfer.
				//
				// The abort is the close of the underlying socket: it
				// stops the server from sending the rest of the
				// response and it writes nothing to the connection,
				// so it cannot race the command writer the way an
				// interleaved CLOSE or LOGOUT would. The client is
				// unusable afterwards, but withClient discards it when
				// this call returns the error below.
				overrun = true
				b.abortConn()
				continue
			}
			if opts.Since != nil && m.InternalDate.Before(*opts.Since) {
				continue
			}
			if opts.Before != nil && !m.InternalDate.Before(*opts.Before) {
				continue
			}
			selector.Add(mailbox.MessageSummary{
				UID:          m.Uid,
				Flags:        flagsToStrings(m.Flags),
				InternalDate: m.InternalDate,
				Size:         m.Size,
				From:         envelopeFrom(m.Envelope),
				To:           envelopeAddresses(m.Envelope, "To"),
				Subject:      envelopeSubject(m.Envelope),
			})
		}
		// The fetch must complete before this function returns. The
		// go-imap client does not serialise writes to the connection, so
		// closing the connection while a command is still in flight is a
		// data race; waiting here is what orders the fetch's write
		// before the close that withClient performs on the error path.
		//
		// After an abort the fetch fails with a connection error. That
		// is the expected outcome, and the budget error below is the
		// one that is reported.
		fetchErr := <-done
		if overrun {
			return listScanBudgetExceededError(mbx, budget)
		}
		if fetchErr != nil {
			return fetchErr
		}
		out := selector.Sorted()
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
	err := b.withClient(ctx, opMessage, func(c *client.Client) error {
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
	// The scan budget bounds how many messages one search may fetch and
	// process. It does not bound the work the server does to evaluate
	// the search criteria: that computation is server-side and the
	// client cannot observe or limit it.
	budget := b.scanBudget()
	res := mailbox.ListResult{}
	err := b.withClient(ctx, opSearch, func(c *client.Client) error {
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
		//
		// The search is bounded by the scan budget: a result
		// that matches more messages than the budget allows is
		// refused before any FETCH is sent, so the server is
		// never asked for the envelopes of every match.
		uids, err := searchUIDs(c, q, budget)
		if err != nil {
			return err
		}
		if len(uids) == 0 {
			return nil
		}
		// The sequence set holds at most the budget UIDs, so its
		// size and the size of the FETCH command line it produces
		// are bounded.
		seqset := new(imap.SeqSet)
		seqset.AddNum(uids...)
		items := []imap.FetchItem{
			imap.FetchEnvelope,
			imap.FetchInternalDate,
			imap.FetchRFC822Size,
			imap.FetchFlags,
			imap.FetchUid,
		}
		// The number of summaries that can appear in the result is the
		// limit. At most that many are retained, so the working set
		// does not grow with the size of the result set.
		keep := limit
		if keep > budget {
			keep = budget
		}
		ch := make(chan *imap.Message, 32)
		done := make(chan error, 1)
		go func() { done <- c.UidFetch(seqset, items, ch) }()
		// The summaries are collected in the bounded top-K selector,
		// which keeps the same page a full sort of every summary
		// would produce.
		selector := &summarySelector{limit: keep}
		// delivered counts every message the FETCH handler hands to
		// this process. It is the second line of defence: the search
		// already bounded the number of requested UIDs, but a server
		// that sends more messages than it was asked for is caught
		// here.
		delivered := 0
		overrun := false
		for m := range ch {
			if m == nil {
				continue
			}
			if overrun {
				// Keep draining. The fork's response handler blocks
				// on this channel, and a blocked handler stops the
				// reader, so the command would never complete and
				// the goroutine would leak. The channel is closed by
				// the fork, never by us.
				continue
			}
			delivered++
			if delivered > budget {
				// The server delivered more messages than the budget
				// allows. Record the overrun and abort the transfer.
				//
				// The abort is the close of the underlying socket: it
				// stops the server from sending the rest of the
				// response and it writes nothing to the connection,
				// so it cannot race the command writer the way an
				// interleaved CLOSE or LOGOUT would. The client is
				// unusable afterwards, but withClient discards it when
				// this call returns the error below.
				overrun = true
				b.abortConn()
				continue
			}
			selector.Add(mailbox.MessageSummary{
				UID:          m.Uid,
				Flags:        flagsToStrings(m.Flags),
				InternalDate: m.InternalDate,
				Size:         m.Size,
				From:         envelopeFrom(m.Envelope),
				To:           envelopeAddresses(m.Envelope, "To"),
				Subject:      envelopeSubject(m.Envelope),
			})
		}
		// The fetch must complete before this function returns. The
		// go-imap client does not serialise writes to the connection, so
		// closing the connection while a command is in flight is a
		// data race; waiting here is what orders the fetch's write
		// before the close that withClient performs on the error path.
		//
		// After an abort the fetch fails with a connection error. That
		// is the expected outcome, and the budget error below is the
		// one that is reported.
		fetchErr := <-done
		if overrun {
			return searchTooLargeError(budget)
		}
		if fetchErr != nil {
			return fetchErr
		}
		out := selector.Sorted()
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
	err := b.withClient(ctx, opPlain, func(c *client.Client) error {
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
// The client, the socket that backs it and the budget that accounts
// its inbound bytes are owned together: they are set and cleared in
// lockstep under the per-account mutex, so the backend never holds a
// client without a socket, never holds a socket without its client,
// never holds a budget that belongs to a connection that no longer
// exists, and never holds the socket of a previous connection. A
// session that the server has ended is invalidated — client, socket,
// budget and selection metadata — before the replacement connection is
// attempted, so a failed reconnect leaves no obsolete state behind.
//
// The allowance in force is armed per operation. Between operations
// the connection is idle and the server is expected to send very
// little, so the allowance drops back to the small idle one and a
// server that floods an idle connection loses it quickly.
//
// fn MUST be synchronous and non-blocking; the IMAP client is not
// safe for concurrent use. The connection is closed on error.
func (b *Backend) withClient(ctx context.Context, kind opKind, fn func(*client.Client) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil || isLoggedOut(b.client) {
		// The previous session is over, so its client, socket, budget
		// and selection metadata are discarded together, before the
		// replacement connection is attempted.
		//
		// Doing this before the dial matters: if the replacement
		// connection fails, the backend must not keep an obsolete
		// client without an owned socket, nor a UIDVALIDITY that
		// was observed on a session that no longer exists.
		//
		// The budget's violation record is captured before the budget
		// is dropped: it is the cause of the session's end, and an
		// idle violation must be reported rather than erased silently.
		cause := b.discardLocked()
		if cause != nil {
			// The connection was invalidated by an aggregate-limit
			// violation while it was idle. The violation is reported
			// by the operation that recognises it; the following call
			// reconnects.
			return idleServerDataBudgetError()
		}
		c, conn, budget, err := b.dialAndLogin(ctx)
		if err != nil {
			// A server that floods before the client has issued a
			// command exhausts the allowance while the connection is
			// being established. That is the same condition as a flood
			// during an operation, and it is reported the same way.
			if budget != nil && budget.violationErr() != nil {
				return serverDataBudgetError(kind)
			}
			return err
		}
		b.client = c
		// The socket is adopted together with the client, so the
		// abort path can never close a replacement connection.
		b.conn = conn
		b.budget = budget
		// Force a re-EXAMINE on the next call by clearing the
		// selected mailbox: a new connection cannot inherit the
		// server-side state.
		b.selectedMailbox = ""
		b.selectedValidity = 0
	}
	// Arm the server data budget for this operation, so that the
	// inbound protocol bytes of its commands and of the unsolicited
	// responses that arrive while they run are bounded.
	b.budget.setLimit(b.dataAllowance(kind))

	err := fn(b.client)

	// A budget violation is reported as itself, not as the connection
	// failure it causes: the connection died because the server sent
	// too much data, and the caller can act on that.
	//
	// The check happens even when the callback appeared to succeed: a
	// violation that invalidated the data the callback consumed must
	// not be reported as a complete result.
	if b.budget != nil && b.budget.violationErr() != nil {
		err = serverDataBudgetError(kind)
	}
	if err != nil {
		// On error, close the client so the next call reconnects.
		// We never silently retry a mutating operation; the
		// backend is read-only and the only operations are
		// EXAMINE, FETCH, SEARCH and LIST.
		//
		// The client and its socket are dropped together, so no stale
		// socket reference survives the failure and the next call
		// dials a fresh connection. c.Close() writes a CLOSE command,
		// which is safe here because the caller has already awaited
		// the completion of the command that failed.
		c := b.client
		if c != nil {
			_ = c.Close()
		}
		b.discardLocked()
		return err
	}
	// Between operations the connection is idle and the server is
	// expected to send very little, so the allowance drops back to
	// the small idle one. A server that floods an idle connection
	// therefore loses it quickly.
	//
	// Arming a window does not erase the violation record: the record
	// belongs to the connection, and it is cleared only when the
	// connection is discarded.
	b.budget.setLimit(idleDataAllowance)
	return nil
}

// discardLocked drops the current connection.
//
// The client, the socket that backs it, the budget that accounts its
// inbound bytes and the selection metadata that belonged to the
// session are dropped together, so the backend never holds a client
// without a socket, a socket without its client, or a budget that
// belongs to a connection that no longer exists.
//
// It reports the budget violation that ended the session, if there was
// one. The caller decides whether to report it: the operation that
// ended the session has already reported it, and only a violation that
// nobody has reported yet - an idle one - must still be surfaced.
//
// It writes nothing to the connection: closing the socket is enough to
// end a session whose command has already completed.
//
// Called under the per-account mutex.
func (b *Backend) discardLocked() error {
	var cause error
	if b.budget != nil {
		cause = b.budget.violationErr()
		b.budget.clearViolation()
	}
	b.releaseConn()
	b.client = nil
	b.budget = nil
	b.selectedMailbox = ""
	b.selectedValidity = 0
	return cause
}

// releaseConn closes the socket of the current client and forgets it.
//
// Closing a socket writes nothing to the connection, so it is safe
// even when the connection is already broken. It is the same operation
// the abort path uses, minus the semantics: the abort path keeps the
// client in place until the operation returns, whereas releaseConn
// accompanies the client being discarded.
//
// Called under the per-account mutex.
func (b *Backend) releaseConn() {
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
}

// decoderLimits returns the reader's resource limits for this account.
//
// The go-imap fork (v1.2.1-themailroom.6) bounds the memory a single
// response can make its reader allocate: a maximum number of fields, a
// maximum atom and quoted-string size, a maximum number of bytes per
// response, and a maximum list nesting depth. They are configured
// through client.DialOptions and applied by client.NewWithOptions
// before the reader goroutine starts, so they are in effect for the
// very first byte read from the connection.
//
// The limits are derived from the account's own configuration, so that
// they bound exactly the responses the account legitimately expects:
//
//   - The field limit is the scan budget plus the response's name, so
//     that a search result of budget+1 UIDs is still decodable and the
//     application can produce its own typed refusal rather than a
//     decoder error. A floor keeps an ordinary FETCH envelope usable
//     whatever the scan budget is.
//
//   - The response-byte limit is the greater of the SEARCH response's
//     own requirement and the largest legitimate literal body plus the
//     protocol around it.
//
// The decoder's limits are a second line of defence. The
// application-level bounds - the UID collector and the FETCH delivery
// count - remain the primary ones, and a decoder limit never replaces
// them.
//
// Every value is computed in int64 and clamped to the range the reader
// can represent. A limit is never clamped to zero, because zero means
// "no limit" and would disable the enforcement entirely.
func (b *Backend) decoderLimits(scanBudget int) client.DialOptions {
	opts := client.DialOptions{}

	// The field limit: the scan budget plus the response's name, so
	// that a result of budget+1 UIDs is still decodable.
	fields := int64(scanBudget) + 2
	if fields < decoderFieldLimitFloor {
		fields = decoderFieldLimitFloor
	}
	opts.MaxResponseFields = clampDecoderLimit(fields)

	opts.MaxAtomSize = decoderAtomSizeLimit
	opts.MaxQuotedStringSize = decoderQuotedStringSizeLimit
	opts.MaxListDepth = decoderListDepthLimit

	// The largest literal one response can carry: the body plus the
	// protocol around it must fit in the response budget, and the
	// budget is a uint32. A configured maximum message size above
	// that is reduced to what one response can carry; the value is
	// never reduced to zero.
	literal := int64(b.limits.MaxMessageBytes)
	if literal < 0 {
		literal = 0
	}
	if maxCarriable := int64(math.MaxUint32) - decoderLiteralOverhead; literal > maxCarriable {
		literal = maxCarriable
	}
	if literal > 0 {
		opts.MaxLiteralSize = client.MaxLiteralSize(literal)
	}

	// The SEARCH response's own requirement: budget+1 UIDs, each at
	// most a separator and ten decimal digits, plus the protocol
	// around the response.
	search := decoderBytesPerUID*(int64(scanBudget)+1) + decoderSearchOverhead

	// The literal's own requirement: the body plus the protocol around
	// it.
	body := literal + decoderLiteralOverhead

	bytes := search
	if body > bytes {
		bytes = body
	}
	opts.MaxResponseBytes = clampDecoderLimit(bytes)

	return opts
}

// clampDecoderLimit converts a computed limit to the uint32 the reader
// uses.
//
// The value is never clamped to zero: zero means "no limit" in the
// reader, so clamping to it would silently disable the enforcement.
func clampDecoderLimit(v int64) uint32 {
	if v <= 0 {
		return 1
	}
	if v > int64(math.MaxUint32) {
		return math.MaxUint32
	}
	return uint32(v)
}

// The decoder's limits and the allowances they are derived from.
const (
	// decoderFieldLimitFloor is the smallest number of fields one
	// response may hold. An ordinary FETCH envelope holds about thirty
	// fields - the sequence number, the name, the item list and every
	// item inside it, including the nested envelope and address lists
	// - so the floor keeps ordinary responses usable whatever the scan
	// budget is.
	decoderFieldLimitFloor = 64

	// decoderAtomSizeLimit is the largest atom, in bytes, the reader
	// accepts. It is generously above any legitimate atom: a UID, a
	// mailbox name, a flag or a date.
	decoderAtomSizeLimit = 64 << 10

	// decoderQuotedStringSizeLimit is the largest quoted string, in
	// bytes, the reader accepts. It is generously above any legitimate
	// quoted string, such as a subject or a response-code argument.
	decoderQuotedStringSizeLimit = 64 << 10

	// decoderListDepthLimit is the deepest list nesting the reader
	// accepts. It is generously above the deepest legitimate nesting,
	// which is an envelope's address list.
	decoderListDepthLimit = 64

	// decoderSearchOverhead is the allowance for the protocol around a
	// SEARCH response: the tag, the response name, the CRLF, the
	// tagged completion and the separators.
	decoderSearchOverhead = 1 << 10

	// decoderLiteralOverhead is the allowance for the protocol around a
	// literal: the FETCH items, the literal's length declaration, the
	// CRLF and the envelope's own fields.
	decoderLiteralOverhead = 64 << 10

	// decoderBytesPerUID is the number of bytes one UID occupies in a
	// SEARCH response: a separator and up to ten decimal digits.
	decoderBytesPerUID = 11
)

// scanBudget returns the effective scan budget: the number of messages
// one list or search operation may fetch and process.
//
// The same limit bounds both workloads because it bounds the same
// thing: the number of messages whose envelopes one operation may
// fetch, parse and process. Zero means the configured limit is unset,
// in which case the default applies.
func (b *Backend) scanBudget() int {
	budget := b.limits.MaxListScanMessages
	if budget <= 0 {
		budget = config.DefaultMaxListScanMessages
	}
	return budget
}

// abortConn closes the raw socket that backs the current client.
//
// It aborts an in-flight command without issuing another IMAP command:
// closing a socket does not write to the connection, so it cannot race
// the command writer the way an interleaved CLOSE or LOGOUT would.
//
// The socket is not replaced here. The client stays in place until the
// operation that called abortConn returns an error, at which point
// withClient discards the client and its socket together and the next
// operation dials a fresh connection.
//
// Called under the per-account mutex.
func (b *Backend) abortConn() {
	if b.conn != nil {
		_ = b.conn.Close()
	}
}

// dialAndLogin establishes one IMAP connection and authenticates it.
//
// It returns the client together with the raw TCP socket that backs it.
// The socket is what the abort path closes to stop an in-flight command
// without issuing another IMAP command. It is the same socket in both
// TLS modes: with implicit TLS the client speaks over a tls.Conn that
// wraps it, and with STARTTLS the connection is upgraded in place.
//
// ctx bounds the credential command: it is the caller's operation context
// and is cancelled with it. The TLS handshake deliberately keeps using
// context.Background(), as before, so that the existing implicit-TLS and
// STARTTLS connection behaviour is unchanged.
//
// Every failure path releases the socket before returning, so a failed
// connection setup leaks no file descriptor.
func (b *Backend) dialAndLogin(ctx context.Context) (*client.Client, net.Conn, *serverDataBudget, error) {
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
		return nil, nil, nil, fmt.Errorf("imap: dial %s: %w", redactAddr(addr), err)
	}
	// The socket is released on every failure path below. It is only
	// handed back to the caller together with a client, and the caller
	// then owns it for the lifetime of that client.
	handedOff := false
	defer func() {
		if !handedOff {
			_ = raw.Close()
		}
	}()
	// The budget wraps the socket below the TLS layer, so it accounts
	// every byte the server sends, in both TLS modes and after a
	// STARTTLS upgrade. The greeting and the login are read through
	// it, under the idle allowance.
	budget := &serverDataBudget{Conn: raw}
	budget.setLimit(idleDataAllowance)
	var conn net.Conn = budget
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
	// opts carries the protocol-level limits: the literal-size cap and
	// the decoder's per-response limits. They are applied at the
	// *imap.Reader level, before the reader allocates anything, and
	// before the reader goroutine starts.
	//
	// The literal-size cap is the pre-allocation control required by
	// docs/PHASE2_DESIGN.md §10: the reader checks the announced size
	// BEFORE the make([]byte, n) call, so an over-cap announcement is
	// rejected without allocating a buffer proportional to it.
	//
	// The decoder's limits bound the memory one response can make the
	// reader allocate. They are derived from the account's own
	// configuration, so that they bound the responses the account
	// legitimately expects.
	if b.limits.MaxMessageBytes > math.MaxUint32 {
		return nil, nil, budget, fmt.Errorf("imap: max_message_bytes %d exceeds the protocol literal limit", b.limits.MaxMessageBytes)
	}
	opts := b.decoderLimits(b.scanBudget())
	switch {
	case b.cfg.IMAP.TLS:
		tlsConn := tls.Client(conn, tlsCfg)
		if err := tlsConn.HandshakeContext(context.Background()); err != nil {
			return nil, nil, budget, fmt.Errorf("imap: tls handshake: %w", err)
		}
		conn = tlsConn
	case b.cfg.IMAP.StartTLS:
		c, err := client.NewWithOptions(conn, opts)
		if err != nil {
			return nil, nil, budget, err
		}
		if b.commandTimeout > 0 {
			c.Timeout = b.commandTimeout
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			_ = c.Close()
			return nil, nil, budget, fmt.Errorf("imap: starttls: %w", err)
		}
		pw, err := resolvePassword(ctx, b.cfg.IMAP)
		if err != nil {
			_ = c.Close()
			return nil, nil, budget, err
		}
		if err := c.Login(b.cfg.IMAP.Username, pw); err != nil {
			_ = c.Close()
			return nil, nil, budget, fmt.Errorf("imap: login: %w", err)
		}
		handedOff = true
		return c, raw, budget, nil
	default:
		return nil, nil, budget, errors.New("imap: tls or starttls must be enabled")
	}
	c, err := client.NewWithOptions(conn, opts)
	if err != nil {
		return nil, nil, budget, err
	}
	if b.commandTimeout > 0 {
		c.Timeout = b.commandTimeout
	}
	pw, err := resolvePassword(ctx, b.cfg.IMAP)
	if err != nil {
		_ = c.Close()
		return nil, nil, budget, err
	}
	if err := c.Login(b.cfg.IMAP.Username, pw); err != nil {
		_ = c.Close()
		return nil, nil, budget, fmt.Errorf("imap: login: %w", err)
	}
	handedOff = true
	return c, raw, budget, nil
}

// listTooLargeError reports that a mailbox holds more messages than the
// list scan budget allows, as reported by the server in the EXAMINE
// response. The message names the counts and the recourse, so a caller
// can act on it.
//
// The recourse is to raise the limit. It is not to narrow the query: the
// date filters run in the client after the preflight check and after the
// fetch, so they cannot make an oversized mailbox listable.
func listTooLargeError(mbx string, messages uint32, budget int) error {
	return fmt.Errorf("%w: mailbox %q holds %d messages, more than the list scan budget of %d; "+
		"raise limits.max_list_scan_messages (maximum %d) to list it",
		mailbox.ErrListTooLarge, mbx, messages, budget, config.MaxListScanMessagesLimit)
}

// listScanBudgetExceededError reports that the server delivered more
// messages than the list scan budget allows.
//
// This is the observed overrun: the count is what this process actually
// received, not the count the server claimed in the EXAMINE response. A
// server that under-reports its mailbox size is caught here rather than
// being believed.
//
// The recourse is to raise the limit. It is not to narrow the query: the
// date filters run in the client after the fetch and do not reduce the
// number of messages the server sends.
func listScanBudgetExceededError(mbx string, budget int) error {
	return fmt.Errorf("%w: the server delivered more than %d messages while listing mailbox %q, "+
		"which is the list scan budget; raise limits.max_list_scan_messages (maximum %d) to list it",
		mailbox.ErrListTooLarge, budget, mbx, config.MaxListScanMessagesLimit)
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
