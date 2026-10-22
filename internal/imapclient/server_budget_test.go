package imapclient

// --- the aggregate server data budget ---
//
// The decoder's limits are per response. The server data budget is the
// aggregate: it bounds the inbound protocol bytes one operation may
// consume, including the unsolicited responses that arrive while its
// commands run.
//
// The tests below use local fixtures and deterministic sizes: no test
// sleeps to sequence an event, and none of them allocates more than a
// few megabytes. Where a test needs a read to be pending while the
// allowance changes, it uses a connection whose reads the test
// controls, so the interleaving is exact rather than probable.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/client"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// idleFloodResponses is the number of unsolicited responses an idle
// flood sends.
//
// Each response is thirteen bytes, so the flood is about 400 kB: more
// than the idle allowance of 256 KiB, and less than the summary
// allowance of a hundred-message budget. The idle window is therefore
// what stops it.
const idleFloodResponses = 30770

// --- the controlled connection ---

// budgetTestConn is a net.Conn whose reads the test controls.
//
// It makes the interleaving between a pending read and an allowance
// transition deterministic: the test knows that the read has entered
// the connection before it changes the allowance, and it decides what
// the read returns. No sleep is involved.
type budgetTestConn struct {
	// results carries what each read returns. The test queues one
	// result per read it expects.
	results chan budgetTestResult
	// entered carries one token per read that has entered the
	// connection, carrying the number of that read.
	entered chan int

	mu     sync.Mutex
	reads  int
	closed bool
}

// budgetTestResult is what one read of the controlled connection
// returns.
type budgetTestResult struct {
	data []byte
	err  error
}

// newBudgetTestConn returns a connection whose reads block until the
// test queues a result.
func newBudgetTestConn() *budgetTestConn {
	return &budgetTestConn{
		results: make(chan budgetTestResult, 16),
		entered: make(chan int, 16),
	}
}

// push queues the result of the next read.
func (c *budgetTestConn) push(r budgetTestResult) {
	c.results <- r
}

// awaitRead blocks until the given read has entered the connection.
//
// The read is numbered from one, so the test can wait for a specific
// read and not for "some read": a test that waits for the wrong read
// would let the transition happen before the read it cares about even
// started.
func (c *budgetTestConn) awaitRead(t *testing.T, want int) {
	t.Helper()
	for {
		select {
		case got := <-c.entered:
			if got == want {
				return
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("read %d never entered the connection", want)
		}
	}
}

func (c *budgetTestConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	c.reads++
	n := c.reads
	c.mu.Unlock()
	c.entered <- n
	r, ok := <-c.results
	if !ok {
		return 0, io.EOF
	}
	if len(r.data) == 0 {
		return 0, r.err
	}
	read := copy(p, r.data)
	return read, r.err
}

func (c *budgetTestConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *budgetTestConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	// A pending read must end: the connection is gone.
	select {
	case c.results <- budgetTestResult{err: net.ErrClosed}:
	default:
	}
	return nil
}

func (c *budgetTestConn) LocalAddr() net.Addr              { return budgetTestAddr{} }
func (c *budgetTestConn) RemoteAddr() net.Addr             { return budgetTestAddr{} }
func (c *budgetTestConn) SetDeadline(time.Time) error      { return nil }
func (c *budgetTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *budgetTestConn) SetWriteDeadline(time.Time) error { return nil }

// budgetTestAddr is the address of the controlled connection.
type budgetTestAddr struct{}

func (budgetTestAddr) Network() string { return "test" }
func (budgetTestAddr) String() string  { return "test" }

// readOutcome is the result of a read that runs in its own goroutine.
type readOutcome struct {
	n   int
	err error
}

// --- the transition and the boundary ---

// TestServerDataBudgetStaleReadCrossingToASmallerAllowance checks the
// transition HAL's review called out: a read that began under one
// allowance must not deliver bytes after a smaller allowance has been
// installed.
//
// The read is pending in the connection when the allowance changes, so
// the interleaving is exact: the read has already captured the old
// allowance when the new one is installed.
func TestServerDataBudgetStaleReadCrossingToASmallerAllowance(t *testing.T) {
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	budget.setLimit(4096)

	done := make(chan readOutcome, 1)
	go func() {
		buf := make([]byte, 4096)
		n, err := budget.Read(buf)
		done <- readOutcome{n, err}
	}()

	// The read is now pending in the connection.
	conn.awaitRead(t, 1)

	// The allowance shrinks while the read is in flight.
	budget.setLimit(64)

	// The read completes with a full buffer, far beyond the allowance
	// that is now in force.
	conn.push(budgetTestResult{data: make([]byte, 4096)})

	got := <-done
	if got.err == nil {
		t.Fatalf("the read returned %d bytes and no error, want a refusal", got.n)
	}
	if !errors.Is(got.err, errServerDataBudgetExceeded) {
		t.Errorf("err = %v, want errServerDataBudgetExceeded", got.err)
	}
	if got.n != 0 {
		t.Errorf("the read delivered %d bytes, want 0: a read that does not fit the allowance must deliver nothing", got.n)
	}
	if budget.violationErr() == nil {
		t.Error("the violation was not recorded")
	}
}

// TestServerDataBudgetStaleReadCrossingToALargerAllowance checks the
// other direction: a read that began under a small allowance and
// completes after a larger allowance has been installed is delivered,
// because it fits the allowance that is in force when it completes.
func TestServerDataBudgetStaleReadCrossingToALargerAllowance(t *testing.T) {
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	budget.setLimit(64)

	done := make(chan readOutcome, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := budget.Read(buf)
		done <- readOutcome{n, err}
	}()

	conn.awaitRead(t, 1)

	// The allowance grows while the read is in flight.
	budget.setLimit(4096)

	conn.push(budgetTestResult{data: make([]byte, 64)})

	got := <-done
	if got.err != nil {
		t.Fatalf("the read failed with %v, want it to be delivered", got.err)
	}
	if got.n != 64 {
		t.Errorf("the read delivered %d bytes, want 64", got.n)
	}
	if budget.violationErr() != nil {
		t.Error("a read that fits the allowance was recorded as a violation")
	}
}

// TestServerDataBudgetProbeCrossingIntoANewWindow checks the probe
// that runs when the allowance is spent.
//
// The probe waits for the server instead of refusing, and the byte it
// receives is re-validated against the window that is in force when
// the byte arrives: a window armed while the probe was pending may
// allow it.
func TestServerDataBudgetProbeCrossingIntoANewWindow(t *testing.T) {
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	budget.setLimit(64)

	buf := make([]byte, 64)
	conn.push(budgetTestResult{data: make([]byte, 64)})
	if n, err := budget.Read(buf); err != nil || n != 64 {
		t.Fatalf("the first read returned %d bytes and %v, want 64 bytes and no error", n, err)
	}

	// The allowance is spent, so the next read is a probe.
	done := make(chan readOutcome, 1)
	go func() {
		n, err := budget.Read(buf)
		done <- readOutcome{n, err}
	}()
	conn.awaitRead(t, 2)

	// A new window is armed while the probe is pending.
	budget.setLimit(4096)

	// The server sends one byte. It fits the window in force.
	conn.push(budgetTestResult{data: []byte("x")})

	got := <-done
	if got.err != nil {
		t.Fatalf("the probe failed with %v, want the byte to be delivered", got.err)
	}
	if got.n != 1 {
		t.Errorf("the probe delivered %d bytes, want 1", got.n)
	}
	if budget.violationErr() != nil {
		t.Error("the probe recorded a violation for a byte the new window allows")
	}
}

// TestServerDataBudgetExactlyAtBudgetThenSilence checks that a
// connection that received exactly the permitted amount and is then
// silent is not declared to have received excess data.
//
// The reader asked for one more read, but the server sent nothing
// beyond the allowance, so there is no excess.
func TestServerDataBudgetExactlyAtBudgetThenSilence(t *testing.T) {
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	budget.setLimit(64)

	buf := make([]byte, 64)
	conn.push(budgetTestResult{data: make([]byte, 64)})
	if n, err := budget.Read(buf); err != nil || n != 64 {
		t.Fatalf("the first read returned %d bytes and %v, want 64 bytes and no error", n, err)
	}

	// The allowance is spent. The next read waits for the server.
	done := make(chan readOutcome, 1)
	go func() {
		n, err := budget.Read(buf)
		done <- readOutcome{n, err}
	}()
	conn.awaitRead(t, 2)

	// The read is waiting for the server: it has not been refused.
	select {
	case got := <-done:
		t.Fatalf("the read returned %d bytes and %v while the server was silent, want it to wait", got.n, got.err)
	default:
	}
	if budget.violationErr() != nil {
		t.Error("a silent server was recorded as an excess")
	}

	// The connection ends: the waiting read ends with the transport's
	// outcome, not with a violation.
	conn.Close()
	got := <-done
	if errors.Is(got.err, errServerDataBudgetExceeded) {
		t.Errorf("the read ended with the budget error %v, want the transport's outcome", got.err)
	}
}

// TestServerDataBudgetExactlyAtBudgetThenEOF checks that the
// transport's outcome is preserved when the server stops sending at
// exactly the permitted amount.
func TestServerDataBudgetExactlyAtBudgetThenEOF(t *testing.T) {
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	budget.setLimit(64)

	buf := make([]byte, 64)
	conn.push(budgetTestResult{data: make([]byte, 64)})
	if n, err := budget.Read(buf); err != nil || n != 64 {
		t.Fatalf("the first read returned %d bytes and %v, want 64 bytes and no error", n, err)
	}

	// The server stops sending at exactly the permitted amount.
	conn.push(budgetTestResult{err: io.EOF})
	n, err := budget.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want the end of the stream to be preserved", err)
	}
	if n != 0 {
		t.Errorf("the read delivered %d bytes, want 0", n)
	}
	if budget.violationErr() != nil {
		t.Error("the end of the stream was recorded as an excess")
	}
}

// TestServerDataBudgetExactlyAtBudgetThenOneExtraByte checks that the
// byte beyond the allowance is refused and recorded.
//
// This is the case the boundary has to get right in the other
// direction: the server did send more than the allowance permits.
func TestServerDataBudgetExactlyAtBudgetThenOneExtraByte(t *testing.T) {
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	budget.setLimit(64)

	buf := make([]byte, 64)
	conn.push(budgetTestResult{data: make([]byte, 64)})
	if n, err := budget.Read(buf); err != nil || n != 64 {
		t.Fatalf("the first read returned %d bytes and %v, want 64 bytes and no error", n, err)
	}

	// One byte more than the allowance.
	conn.push(budgetTestResult{data: []byte("x")})
	n, err := budget.Read(buf)
	if !errors.Is(err, errServerDataBudgetExceeded) {
		t.Errorf("err = %v, want errServerDataBudgetExceeded", err)
	}
	if n != 0 {
		t.Errorf("the read delivered %d bytes, want 0", n)
	}
	if budget.violationErr() == nil {
		t.Error("the violation was not recorded")
	}
}

// TestServerDataBudgetAccountingIsMonotonicAcrossTransitions checks
// that the counter is never reset.
//
// A window measures the consumption since it opened, so the total is
// preserved across every transition.
func TestServerDataBudgetAccountingIsMonotonicAcrossTransitions(t *testing.T) {
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	budget.setLimit(4096)

	buf := make([]byte, 100)
	conn.push(budgetTestResult{data: make([]byte, 100)})
	if _, err := budget.Read(buf); err != nil {
		t.Fatalf("the first read failed with %v", err)
	}
	if got := budget.usedBytes(); got != 100 {
		t.Fatalf("the counter reads %d, want 100", got)
	}

	// A transition must not reset the accounting.
	budget.setLimit(4096)
	conn.push(budgetTestResult{data: make([]byte, 50)})
	if _, err := budget.Read(buf); err != nil {
		t.Fatalf("the second read failed with %v", err)
	}
	if got := budget.usedBytes(); got != 150 {
		t.Errorf("the counter reads %d, want 150: a transition must not reset the accounting", got)
	}
	if got := budget.deliveredBytes(); got != 150 {
		t.Errorf("the delivered counter reads %d, want 150", got)
	}
}

// TestServerDataBudgetViolationSurvivesTheTransition checks that the
// violation record belongs to the connection.
//
// Arming a new window must not erase it: the record is what tells the
// next operation why the connection is gone.
func TestServerDataBudgetViolationSurvivesTheTransition(t *testing.T) {
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	budget.setLimit(64)

	buf := make([]byte, 64)
	conn.push(budgetTestResult{data: make([]byte, 64)})
	if _, err := budget.Read(buf); err != nil {
		t.Fatalf("the first read failed with %v", err)
	}
	conn.push(budgetTestResult{data: []byte("x")})
	if _, err := budget.Read(buf); !errors.Is(err, errServerDataBudgetExceeded) {
		t.Fatalf("the read past the allowance returned %v, want errServerDataBudgetExceeded", err)
	}

	// Arming a new window must not erase the record.
	budget.setLimit(4096)
	if budget.violationErr() == nil {
		t.Error("arming a new window erased the violation record")
	}
}

// TestServerDataBudgetIsNotDisabledByZero checks that a limit of zero
// means "no limit" and that the clamped allowances are never zero.
func TestServerDataBudgetIsNotDisabledByZero(t *testing.T) {
	if got := clampDataAllowance(0); got != 1 {
		t.Errorf("clampDataAllowance(0) = %d, want 1: zero would disable the enforcement", got)
	}
	if got := clampDataAllowance(-5); got != 1 {
		t.Errorf("clampDataAllowance(-5) = %d, want 1", got)
	}
	if got := summaryDataAllowanceFor(0); got <= 0 {
		t.Errorf("summaryDataAllowanceFor(0) = %d, want a positive allowance", got)
	}
	if got := messageDataAllowanceFor(0); got <= 0 {
		t.Errorf("messageDataAllowanceFor(0) = %d, want a positive allowance", got)
	}
	// An unarmed window permits nothing: the budget fails closed, so a
	// zero value can never silently disable the enforcement.
	conn := newBudgetTestConn()
	budget := &serverDataBudget{Conn: conn}
	buf := make([]byte, 16)
	conn.push(budgetTestResult{data: make([]byte, 16)})
	if n, err := budget.Read(buf); n != 0 || !errors.Is(err, errServerDataBudgetExceeded) {
		t.Errorf("the read on an unarmed budget returned %d bytes and %v, want a refusal", n, err)
	}
}

// TestSummaryDataAllowanceIsDerivedFromTheScanBudget checks the
// derivation of the summary allowance.
//
// The allowance must grow with the scan budget, so that a larger budget
// is not refused by the aggregate bound, and it must not overflow.
func TestSummaryDataAllowanceIsDerivedFromTheScanBudget(t *testing.T) {
	small := summaryDataAllowanceFor(10)
	large := summaryDataAllowanceFor(10000)
	if large <= small {
		t.Errorf("the allowance did not grow with the budget: %d then %d", small, large)
	}
	if large < int64(10000)*summaryDataAllowance {
		t.Errorf("the allowance %d does not cover the budget times the per-summary allowance", large)
	}
	// The largest budget the configuration allows must not overflow.
	max := summaryDataAllowanceFor(1000000)
	if max <= 0 {
		t.Error("the allowance at the maximum budget is not positive")
	}
	if got := summaryDataAllowanceFor(-1); got <= 0 {
		t.Errorf("the allowance for a negative budget is %d, want a positive one", got)
	}
}

// --- the backend ---

// TestServerDataBudgetViolationAfterASuccessfulCallback checks that a
// violation is reported even when the backend callback appeared to
// succeed.
//
// The callback waits for the connection to be invalidated by the flood,
// so the violation is recorded before the callback returns. A backend
// that only looked at the callback's error would report a complete
// result for data the flood invalidated.
func TestServerDataBudgetViolationAfterASuccessfulCallback(t *testing.T) {
	srv := &listFixtureServer{floodAfterList: idleFloodResponses}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	err := be.withClient(t.Context(), opPlain, func(c *client.Client) error {
		ch := make(chan *imap.MailboxInfo, 16)
		done := make(chan error, 1)
		go func() { done <- c.List("", "*", ch) }()
		for range ch {
		}
		if err := <-done; err != nil {
			return err
		}
		// The flood follows the completion. Wait for the connection to
		// be invalidated by it, and then report success.
		select {
		case <-c.LoggedOut():
		case <-time.After(30 * time.Second):
			t.Error("the connection was not invalidated by the flood")
		}
		return nil
	})
	if err == nil {
		t.Fatal("withClient returned no error, want the budget violation")
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
	}
}

// TestServerDataBudgetIdleFloodAfterACompletedOperation checks that a
// flood that arrives while the connection is idle is detected.
//
// The flood needs no command to trigger it: the operation has already
// returned and the server keeps sending. The connection is invalidated,
// the next operation reports the violation, and the operation after
// that reconnects.
func TestServerDataBudgetIdleFloodAfterACompletedOperation(t *testing.T) {
	const budget = 100
	srv := &listFixtureServer{
		messages:   budgetMessages(1),
		searchUIDs: []uint32{1},
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	// A first operation succeeds and leaves the connection idle.
	if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{}); err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}

	be.mu.Lock()
	conn := be.client
	be.mu.Unlock()
	if conn == nil {
		t.Fatal("the backend holds no client after a successful operation")
	}

	// The server floods the idle connection. No command is sent: the
	// flood is unsolicited.
	srv.idleFloodNow(idleFloodResponses)

	// The connection must be invalidated by the idle allowance.
	select {
	case <-conn.LoggedOut():
	case <-time.After(30 * time.Second):
		t.Fatal("the idle flood did not invalidate the connection")
	}

	// The next operation recognises the violation and reports it.
	_, err := be.Mailboxes(t.Context(), "test")
	if err == nil {
		t.Fatal("the operation after the idle flood returned no error")
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Errorf("err = %v, want it to name the idle allowance", err)
	}

	// The operation after that reconnects and succeeds. The flood was a
	// single burst, so the new connection is quiet.
	if _, err := be.Mailboxes(t.Context(), "test"); err != nil {
		t.Fatalf("Mailboxes after the idle flood: %v", err)
	}
	if got := srv.connectionCount(); got < 2 {
		t.Errorf("the server saw %d connections, want at least 2: the flooded connection must be discarded", got)
	}
}

// TestServerDataBudgetIdleFloodUnderBothTLSModes checks the idle flood
// under implicit TLS and STARTTLS.
//
// The budget wraps the socket below the TLS layer, so it accounts the
// same bytes in both modes.
func TestServerDataBudgetIdleFloodUnderBothTLSModes(t *testing.T) {
	const budget = 100
	for _, mode := range []struct {
		name     string
		startTLS bool
	}{
		{name: "implicit TLS", startTLS: false},
		{name: "STARTTLS", startTLS: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			srv := &listFixtureServer{
				messages:   budgetMessages(1),
				searchUIDs: []uint32{1},
				startTLS:   mode.startTLS,
			}
			addr, cleanup := srv.start(t)
			defer cleanup()

			be := listFixtureBackendMode(t, addr, budget, mode.startTLS, 30*time.Second)
			defer be.Close()

			if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{}); err != nil {
				t.Fatalf("SearchMessages: %v", err)
			}

			be.mu.Lock()
			conn := be.client
			be.mu.Unlock()
			if conn == nil {
				t.Fatal("the backend holds no client after a successful operation")
			}

			srv.idleFloodNow(idleFloodResponses)

			select {
			case <-conn.LoggedOut():
			case <-time.After(30 * time.Second):
				t.Fatal("the idle flood did not invalidate the connection")
			}

			_, err := be.Mailboxes(t.Context(), "test")
			if err == nil {
				t.Fatal("the operation after the idle flood returned no error")
			}
			if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
				t.Errorf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
			}
			if errors.Is(err, mailbox.ErrMalformed) {
				t.Errorf("err = %v, want it not to be classified as a malformed response", err)
			}
			if errors.Is(err, mailbox.ErrResponseTooLarge) {
				t.Errorf("err = %v, want it not to be classified as a decoder-limit violation", err)
			}
		})
	}
}

// TestServerDataBudgetDiscardsTheWholeConnection checks that the
// client, the socket, the budget and the selection metadata are
// dropped together when a connection is discarded.
//
// The three of them always refer to the same connection: a budget that
// outlived its connection would account the wrong bytes.
func TestServerDataBudgetDiscardsTheWholeConnection(t *testing.T) {
	const budget = 10
	srv := &listFixtureServer{
		messages:          budgetMessages(budget),
		searchUIDs:        []uint32{1},
		floodAfterExamine: idleFloodResponses,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{}); err == nil {
		t.Fatal("expected the flood to be refused")
	}

	be.mu.Lock()
	client, conn, b := be.client, be.conn, be.budget
	be.mu.Unlock()
	if client != nil {
		t.Error("the client was retained after the flood")
	}
	if conn != nil {
		t.Error("the socket was retained after the flood")
	}
	if b != nil {
		t.Error("the budget was retained after the flood")
	}
	if got := be.SelectedUIDValidity(); got != 0 {
		t.Errorf("the selection metadata was retained after the flood: UIDVALIDITY = %d", got)
	}
}

// TestServerDataBudgetReconnectsAfterADiscardedConnection checks that
// a discarded connection is replaced and that the replacement serves a
// normal operation.
func TestServerDataBudgetReconnectsAfterADiscardedConnection(t *testing.T) {
	const budget = 10
	srv := &listFixtureServer{
		messages:          budgetMessages(budget),
		searchUIDs:        []uint32{1},
		floodAfterExamine: idleFloodResponses,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{}); err == nil {
		t.Fatal("expected the flood to be refused")
	}

	// The flood stops: the next operation reconnects and succeeds.
	srv.setFloodResponses(0)
	srv.setExamineFlood(0)
	srv.setMessages(budgetMessages(1), 0)
	srv.setSearchUIDs([]uint32{1})

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages after the flood: %v", err)
	}
	if len(res.Messages) != 1 {
		t.Errorf("got %d messages, want 1", len(res.Messages))
	}
	if got := srv.connectionCount(); got < 2 {
		t.Errorf("the server saw %d connections, want at least 2", got)
	}
}

// TestServerDataBudgetKeepsPlainOperationsCompatible checks that an
// ordinary operation is unaffected by the budget.
func TestServerDataBudgetKeepsPlainOperationsCompatible(t *testing.T) {
	srv := &listFixtureServer{messages: budgetMessages(1)}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	uv, err := be.MailboxUIDValidity(t.Context(), "test", "INBOX")
	if err != nil {
		t.Fatalf("MailboxUIDValidity: %v", err)
	}
	if uv == 0 {
		t.Error("MailboxUIDValidity returned 0")
	}
	if got := be.SelectedUIDValidity(); got != uv {
		t.Errorf("SelectedUIDValidity = %d, want %d", got, uv)
	}
}

// TestServerDataBudgetFloodBeforeTheLoginIsRefused checks the
// connection-setup path.
//
// A server that floods before the client has issued a command exhausts
// the allowance while the connection is being established. That is the
// same condition as a flood during an operation, and it is reported the
// same way.
func TestServerDataBudgetFloodBeforeTheLoginIsRefused(t *testing.T) {
	srv := &listFixtureServer{
		messages:       budgetMessages(1),
		searchUIDs:     []uint32{1},
		floodResponses: idleFloodResponses,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatal("expected the flood to be refused")
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
	}
	if errors.Is(err, mailbox.ErrMalformed) {
		t.Errorf("err = %v, want it not to be classified as a malformed response", err)
	}
	// A failed connection setup leaves no state behind.
	be.mu.Lock()
	client, conn, b := be.client, be.conn, be.budget
	be.mu.Unlock()
	if client != nil {
		t.Error("a client was retained after the failed connection setup")
	}
	if conn != nil {
		t.Error("a socket was retained after the failed connection setup")
	}
	if b != nil {
		t.Error("a budget was retained after the failed connection setup")
	}
}

// TestServerDataBudgetKeepsLegitimateResults checks that the budget
// does not interfere with an ordinary operation.
func TestServerDataBudgetKeepsLegitimateResults(t *testing.T) {
	const budget = 50
	msgs := make([]listFixtureMessage, budget)
	uids := make([]uint32, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	_, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(res.Messages) != budget {
		t.Errorf("got %d messages, want %d", len(res.Messages), budget)
	}
}

// TestLegitimateOperationLargerThanTheIdleAllowanceSucceeds checks
// that an operation whose legitimate traffic exceeds the idle
// allowance is still served.
//
// The idle allowance is deliberately small, because a server is
// expected to send almost nothing between operations. An operation's
// own allowance is larger, and it has to be armed: otherwise a
// legitimate summary listing would be refused.
func TestLegitimateOperationLargerThanTheIdleAllowanceSucceeds(t *testing.T) {
	const budget = 400
	// The summaries carry a long subject, so the operation transfers
	// comfortably more than the idle allowance.
	subject := strings.Repeat("s", 1024)
	msgs := make([]listFixtureMessage, budget)
	uids := make([]uint32, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, subject)
		uids[i] = uint32(i + 1)
	}
	srv, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	// The operation must transfer more than the idle allowance.
	if got := int64(budget) * 1024; got <= idleDataAllowance {
		t.Fatalf("the test transfers %d bytes, which does not exceed the idle allowance %d", got, idleDataAllowance)
	}

	if _, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{}); err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	// Every summary was fetched, so the whole transfer went through the
	// connection.
	if got := srv.servedCount(); got != budget {
		t.Errorf("the server served %d summaries, want %d", got, budget)
	}
}

// TestServerDataBudgetLargeMailboxEnumerationSucceeds checks that a
// legitimate large mailbox list is served.
//
// The list is about 370 kB: above the plain allowance of 256 KiB, so
// the small operation allowance would refuse it, and well within the
// enumeration allowance of 4 MiB.
func TestServerDataBudgetLargeMailboxEnumerationSucceeds(t *testing.T) {
	const count = 4000
	srv := &listFixtureServer{}
	srv.setListMailboxes(count, 64)
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	boxes, err := be.Mailboxes(t.Context(), "test")
	if err != nil {
		t.Fatalf("Mailboxes: %v", err)
	}
	if len(boxes) != count {
		t.Errorf("got %d mailboxes, want %d", len(boxes), count)
	}
}

// TestServerDataBudgetMailboxEnumerationExceedingItsAllowance checks
// that a mailbox list beyond the enumeration allowance is refused with
// the typed error.
//
// The list is about 4.7 MB, which is past the 4 MiB enumeration
// allowance. The fixture generates the responses on the fly, so the
// test holds no large buffer.
func TestServerDataBudgetMailboxEnumerationExceedingItsAllowance(t *testing.T) {
	const count = 50000
	srv := &listFixtureServer{}
	srv.setListMailboxes(count, 64)
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	boxes, err := be.Mailboxes(t.Context(), "test")
	if err == nil {
		t.Fatalf("Mailboxes returned %d mailboxes, want a refusal", len(boxes))
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
	}
	if len(boxes) != 0 {
		t.Errorf("got %d mailboxes, want 0: a refused enumeration must not return a partial result", len(boxes))
	}
	// The refusal names the enumeration, and it does not suggest
	// narrowing a query parameter that does not exist.
	if !strings.Contains(err.Error(), "enumeration") {
		t.Errorf("err = %v, want it to name the enumeration", err)
	}
	if strings.Contains(err.Error(), "narrow") {
		t.Errorf("err = %v, want it not to suggest narrowing the enumeration", err)
	}
}

// TestServerDataBudgetArmsTheAllowancePerOperation checks that each
// operation arms its own allowance.
//
// The allowances differ because the operations differ: a summary
// listing transfers one summary per scanned message, a raw message
// fetch transfers a body, and a mailbox enumeration transfers one
// response per mailbox.
func TestServerDataBudgetArmsTheAllowancePerOperation(t *testing.T) {
	srv := &listFixtureServer{}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	cases := []struct {
		name string
		kind opKind
		want int64
	}{
		{name: "plain", kind: opPlain, want: plainDataAllowance},
		{name: "summary", kind: opSummary, want: summaryDataAllowanceFor(100)},
		{name: "message", kind: opMessage, want: messageDataAllowanceFor(1 << 20)},
		{name: "mailbox list", kind: opMailboxList, want: mailboxListDataAllowance},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := be.withClient(t.Context(), tc.kind, func(*client.Client) error {
				got := be.budget.windowLimit()
				if got != tc.want {
					t.Errorf("the armed allowance is %d, want %d", got, tc.want)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("withClient: %v", err)
			}
		})
	}
}

// TestServerDataBudgetAtTheTLSRecordBoundary checks that a flood that
// crosses the allowance while a TLS record is in flight is classified
// as a budget violation and not as a malformed response.
//
// The budget sits below the TLS layer, so it accounts the encrypted
// bytes and the allowance boundary does not fall on a record boundary.
// The classification must not depend on that alignment.
func TestServerDataBudgetAtTheTLSRecordBoundary(t *testing.T) {
	srv := &listFixtureServer{
		messages:   budgetMessages(1),
		searchUIDs: []uint32{1},
		startTLS:   true,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackendMode(t, addr, 100, true, 30*time.Second)
	defer be.Close()

	// The flood is sent while the operation runs, so it is consumed
	// under the operation's allowance and crosses it in the middle of
	// the encrypted stream.
	srv.setFloodResponses(idleFloodResponses)

	_, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatal("expected the flood to be refused")
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want errors.Is(err, mailbox.ErrServerDataBudgetExceeded)", err)
	}
	if errors.Is(err, mailbox.ErrMalformed) {
		t.Errorf("err = %v, want it not to be classified as a malformed response", err)
	}
	if errors.Is(err, mailbox.ErrResponseTooLarge) {
		t.Errorf("err = %v, want it not to be classified as a decoder-limit violation", err)
	}
}

// TestServerDataBudgetUnderConcurrentOperations checks that concurrent
// operations, which the backend serialises over one connection, are
// unaffected.
//
// The budget is armed per operation, and the per-account mutex
// serialises the operations, so the second operation must not inherit
// the first one's consumption.
func TestServerDataBudgetUnderConcurrentOperations(t *testing.T) {
	const budget = 20
	msgs := make([]listFixtureMessage, budget)
	uids := make([]uint32, budget)
	for i := range msgs {
		msgs[i] = fixtureMessage(uint32(i+1), baseTime, fmt.Sprintf("m%d", i+1))
		uids[i] = uint32(i + 1)
	}
	_, addr, cleanup := startSearchFixture(t, msgs, uids)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: %v", i, err)
		}
	}
}

// TestServerDataBudgetLeavesNoBlockedGoroutines checks that a refused
// operation leaves no goroutine behind and returns no partial result.
func TestServerDataBudgetLeavesNoBlockedGoroutines(t *testing.T) {
	const budget = 10
	srv := &listFixtureServer{
		messages:       budgetMessages(budget),
		searchUIDs:     []uint32{1},
		floodResponses: idleFloodResponses,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, budget)
	defer be.Close()

	res, err := be.SearchMessages(t.Context(), "test", "INBOX", mailbox.SearchQuery{})
	if err == nil {
		t.Fatal("expected the flood to be refused")
	}
	if len(res.Messages) != 0 {
		t.Errorf("got %d messages, want 0: a refused operation must not return a partial result", len(res.Messages))
	}
	assertNoBlockedFetchGoroutine(t)
}

// TestServerDataBudgetErrorMessagesAreOperationAppropriate checks the
// wording of the refusal.
//
// The recourse must be meaningful for the operation that was refused:
// there is no narrower query for a mailbox enumeration, and the search
// scan budget has nothing to do with a message fetch.
func TestServerDataBudgetErrorMessagesAreOperationAppropriate(t *testing.T) {
	summary := serverDataBudgetError(opSummary)
	if !errors.Is(summary, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("the summary refusal %v does not carry the sentinel", summary)
	}
	if !strings.Contains(summary.Error(), "narrow") {
		t.Errorf("the summary refusal %q does not carry the recourse", summary)
	}

	enumeration := serverDataBudgetError(opMailboxList)
	if !errors.Is(enumeration, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("the enumeration refusal %v does not carry the sentinel", enumeration)
	}
	if !strings.Contains(enumeration.Error(), "enumeration") {
		t.Errorf("the enumeration refusal %q does not name the enumeration", enumeration)
	}
	if strings.Contains(enumeration.Error(), "narrow") {
		t.Errorf("the enumeration refusal %q suggests narrowing a query that does not exist", enumeration)
	}
	if strings.Contains(enumeration.Error(), "max_list_scan_messages") {
		t.Errorf("the enumeration refusal %q names the scan budget, which has nothing to do with it", enumeration)
	}

	message := serverDataBudgetError(opMessage)
	if !errors.Is(message, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("the message refusal %v does not carry the sentinel", message)
	}
	if !strings.Contains(message.Error(), "message retrieval") {
		t.Errorf("the message refusal %q does not name the retrieval", message)
	}
	if strings.Contains(message.Error(), "max_list_scan_messages") {
		t.Errorf("the message refusal %q names the search scan budget, which has nothing to do with it", message)
	}

	idle := idleServerDataBudgetError()
	if !errors.Is(idle, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("the idle refusal %v does not carry the sentinel", idle)
	}
	if !strings.Contains(idle.Error(), "idle") {
		t.Errorf("the idle refusal %q does not name the idle allowance", idle)
	}

	// The refusals carry no content from the server's response.
	for _, err := range []error{summary, enumeration, message, idle} {
		if strings.Contains(err.Error(), "EXISTS") || strings.Contains(err.Error(), "LIST") {
			t.Errorf("the refusal %q carries content from the server's response", err)
		}
	}
}
