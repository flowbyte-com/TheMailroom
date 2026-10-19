package imapclient

// IMAP literal pre-allocation cap via a net.Conn filter.
//
// Background
// ----------
// The go-imap client (v1.2.1) buffers every IMAP literal in
// memory before the application sees it. Reader.ReadLiteral
// (read.go) does:
//
//     b := make([]byte, n)
//     if _, err := io.ReadFull(r, b); err != nil { ... }
//
// where n is the literal size announced by the server. The
// application-level cap enforced by the application reader
// (io.LimitReader + length check) is a *backstop*: it bounds
// the application's memory, but the protocol layer has
// already allocated `n` bytes for the literal before our
// reader is called.
//
// Approach
// --------
// We wrap the net.Conn with a literalLimitConn that scans
// the byte stream for IMAP literal headers (`{N}\r\n` or
// `{N+}\r\n`). When the announced size N exceeds the cap,
// the conn is closed and the next Read returns io.EOF. The
// go-imap executor sees the EOF and returns errClosed; our
// backend maps that to ErrMessageTooLarge.
//
// The IMAP protocol allows the literal header to appear
// inline in a response (e.g. inside a FETCH list, between
// the `BODY[]` token and the literal body). The filter
// scans the byte stream for the literal header and splits
// the response at the literal boundary: the bytes before
// the header, the literal body (N bytes), and the bytes
// after. The filter maintains a small lookahead buffer to
// handle the case where the literal header spans across
// multiple Read calls.
//
// In the byte-stream mode, the filter returns bytes from
// the lookahead buffer; if the lookahead doesn't have a
// full literal header, it returns what it has. The caller
// (the go-imap client's bufio.Reader) is responsible for
// line buffering. The filter only short-circuits the read
// when the literal body is in progress and the cap is hit.
//
// In the literal-body mode, the filter returns up to
// literalRem bytes directly from the underlying conn, then
// transitions back to byte-stream mode.
//
// Current status: DISABLED in dialAndLogin
// ----------------------------------------
// The filter is NOT currently wired into the implicit-TLS
// dial path because its whole-lookahead scan produces
// false positives on the TLS handshake (the binary
// protocol contains byte sequences that match `{N}\r\n`
// by accident). The pre-allocation cap is therefore not
// enforced for the implicit-TLS path. The application
// cap (io.LimitReader + length check) remains in effect
// as a backstop.
//
// To re-enable the filter, the literal-header detection
// must be line-aware: only consider `{N}\r\n` patterns
// that are on their own line, preceded by a known IMAP
// separator (space, `(`, etc.). A simpler "scan the whole
// lookahead" approach produces false positives because
// TLS records are binary and contain byte sequences
// that look like literal headers.

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
)

// literalLimitConn wraps a net.Conn and enforces a per-literal
// size cap.
type literalLimitConn struct {
	net.Conn
	// underlying is the real net.Conn; reads come from
	// here. We do not read from c.Conn (the embedded
	// net.Conn) because that would recurse into our own
	// Read method.
	underlying net.Conn
	// cap is the maximum literal body size in bytes.
	cap int
	// inLiteral tracks whether we are in the literal
	// body mode.
	inLiteral bool
	// literalRem is the number of bytes remaining to be
	// read from the literal body.
	literalRem int
	// lookahead is a small buffer that holds bytes not
	// yet returned to the caller. It is used to detect
	// literal headers that may span across multiple
	// Read calls.
	lookahead []byte
	mu        sync.Mutex
	hit       bool
}

const lookaheadMax = 4096

// newLiteralLimitConn returns a conn that enforces the
// literal cap on the given underlying conn. The cap is
// applied to every IMAP literal the client reads.
func newLiteralLimitConn(wrapper, underlying net.Conn, literalCap int) net.Conn {
	return &literalLimitConn{
		Conn:       wrapper,
		underlying: underlying,
		cap:        literalCap,
	}
}

// Read returns bytes from the underlying stream. The state
// machine has two modes:
//
//   - byte-stream (default): scan the lookahead for a
//     literal header. If found, return the bytes up to and
//     including the header, and transition to literal-body
//     mode. If the announced size exceeds the cap, close
//     the conn and return io.EOF. Otherwise, return the
//     bytes from the lookahead (or the next chunk read
//     from the underlying conn).
//   - literal-body: return up to literalRem bytes from
//     the underlying conn, then transition to byte-stream.
func (c *literalLimitConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if c.hit {
		c.mu.Unlock()
		return 0, io.EOF
	}
	inLiteral := c.inLiteral
	literalRem := c.literalRem
	c.mu.Unlock()

	if !inLiteral {
		// Ensure the lookahead has some bytes.
		if len(c.lookahead) == 0 {
			_, err := c.fillLookahead()
			if err != nil && len(c.lookahead) == 0 {
				return 0, err
			}
		}
		// Scan for a literal header. If the header is
		// within the current lookahead, split the
		// stream at the literal boundary.
		idx, sz, nonSync, ok := findLiteralHeader(c.lookahead)
		if ok {
			if c.cap > 0 && sz > c.cap {
				c.mu.Lock()
				c.hit = true
				c.mu.Unlock()
				_ = c.underlying.Close()
				return 0, io.EOF
			}
			// Return the bytes up to and
			// including the literal header, then
			// as much of the literal body as
			// fits in p. The body bytes already
			// in the lookahead are consumed
			// immediately; any remaining body
			// bytes are read directly from the
			// underlying conn in subsequent
			// calls.
			headerLen := idx + 1 // {
			if nonSync {
				headerLen++ // +
			}
			// Add the size digits.
			headerLen += len(strconv.Itoa(sz))
			headerLen++ // }
			// Add the trailing \r\n.
			if headerLen < len(c.lookahead) && c.lookahead[headerLen] == '\r' {
				headerLen++
			}
			if headerLen < len(c.lookahead) && c.lookahead[headerLen] == '\n' {
				headerLen++
			}
			// Body bytes available in the
			// lookahead.
			bodyInLookahead := len(c.lookahead) - headerLen
			if bodyInLookahead > sz {
				bodyInLookahead = sz
			}
			// How much we can return in this
			// call: the header plus as much
			// body as fits in p and in the
			// lookahead.
			headerCopy := copy(p, c.lookahead[:headerLen])
			bodyCopy := 0
			roomForBody := len(p) - headerCopy
			if roomForBody > bodyInLookahead {
				roomForBody = bodyInLookahead
			}
			if roomForBody > 0 {
				end := headerLen + roomForBody
				if end > len(c.lookahead) {
					end = len(c.lookahead)
				}
				bodyCopy = copy(p[headerCopy:], c.lookahead[headerLen:end])
			}
			total := headerCopy + bodyCopy
			c.lookahead = c.lookahead[total:]
			// Transition to literal-body mode if
			// any body bytes remain.
			remainingBody := bodyInLookahead - bodyCopy
			c.mu.Lock()
			c.inLiteral = remainingBody > 0
			c.literalRem = sz - bodyInLookahead
			c.mu.Unlock()
			return total, nil
		}
		// No literal header found. If the
		// lookahead is large (>= lookaheadMax), we
		// have read as much as we'll buffer. Return
		// what we have; the caller will call Read
		// again. If the lookahead is smaller, we
		// may not have a full header yet; try to
		// read more.
		if len(c.lookahead) >= lookaheadMax {
			n := copy(p, c.lookahead)
			c.lookahead = c.lookahead[n:]
			return n, nil
		}
		// Try to read more from the underlying.
		n, err := c.fillLookahead()
		if err != nil && len(c.lookahead) == 0 {
			return 0, err
		}
		if n > 0 {
			// We have more bytes; recurse to
			// retry the scan.
			return c.Read(p)
		}
		// No more bytes; return what we have.
		m := copy(p, c.lookahead)
		c.lookahead = c.lookahead[m:]
		return m, nil
	}

	// Literal-body mode. Return up to literalRem
	// bytes from the underlying conn.
	want := literalRem
	if want > len(p) {
		want = len(p)
	}
	n, err := c.underlying.Read(p[:want])
	if n > 0 {
		c.mu.Lock()
		c.literalRem -= n
		if c.literalRem == 0 {
			c.inLiteral = false
		}
		c.mu.Unlock()
	}
	return n, err
}

// fillLookahead reads from the underlying conn into the
// lookahead buffer, up to lookaheadMax bytes.
func (c *literalLimitConn) fillLookahead() (int, error) {
	free := lookaheadMax - len(c.lookahead)
	if free <= 0 {
		return 0, nil
	}
	buf := make([]byte, free)
	n, err := c.underlying.Read(buf)
	if n > 0 {
		c.lookahead = append(c.lookahead, buf[:n]...)
	}
	return n, err
}

// findLiteralHeader scans buf for an IMAP literal header
// (`{N}` or `{N+}`) followed by `\r\n` or `\n`. Returns
// the index of the opening brace, the parsed size, the
// non-sync flag, and whether a header was found.
func findLiteralHeader(buf []byte) (idx, sz int, nonSync, ok bool) {
	for i := 0; i < len(buf); i++ {
		if buf[i] != '{' {
			continue
		}
		// Try to parse the size. The format is
		// either `{N}` or `{N+}` (non-sync
		// literal). The `+` can appear either
		// before or after the digits per go-imap's
		// own parser (write.go::writeLiteral).
		j := i + 1
		ns := false
		if j < len(buf) && buf[j] == '+' {
			ns = true
			j++
		}
		start := j
		for j < len(buf) && buf[j] >= '0' && buf[j] <= '9' {
			j++
		}
		if j == start {
			continue
		}
		// The non-sync `+` may also appear after the
		// digits, before the closing brace. Strip it
		// from the size before Atoi.
		if j < len(buf) && buf[j] == '+' {
			ns = true
			j++
		}
		if j >= len(buf) || buf[j] != '}' {
			continue
		}
		sizeEnd := j
		if ns {
			sizeEnd = j - 1
		}
		n, err := strconv.Atoi(string(buf[start:sizeEnd]))
		if err != nil || n < 0 {
			continue
		}
		// Check for trailing \r\n or \n. The header
		// is only valid if the terminator is within
		// the current buffer; otherwise we wait
		// for more bytes.
		k := j + 1
		hasCR := false
		if k < len(buf) && buf[k] == '\r' {
			hasCR = true
			k++
		}
		if k >= len(buf) || buf[k] != '\n' {
			// Need more bytes; the header
			// might span across reads.
			if k >= lookaheadMax {
				// Header is too long
				// for our lookahead;
				// not a valid header.
				continue
			}
			return 0, 0, false, false
		}
		_ = hasCR
		return i, n, ns, true
	}
	return 0, 0, false, false
}

// String returns a debug-friendly representation.
func (c *literalLimitConn) String() string {
	return fmt.Sprintf("literalLimitConn(cap=%d, hit=%v)", c.cap, c.hit)
}
