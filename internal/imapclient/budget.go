package imapclient

// The server data budget.
//
// The go-imap reader's limits are per response: they reset at every
// response boundary. A server that sends an unlimited sequence of
// individually valid responses therefore still consumes an unlimited
// amount of input bytes, parsing work, heap allocation and CPU time in
// the client's reader goroutine.
//
// This file closes that gap at the transport layer. The budget wraps
// the socket *below* the TLS layer, so it accounts every byte the
// server sends, whether the connection is plaintext, implicitly
// encrypted or upgraded with STARTTLS.
//
// # The accounting
//
// One mutex governs the accounting and the window transitions, so a
// transition is a coordinated operation and not a set of independent
// atomic loads and stores.
//
// The byte counter is monotonic: it is never reset. A window measures
// the consumption since it opened, so a transition cannot lose
// accounting, and the bytes a read consumes while it is in flight are
// committed against the window that is active when the read completes.
//
// The read is in two phases. The allowance is checked and the read is
// capped under the mutex; the blocking network read happens *outside*
// it, so a transition is never blocked behind a pending read; and the
// bytes are then committed and re-validated under the mutex again.
// The linearization point is that commit: a successful read's bytes are
// committed against one window before they are returned to the decoder.
//
// A read that no longer fits the active allowance returns no payload
// bytes, closes the connection and preserves the typed violation, so
// a partially parsed response is never delivered.
//
// # The boundary
//
// An exhausted allowance does not refuse immediately. The reader
// performs a one-byte probe read, so a server that sent exactly the
// permitted amount and is then silent is not mistaken for an excess,
// while the first byte that arrives *is* the violation.
//
// # The violation record
//
// The violation belongs to the connection. It survives every window
// transition and is cleared only when the connection is discarded; the
// cause is then preserved on the backend long enough for the next
// operation to report it.

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// errServerDataBudgetExceeded is returned by the budget's Read when the
// connection's allowance is exhausted.
//
// It is deliberately not a parse error: the response stream is intact,
// the server simply sent more data than the operation allows.
var errServerDataBudgetExceeded = errors.New("imap: server data budget exceeded")

// budgetWindow is the allowance in force for the current phase of a
// connection's life.
type budgetWindow struct {
	// start is the value of the counter when the window opened.
	start int64
	// limit is the number of bytes the window permits beyond start.
	//
	// A limit of zero or less means the window is not armed. An
	// unarmed window permits no bytes at all: the budget fails
	// closed, so a zero value can never silently disable the
	// enforcement.
	limit int64
}

// serverDataBudget bounds the inbound protocol bytes one connection may
// consume.
type serverDataBudget struct {
	net.Conn

	mu sync.Mutex
	// used is the number of bytes consumed from the socket on this
	// connection. It is monotonic: it is never reset.
	used int64
	// delivered is the number of bytes handed to the decoder on this
	// connection. It is monotonic too. It is tracked separately from
	// used because a refused read consumes bytes without delivering
	// them.
	delivered int64
	// win is the allowance in force.
	win budgetWindow
	// violation is the connection-level record of a limit violation. It
	// survives every transition and is cleared only when the
	// connection is discarded.
	violation error
}

// setLimit opens a new window with the given allowance.
//
// The counter is not reset: the new window measures the consumption
// since it opened, so no accounting is lost across the transition. The
// violation record is not cleared either - it belongs to the
// connection, not to a window.
//
// The mutex is not held across a network read, so a transition is never
// blocked behind a pending read.
func (b *serverDataBudget) setLimit(limit int64) {
	if limit < 0 {
		limit = 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.win = budgetWindow{start: b.used, limit: limit}
}

// violationErr returns the connection's violation record, or nil.
func (b *serverDataBudget) violationErr() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.violation
}

// clearViolation clears the record. It is called when the connection is
// discarded.
func (b *serverDataBudget) clearViolation() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.violation = nil
}

// usedBytes returns the number of bytes consumed from the socket.
func (b *serverDataBudget) usedBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// deliveredBytes returns the number of bytes handed to the decoder.
func (b *serverDataBudget) deliveredBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.delivered
}

// windowLimit returns the allowance of the window in force.
func (b *serverDataBudget) windowLimit() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.win.limit
}

// Read reads from the connection, refusing to deliver bytes beyond the
// allowance in force when the read completes.
func (b *serverDataBudget) Read(p []byte) (int, error) {
	// A zero-length read carries no data, so there is nothing to
	// account and nothing to deliver. The io.Reader contract allows
	// it, and it must not be mistaken for an exhausted allowance.
	if len(p) == 0 {
		return 0, nil
	}

	// Phase 1: check the allowance and cap the read.
	b.mu.Lock()
	remaining := b.win.limit - (b.used - b.win.start)
	exhausted := b.win.limit <= 0 || remaining <= 0
	if !exhausted && int64(len(p)) > remaining {
		p = p[:remaining]
	}
	b.mu.Unlock()

	if exhausted {
		return b.probe(p)
	}

	// Phase 2: the blocking read, outside the mutex, so that a
	// transition is never blocked by a slow or silent server.
	n, err := b.Conn.Read(p)

	// Phase 3: commit the bytes against the window that is active
	// now, which may differ from the one the read began under.
	return b.commit(n, err)
}

// commit accounts for a completed read and re-validates it against the
// window in force.
//
// The commit is the linearization point: the bytes are accounted before
// they are returned to the decoder.
func (b *serverDataBudget) commit(n int, readErr error) (int, error) {
	b.mu.Lock()
	b.used += int64(n)
	over := b.win.limit <= 0 || (b.win.limit > 0 && b.used-b.win.start > b.win.limit)
	if over && b.violation == nil {
		b.violation = errServerDataBudgetExceeded
	}
	if !over {
		b.delivered += int64(n)
	}
	b.mu.Unlock()

	if over {
		// The bytes do not fit the allowance in force. Nothing is
		// delivered, so the decoder never sees a partial response,
		// and the connection is not usable any more.
		_ = b.Conn.Close()
		return 0, errServerDataBudgetExceeded
	}
	return n, readErr
}

// probe reads a single byte when the allowance is spent.
//
// The allowance is spent but the server may simply be silent: the
// reader waits for one byte, and the first byte that arrives is the
// excess. If a new window has been armed while the probe was pending,
// the byte is re-validated against that window and delivered if it
// fits.
func (b *serverDataBudget) probe(p []byte) (int, error) {
	var scratch [1]byte
	n, err := b.Conn.Read(scratch[:])

	b.mu.Lock()
	b.used += int64(n)
	over := n > 0 && (b.win.limit <= 0 || b.used-b.win.start > b.win.limit)
	if over && b.violation == nil {
		b.violation = errServerDataBudgetExceeded
	}
	if !over && n > 0 {
		b.delivered++
	}
	b.mu.Unlock()

	if over {
		// The byte arrived after the allowance was spent. Nothing is
		// delivered: a response that does not fit the allowance is
		// never handed to the decoder.
		_ = b.Conn.Close()
		return 0, errServerDataBudgetExceeded
	}
	if n > 0 {
		// The byte fits the window in force: deliver it.
		p[0] = scratch[0]
		return 1, err
	}
	if err != nil {
		// The transport outcome is preserved: a server that stopped
		// sending is not an excess.
		return 0, err
	}
	// A zero-length read with no error: the connection is idle.
	return 0, nil
}

// opKind selects the inbound data allowance and the wording of a
// refusal.
//
// The operations have materially different requirements: a summary
// fetch transfers one summary per scanned message, a raw message fetch
// transfers one message body, a mailbox enumeration transfers one
// response per mailbox, and the rest transfer a small response.
type opKind int

const (
	// opPlain is an operation that expects only a small response.
	opPlain opKind = iota
	// opSummary is an operation that fetches one summary per scanned
	// message: a listing or a search.
	opSummary
	// opMessage is an operation that fetches one raw message.
	opMessage
	// opMailboxList is a mailbox enumeration.
	opMailboxList
)

// dataAllowance returns the inbound protocol allowance of an operation
// of the given kind.
//
// The allowances are derived from the account's own configuration, so
// that they follow the limits the operator already sets and no new
// public option is needed.
func (b *Backend) dataAllowance(kind opKind) int64 {
	switch kind {
	case opSummary:
		return summaryDataAllowanceFor(b.scanBudget())
	case opMessage:
		return messageDataAllowanceFor(b.limits.MaxMessageBytes)
	case opMailboxList:
		return mailboxListDataAllowance
	default:
		return plainDataAllowance
	}
}

// serverDataBudgetError reports a violation with an operation-appropriate
// recourse.
//
// The wording never suggests an action that would not help: there is no
// narrower query for a mailbox enumeration, and the search scan budget
// has nothing to do with a message fetch. It never contains content
// from the server's response.
func serverDataBudgetError(kind opKind) error {
	switch kind {
	case opSummary:
		return fmt.Errorf("%w: the server returned more protocol data than this operation permits; "+
			"reduce the requested workload, for example by narrowing the query", mailbox.ErrServerDataBudgetExceeded)
	case opMailboxList:
		return fmt.Errorf("%w: the mailbox enumeration exceeded its server-data allowance",
			mailbox.ErrServerDataBudgetExceeded)
	case opMessage:
		return fmt.Errorf("%w: the message retrieval exceeded the protocol-data allowance",
			mailbox.ErrServerDataBudgetExceeded)
	default:
		return fmt.Errorf("%w: the server exceeded the idle connection allowance",
			mailbox.ErrServerDataBudgetExceeded)
	}
}

// idleServerDataBudgetError reports a violation that was detected on an
// idle connection.
//
// An idle violation is reported by the next operation, whatever that
// operation is: the allowance that was in force when the server sent
// the data was the idle one, so the idle wording is the accurate one.
func idleServerDataBudgetError() error {
	return fmt.Errorf("%w: the server exceeded the idle connection allowance",
		mailbox.ErrServerDataBudgetExceeded)
}

// The allowances.
//
// The allowances are derived from the account's own configuration, so
// that no new public option is introduced and the limits follow the
// limits the operator already sets. They are deliberately not one
// enormous number: a search, a summary fetch, a raw message fetch and
// a mailbox enumeration have materially different requirements.
const (
	// idleDataAllowance is the inbound protocol bytes an idle
	// connection may consume before it is closed.
	//
	// A legitimate server sends very few unsolicited responses: an
	// EXISTS when the mailbox changes, an EXPUNGE when a message is
	// removed, a FLAGS when a flag changes. A quarter of a mebibyte
	// is far more than that, and it keeps the cost of an idle flood
	// small.
	idleDataAllowance = 256 << 10

	// plainDataAllowance is the allowance of an operation that expects
	// only a small response.
	plainDataAllowance = 256 << 10

	// mailboxListDataAllowance is the allowance of a mailbox
	// enumeration.
	//
	// The plain allowance is far too small for a large mailbox list:
	// one LIST response line is thirty to forty bytes per mailbox, so
	// a quarter of a mebibyte is only a few thousand mailboxes, and
	// servers with long nested label names reach it sooner. The
	// enumeration therefore gets its own, much larger, bounded
	// allowance.
	//
	// This is a resource-policy change: the enumeration now has an
	// aggregate protocol-byte limit where it had none. The final
	// mailbox-count and enumeration policy is deferred to the mailbox
	// enumeration hardening work.
	mailboxListDataAllowance = 4 << 20

	// summaryDataAllowance is the inbound protocol bytes one summary
	// may cost.
	//
	// A summary carries the envelope's fields, the flags, the size and
	// the internal date: typically a few hundred bytes, and bounded by
	// the decoder's per-field limits. Four kibibytes leaves generous
	// room for long headers without making the aggregate allowance
	// enormous.
	summaryDataAllowance = 4 << 10

	// operationDataOverhead is the protocol allowance added to every
	// operation: the greeting, the login, the EXAMINE and the SEARCH
	// responses, the tagged completions and the TLS handshake.
	operationDataOverhead = 256 << 10

	// messageDataOverhead is the protocol allowance added to a raw
	// message fetch, on top of the message body itself: the envelope,
	// the headers and the tagged completion.
	messageDataOverhead = 1 << 20
)

// summaryDataAllowanceFor returns the inbound allowance of an operation
// that fetches one summary per scanned message.
//
// The result is the scan budget times the per-summary allowance plus the
// protocol overhead. It is computed in int64 and clamped, so that a
// large budget cannot overflow the arithmetic or wrap to zero.
func summaryDataAllowanceFor(scanBudget int) int64 {
	if scanBudget < 0 {
		scanBudget = 0
	}
	total := int64(scanBudget)*int64(summaryDataAllowance) + operationDataOverhead
	return clampDataAllowance(total)
}

// messageDataAllowanceFor returns the inbound allowance of an operation
// that fetches one raw message.
//
// The body is at most the configured maximum message size, so the
// allowance is that size plus the protocol overhead.
func messageDataAllowanceFor(maxMessageBytes int) int64 {
	if maxMessageBytes < 0 {
		maxMessageBytes = 0
	}
	return clampDataAllowance(int64(maxMessageBytes) + messageDataOverhead)
}

// clampDataAllowance keeps an allowance inside the range the budget
// can represent.
//
// The value is never clamped to zero: the budget treats an unarmed
// window as permitting no bytes at all, so a zero allowance would
// silently disable the enforcement.
func clampDataAllowance(v int64) int64 {
	if v <= 0 {
		return 1
	}
	const maxAllowance = int64(^uint64(0) >> 1)
	if v > maxAllowance {
		return maxAllowance
	}
	return v
}
