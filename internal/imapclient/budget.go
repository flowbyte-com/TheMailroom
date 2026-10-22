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
// encrypted or upgraded with STARTTLS. The check happens before the
// bytes are handed to the reader, so an over-budget response is never
// parsed and never allocated: this is a pre-allocation bound, not a
// post-parse check.
//
// The budget is reset at the start of a backend operation and set from
// that operation's expected inbound size, so the allowance of one
// operation covers its commands *and* the unsolicited responses that
// arrive while they run. Between operations the allowance is the small
// idle one, so a server that floods an idle connection loses it
// quickly.

import (
	"errors"
	"net"
	"sync/atomic"
)

// errServerDataBudgetExceeded is returned by the budget's Read when the
// connection's allowance is exhausted.
//
// It is deliberately not a parse error: the response stream is intact,
// the server simply sent more data than the operation allows.
var errServerDataBudgetExceeded = errors.New("imap: server data budget exceeded")

// serverDataBudget bounds the inbound protocol bytes one connection may
// consume.
//
// The counter is written by the reader goroutine - the go-imap reader
// reads through it - and read and reset by the application goroutine,
// so the state is atomic.
type serverDataBudget struct {
	net.Conn

	// limit is the remaining allowance in bytes. Zero means no limit.
	limit atomic.Int64
	// used is the number of bytes consumed since the last reset.
	used atomic.Int64
	// exceeded records that the allowance ran out.
	exceeded atomic.Bool
}

// setLimit sets the remaining allowance and resets the counter.
//
// A limit of zero means "no limit", which is the state of a connection
// whose operation does not need one.
func (b *serverDataBudget) setLimit(limit int64) {
	b.limit.Store(limit)
	b.used.Store(0)
	b.exceeded.Store(false)
}

// usedBytes returns the number of bytes consumed since the last reset.
func (b *serverDataBudget) usedBytes() int64 { return b.used.Load() }

// wasExceeded reports whether the allowance ran out.
func (b *serverDataBudget) wasExceeded() bool { return b.exceeded.Load() }

// Read reads from the connection, refusing to read past the allowance.
//
// The read is capped to the remaining allowance, so the budget is
// never exceeded by more than the bytes already in flight, and the read
// that would exceed it fails instead.
func (b *serverDataBudget) Read(p []byte) (int, error) {
	limit := b.limit.Load()
	if limit > 0 {
		used := b.used.Load()
		if used >= limit {
			b.exceeded.Store(true)
			// Close the connection as well: the server must stop
			// sending, and the socket is unusable from here on.
			_ = b.Conn.Close()
			return 0, errServerDataBudgetExceeded
		}
		if remaining := limit - used; int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := b.Conn.Read(p)
	if n > 0 {
		b.used.Add(int64(n))
	}
	return n, err
}

// opKind selects the inbound data allowance of a backend operation.
//
// The operations have materially different requirements: a summary
// fetch transfers one summary per scanned message, a raw message fetch
// transfers one message body, and the rest transfer a small response.
type opKind int

const (
	// opPlain is an operation that expects only a small response.
	opPlain opKind = iota
	// opSummary is an operation that fetches one summary per scanned
	// message.
	opSummary
	// opMessage is an operation that fetches one raw message.
	opMessage
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
	default:
		return plainDataAllowance()
	}
}

// The allowances.
//
// The allowances are derived from the account's own configuration, so
// that no new public option is introduced and the limits follow the
// limits the operator already sets. They are deliberately not one
// enormous number: a search, a summary fetch and a raw message fetch
// have materially different requirements.
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

// plainDataAllowance returns the inbound allowance of an operation that
// expects only a small response.
func plainDataAllowance() int64 {
	return operationDataOverhead
}

// clampDataAllowance keeps an allowance inside the range the budget
// can represent.
//
// The value is never clamped to zero: zero means "no limit" in the
// budget, and an allowance of zero would silently disable the
// enforcement.
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
