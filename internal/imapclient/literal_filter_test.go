package imapclient

import (
	"bufio"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// memConn is a minimal net.Conn backed by a buffer for tests.
// It satisfies net.Conn but only Read and Close are exercised.
type memConn struct {
	net.Conn
	mu  sync.Mutex
	buf []byte
	// closed signals that the conn is closed. After Close,
	// reads return io.EOF.
	closed bool
	// readDeadline is a point in time after which reads
	// return immediately. Used to ensure the test does
	// not hang if the filter logic is wrong.
	readDeadline time.Time
}

func newMemConn(data []byte) *memConn {
	return &memConn{buf: data}
}

func (c *memConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.EOF
	}
	if !c.readDeadline.IsZero() && time.Now().After(c.readDeadline) {
		return 0, io.ErrUnexpectedEOF
	}
	if len(c.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *memConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *memConn) Close() error                { c.mu.Lock(); defer c.mu.Unlock(); c.closed = true; return nil }
func (c *memConn) LocalAddr() net.Addr         { return &net.TCPAddr{} }
func (c *memConn) RemoteAddr() net.Addr        { return &net.TCPAddr{} }
func (c *memConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	return nil
}
func (c *memConn) SetReadDeadline(t time.Time) error  { return c.SetDeadline(t) }
func (c *memConn) SetWriteDeadline(t time.Time) error { return nil }

// TestLiteralLimitConnUnderCap verifies that a literal of
// exactly the cap (or smaller) passes through the filter
// without closing the conn.
func TestLiteralLimitConnUnderCap(t *testing.T) {
	const cap = 100
	// IMAP literal: `{N}\r\n<literal body>`. We test with
	// a body smaller than the cap.
	body := []byte("hello world")
	raw := []byte("* OK [CAPABILITY IMAP4rev1] server ready\r\n")
	raw = append(raw, []byte("* 1 FETCH (BODY[] {")...)
	raw = append(raw, []byte("11}\r\n")...)
	raw = append(raw, body...)
	raw = append(raw, []byte(")\r\n")...)
	conn := newMemConn(raw)
	filter := newLiteralLimitConn(conn, conn, cap)
	defer filter.Close()

	// The bufio reader is what the go-imap client uses.
	// We read through it to exercise the filter the same
	// way the real client would.
	br := bufio.NewReader(filter)

	// Read the greeting line.
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString: %v", err)
	}
	if !contains(line, "OK") {
		t.Errorf("greeting = %q, want OK", line)
	}
	// Read the FETCH line up to the body.
	prefix, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString 2: %v", err)
	}
	if !contains(prefix, "FETCH") {
		t.Errorf("prefix = %q, want FETCH", prefix)
	}
	// The literal body should be readable.
	got := make([]byte, len(body))
	nn, err := io.ReadFull(br, got)
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if nn != len(body) {
		t.Errorf("ReadFull = %d, want %d", nn, len(body))
	}
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}

// TestLiteralLimitConnOverCap verifies that a literal larger
// than the cap closes the conn and returns io.EOF.
func TestLiteralLimitConnOverCap(t *testing.T) {
	const cap = 5
	body := []byte("hello world this is longer than the cap")
	raw := []byte("* 1 FETCH (BODY[] {")
	raw = append(raw, []byte(itoa(len(body)))...)
	raw = append(raw, []byte("}\r\n")...)
	raw = append(raw, body...)
	conn := newMemConn(raw)
	filter := newLiteralLimitConn(conn, conn, cap)
	defer filter.Close()

	br := bufio.NewReader(filter)

	// The filter detects the over-cap literal in the
	// lookahead and closes the conn. The first read
	// returns io.EOF; the bytes that were in the
	// lookahead are lost.
	buf := make([]byte, 16)
	n, err := br.Read(buf)
	if err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
	if n != 0 {
		t.Errorf("n = %d, want 0", n)
	}
	// Subsequent reads also return io.EOF.
	_, err = br.Read(buf)
	if err != io.EOF {
		t.Errorf("subsequent read err = %v, want io.EOF", err)
	}
}

// TestLiteralLimitConnBoundary exercises the filter at cap-1
// and cap exactly: both should pass through, since the cap
// applies to "exceeds" the configured size.
func TestLiteralLimitConnBoundary(t *testing.T) {
	for _, sz := range []int{4, 5} {
		const cap = 5
		if sz > cap {
			t.Fatalf("sz=%d > cap=%d", sz, cap)
		}
		body := make([]byte, sz)
		for i := range body {
			body[i] = 'A'
		}
		raw := []byte("* 1 FETCH (BODY[] {")
		raw = append(raw, []byte(itoa(sz))...)
		raw = append(raw, []byte("}\r\n")...)
		raw = append(raw, body...)
		conn := newMemConn(raw)
		filter := newLiteralLimitConn(conn, conn, cap)
		br := bufio.NewReader(filter)
		_, _ = br.ReadString('\n') // fetch header
		got := make([]byte, sz)
		_, err := io.ReadFull(br, got)
		if err != nil {
			t.Errorf("sz=%d: ReadFull err = %v", sz, err)
		}
		_ = filter.Close()
	}
}

// TestLiteralLimitConnNonSyncLiteral verifies that the
// filter handles the non-sync literal syntax `{N+}\r\n`.
// The cap check is on N, not the literal type.
func TestLiteralLimitConnNonSyncLiteral(t *testing.T) {
	const cap = 100
	body := []byte("ok")
	raw := []byte("* 1 FETCH (BODY[] {3+}\r\n")
	raw = append(raw, body...)
	raw = append(raw, []byte(")\r\n")...)
	conn := newMemConn(raw)
	filter := newLiteralLimitConn(conn, conn, cap)
	defer filter.Close()
	br := bufio.NewReader(filter)
	_, _ = br.ReadString('\n') // fetch header
	got := make([]byte, len(body))
	if _, err := io.ReadFull(br, got); err != nil {
		t.Errorf("ReadFull err = %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}

// TestFindLiteralHeader exercises the in-buffer literal
// header parser for the cases the filter relies on.
func TestFindLiteralHeader(t *testing.T) {
	for _, tc := range []struct {
		name      string
		buf       string
		wantIdx   int
		wantSz    int
		wantSync  bool
		wantFound bool
	}{
		{"sync, mid-line", "BODY[] {100}\r\nbody", 7, 100, false, true},
		{"non-sync, mid-line", "BODY[] {100+}\r\nbody", 7, 100, true, true},
		{"LF only", "BODY[] {50}\nbody", 7, 50, false, true},
		{"no literal", "BODY[] quoted\r\n", 0, 0, false, false},
		{"invalid size", "BODY[] {abc}\r\n", 0, 0, false, false},
		{"missing CRLF", "BODY[] {10}", 0, 0, false, false},
		{"at start", "{5}\r\nhello", 0, 5, false, true},
		{"empty", "", 0, 0, false, false},
		{"CR only", "BODY[] {5}\rbody", 0, 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, sz, sync, ok := findLiteralHeader([]byte(tc.buf))
			if ok != tc.wantFound || idx != tc.wantIdx || sz != tc.wantSz || sync != tc.wantSync {
				t.Errorf("findLiteralHeader(%q) = (%d, %d, %v, %v), want (%d, %d, %v, %v)",
					tc.buf, idx, sz, sync, ok, tc.wantIdx, tc.wantSz, tc.wantSync, tc.wantFound)
			}
		})
	}
}

// itoa is a small helper to avoid pulling strconv into the
// test file's hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func contains(s, substr string) bool {
	return len(substr) == 0 || (len(s) >= len(substr) && indexOf(s, substr) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
