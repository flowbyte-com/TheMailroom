package imapclient

// Regression tests for the Phase 3 PR 6 deadline and cancellation
// machinery.
//
// They cover the properties the design makes load-bearing: the deadline
// wrapper's transitions are linearizable and never reapply a deadline
// to a reusable idle connection; the terminal decision is made exactly
// once and is not rewritten by a later interruption; a security
// violation is not masked by the interruption it caused; and a
// cancellation reaches the socket without writing an IMAP command.
//
// The fixtures are signal-driven: the server blocks on a channel the
// test releases, so a test reaches the stage it wants to exercise
// without sleeping for it. Time is used only as an upper bound, never
// as the correctness oracle.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// cancelWhenReached cancels the context when the fixture reaches its
// blocking stage, unless the test ends first.
//
// The goroutine is joined by the test's cleanup, so it can never touch
// the context after the test has completed.
func cancelWhenReached(t *testing.T, cancel context.CancelFunc, reached <-chan struct{}) {
	t.Helper()
	stop := make(chan struct{})
	finished := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-finished
	})
	go func() {
		defer close(finished)
		select {
		case <-reached:
			cancel()
		case <-stop:
		}
	}()
}

// cancelAfter cancels the context after the delay, unless the test ends
// first.
//
// The goroutine is joined by the test's cleanup, so it can never touch
// the context after the test has completed: a late cancel would race
// with the testing framework's own bookkeeping.
func cancelAfter(t *testing.T, cancel context.CancelFunc, d time.Duration) {
	t.Helper()
	stop := make(chan struct{})
	finished := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		<-finished
	})
	go func() {
		defer close(finished)
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-stop:
		}
	}()
}

// --- the deadline wrapper ------------------------------------------------

// TestDeadlineConnClearIsNotUndoneByALateSetDeadline covers the first
// linearizability requirement: a deadline request that arrives after the
// operation finished must not reapply a deadline to a connection that is
// about to be reused.
func TestDeadlineConnClearIsNotUndoneByALateSetDeadline(t *testing.T) {
	inner := &recordingConn{}
	w := &deadlineConn{Conn: inner}

	abs := time.Now().Add(time.Minute)
	w.armOperation(abs, newOpRun())
	if inner.lastDeadline().IsZero() {
		t.Fatal("the operation deadline was not applied")
	}

	w.finishOperation()
	if !inner.lastDeadline().IsZero() {
		t.Fatal("the deadline was not cleared when the operation finished")
	}

	// A late request from the go-imap client must be ignored while the
	// connection is idle.
	if err := w.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if !inner.lastDeadline().IsZero() {
		t.Error("a late SetDeadline re-applied a deadline to the idle connection")
	}
}

// TestDeadlineConnCommandFinishedKeepsTheOperationDeadline covers the
// second requirement: a completed command relinquishes its own ceiling
// but the absolute operation deadline remains in force.
func TestDeadlineConnCommandFinishedKeepsTheOperationDeadline(t *testing.T) {
	inner := &recordingConn{}
	w := &deadlineConn{Conn: inner}

	abs := time.Now().Add(time.Minute)
	w.armOperation(abs, newOpRun())
	w.armCommand(time.Now().Add(time.Second))
	if !inner.lastDeadline().Before(abs) {
		t.Fatal("the command ceiling did not shorten the socket deadline")
	}

	w.commandFinished()
	if !inner.lastDeadline().Equal(abs) {
		t.Errorf("socket deadline after the command = %v, want the operation deadline %v", inner.lastDeadline(), abs)
	}
	if inner.lastDeadline().IsZero() {
		t.Error("the operation deadline was dropped when the command finished")
	}
}

// TestDeadlineConnZeroRequestUsesTheArmedCeiling covers the go-imap
// client's command-start signal: it passes the zero time, and the
// wrapper answers with the armed command ceiling.
func TestDeadlineConnZeroRequestUsesTheArmedCeiling(t *testing.T) {
	inner := &recordingConn{}
	w := &deadlineConn{Conn: inner}

	abs := time.Now().Add(time.Minute)
	w.armOperation(abs, newOpRun())
	ceiling := time.Now().Add(2 * time.Second)
	w.armCommand(ceiling)
	inner.deadline = time.Time{}

	if err := w.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if !inner.lastDeadline().Equal(ceiling) {
		t.Errorf("socket deadline = %v, want the armed command ceiling %v", inner.lastDeadline(), ceiling)
	}
	if inner.lastDeadline().After(abs) {
		t.Error("the command ceiling extended the operation deadline")
	}
}

// recordingConn records the deadline that was last applied to it.
type recordingConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (c *recordingConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	return nil
}

func (c *recordingConn) lastDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadline
}

// TestForkDeadlineCallIsClampedToTheOperationDeadline covers the
// command-boundary contract against the real go-imap client: the fork's
// own SetDeadline call cannot extend the operation deadline.
func TestForkDeadlineCallIsClampedToTheOperationDeadline(t *testing.T) {
	inner := &recordingConn{}
	w := &deadlineConn{Conn: inner}
	abs := time.Now().Add(time.Minute)
	w.armOperation(abs, newOpRun())

	// The go-imap client asks for a deadline far beyond the operation
	// deadline when its own timeout is set.
	if err := w.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if got := inner.lastDeadline(); got.After(abs) {
		t.Errorf("the socket deadline %v extended the operation deadline %v", got, abs)
	}
}

// --- the terminal decision ------------------------------------------------

// TestOpRunCommitIsFinalAndCannotBeRewritten covers the terminal
// decision: a committed success is not rewritten by a late cancellation,
// and a cancellation that wins is reported instead.
func TestOpRunCommitIsFinalAndCannotBeRewritten(t *testing.T) {
	t.Run("committed success survives a late cancellation", func(t *testing.T) {
		op := newOpRun()
		ctx, cancel := context.WithCancel(context.Background())
		if err := op.commit(ctx, nil); err != nil {
			t.Fatalf("commit: %v", err)
		}
		cancel()
		op.abort(context.Canceled)
		if err := op.commit(ctx, nil); err != nil {
			t.Errorf("the committed outcome was rewritten: %v", err)
		}
	})

	t.Run("a cancellation observed at the commit wins", func(t *testing.T) {
		op := newOpRun()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := op.commit(ctx, nil)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("commit error = %v, want context.Canceled", err)
		}
	})

	t.Run("the watcher wins the race", func(t *testing.T) {
		op := newOpRun()
		op.abort(errOperationCanceled)
		err := op.commit(context.Background(), nil)
		if !errors.Is(err, errOperationCanceled) {
			t.Errorf("commit error = %v, want the recorded cancellation", err)
		}
	})
}

// TestOpRunCommitKeepsTheSecurityViolationOverAnAbort covers the error
// precedence: a security violation that is already recorded is not
// replaced by the interruption it caused.
//
// The watcher is forced into the aborted state first, so the final
// selection happens after the interruption, exactly as it does when a
// cancellation races with an aggregate-budget violation.
func TestOpRunCommitKeepsTheSecurityViolationOverAnAbort(t *testing.T) {
	op := newOpRun()
	// The watcher interrupted the I/O.
	op.abort(errOperationCanceled)
	if op.currentState() != opAborted {
		t.Fatalf("the operation state = %v, want the aborted state", op.currentState())
	}
	budget := fmt.Errorf("%w: the server sent more protocol data than the operation allows",
		mailbox.ErrServerDataBudgetExceeded)
	err := op.commit(context.Background(), budget)
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("commit error = %v, want the aggregate budget violation", err)
	}
	if op.recordedCause() != budget {
		t.Errorf("the recorded cause = %v, want the budget violation", op.recordedCause())
	}
}

// TestOpRunPublishRefusesAfterCancellation covers the cancellation during
// the dial: a socket cannot be published once the operation is aborted.
func TestOpRunPublishRefusesAfterCancellation(t *testing.T) {
	op := newOpRun()
	op.abort(errOperationCanceled)
	if op.publish(&net.TCPConn{}) {
		t.Error("a socket was published after the operation was aborted")
	}
	if op.isActive() {
		t.Error("the operation is still active after the abort")
	}
}

// --- the classification ---------------------------------------------------

// TestDeadlineConnClassifiesTheExpiredDeadline covers the identity of an
// expired deadline: a per-command ceiling and the absolute operation
// deadline stay distinguishable, and a genuine EOF is never a timeout.
func TestDeadlineConnClassifiesTheExpiredDeadline(t *testing.T) {
	t.Run("command ceiling", func(t *testing.T) {
		op := newOpRun()
		w := &deadlineConn{Conn: &timeoutConn{}, op: op}
		w.armed = true
		w.abs = time.Now().Add(time.Minute)
		cmd := time.Now().Add(time.Second)
		w.cmd = cmd
		w.applied = cmd
		w.observeDeadline(&net.OpError{Op: "read", Err: timeoutError{}})
		if !errors.Is(op.recordedCause(), errCommandTimeout) {
			t.Errorf("cause = %v, want the command timeout", op.recordedCause())
		}
	})

	t.Run("operation deadline", func(t *testing.T) {
		op := newOpRun()
		w := &deadlineConn{Conn: &timeoutConn{}, op: op}
		w.armed = true
		abs := time.Now().Add(time.Second)
		w.abs = abs
		w.cmd = time.Now().Add(time.Minute)
		w.applied = abs
		w.observeDeadline(&net.OpError{Op: "read", Err: timeoutError{}})
		if !errors.Is(op.recordedCause(), errOperationTimeout) {
			t.Errorf("cause = %v, want the operation timeout", op.recordedCause())
		}
	})

	t.Run("a genuine EOF is not a timeout", func(t *testing.T) {
		op := newOpRun()
		w := &deadlineConn{Conn: &timeoutConn{}, op: op}
		w.armed = true
		w.abs = time.Now().Add(time.Second)
		w.applied = w.abs
		w.observeDeadline(&net.OpError{Op: "read", Err: errors.New("EOF")})
		if op.recordedCause() != nil {
			t.Errorf("cause = %v, want none for a genuine EOF", op.recordedCause())
		}
	})
}

// timeoutError is a net.Error that reports a timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// timeoutConn is a connection whose reads and writes always time out.
type timeoutConn struct {
	net.Conn
}

func (c *timeoutConn) Read(p []byte) (int, error) {
	return 0, &net.OpError{Op: "read", Err: timeoutError{}}
}

func (c *timeoutConn) Write(p []byte) (int, error) {
	return 0, &net.OpError{Op: "write", Err: timeoutError{}}
}

// --- the derived deadlines ------------------------------------------------

// TestTransferAllowanceIsPositiveAtEveryRate covers the transfer
// allowance arithmetic: a positive byte allowance and rate always yield
// a positive duration, at the minimum, default and maximum accepted
// rates, for every operation kind.
func TestTransferAllowanceIsPositiveAtEveryRate(t *testing.T) {
	rates := []int{
		config.MinIMAPTransferRate,
		config.DefaultIMAPTransferRate,
		config.MaxIMAPTransferRate,
	}
	kinds := []opKind{opPlain, opList, opSearch, opMessage, opMailboxList}
	for _, rate := range rates {
		for _, kind := range kinds {
			be := deadlineBackend(t, "127.0.0.1", 1, time.Second, time.Second, rate, true)
			got := be.transferAllowance(kind)
			if got <= 0 {
				t.Errorf("rate %d, kind %d: the transfer allowance is %s, want a positive duration", rate, kind, got)
			}
			// A bulk transfer must not collapse into an instant
			// deadline at the fastest accepted rate.
			if kind == opSearch || kind == opMailboxList {
				if got < 100*time.Millisecond {
					t.Errorf("rate %d, kind %d: the transfer allowance is %s, want a usable duration", rate, kind, got)
				}
			}
			// The fraction of a second is preserved: at the fastest
			// accepted rate the enumeration allowance is a quarter of
			// a second, and it must not be rounded to a whole second.
			if rate == config.MaxIMAPTransferRate && kind == opMailboxList {
				if got >= time.Second {
					t.Errorf("the enumeration transfer allowance at the fastest rate is %s, want the fractional part preserved", got)
				}
			}
		}
	}
}

// TestTransferAllowanceKeepsTheDocumentedDefaults checks that the
// documented default durations are exact.
func TestTransferAllowanceKeepsTheDocumentedDefaults(t *testing.T) {
	be := deadlineBackend(t, "127.0.0.1", 1, config.DefaultIMAPOperationAllowance,
		config.DefaultIMAPControlCommandTimeout, config.DefaultIMAPTransferRate, true)
	cases := []struct {
		kind opKind
		want time.Duration
	}{
		{opList, 629 * time.Second},
		{opSearch, 629 * time.Second},
		{opMessage, 416 * time.Second},
		{opMailboxList, 64 * time.Second},
	}
	for _, tc := range cases {
		if got := be.transferAllowance(tc.kind); got != tc.want {
			t.Errorf("the transfer allowance for kind %d = %s, want %s", tc.kind, got, tc.want)
		}
	}
}

// TestCommandCeilingSelection covers the classification of the commands:
// a control command takes the control ceiling and a bulk transfer takes
// the transfer ceiling, and the enumeration's ceiling fits inside its
// operation.
func TestCommandCeilingSelection(t *testing.T) {
	be := deadlineBackend(t, "127.0.0.1", 1, config.DefaultIMAPOperationAllowance,
		config.DefaultIMAPControlCommandTimeout, config.DefaultIMAPTransferRate, true)
	control := be.controlCeiling()
	if control != 30*time.Second {
		t.Errorf("control ceiling = %s, want 30s", control)
	}
	transfer := be.transferAllowance(opList)
	if transfer <= control {
		t.Errorf("the transfer ceiling %s is not larger than the control ceiling %s", transfer, control)
	}
	enumeration := be.transferAllowance(opMailboxList)
	if enumeration <= 0 {
		t.Errorf("the enumeration transfer ceiling is %s, want a positive duration", enumeration)
	}
	// The effective operation deadline is clamped by the maximum, so it
	// can never exceed it.
	if got := time.Until(be.operationDeadline(opMailboxList)); got > be.maxOperationTimeout() {
		t.Errorf("the enumeration operation deadline is %s, want at most %s", got, be.maxOperationTimeout())
	}
}

// --- the end-to-end behaviour --------------------------------------------

// TestOperationDeadlineIsReportedAsATimeout covers the classification
// through the real go-imap reader: a server that never answers EXAMINE
// makes the operation deadline expire, and the failure is reported as a
// timeout rather than as a transport failure.
func TestOperationDeadlineIsReportedAsATimeout(t *testing.T) {
	be, _ := fixtureBackend(t, true, stageExamine, 200*time.Millisecond, 200*time.Millisecond)

	_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err == nil {
		t.Fatal("expected an error for a server that never answers EXAMINE")
	}
	if !errors.Is(err, mailbox.ErrTimeout) {
		t.Errorf("err = %v, want a timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded to remain discoverable", err)
	}
}

// TestCommandDeadlineIsDistinguishableFromTheOperationDeadline covers
// the two timeout identities: a command that exceeds its own ceiling is
// a command timeout, while the operation deadline is the outer bound.
func TestCommandDeadlineIsDistinguishableFromTheOperationDeadline(t *testing.T) {
	be, _ := fixtureBackend(t, true, stageExamine, 500*time.Millisecond, 200*time.Millisecond)

	_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err == nil {
		t.Fatal("expected an error for a server that never answers EXAMINE")
	}
	if !errors.Is(err, mailbox.ErrTimeout) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if !strings.Contains(err.Error(), "command deadline") {
		t.Errorf("err = %v, want the command deadline to be named", err)
	}
}

// TestCallerCancellationIsReportedAsCancellation covers the caller's
// cancellation: it is a cancellation, not a timeout, and it reaches the
// blocked command.
func TestCallerCancellationIsReportedAsCancellation(t *testing.T) {
	be, fx := fixtureBackend(t, true, stageExamine, 30*time.Second, 30*time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	cancelWhenReached(t, cancel, fx.reached)
	_, err := be.ListMessages(ctx, "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err == nil {
		t.Fatal("expected an error after the caller cancelled")
	}
	if !errors.Is(err, mailbox.ErrCanceled) {
		t.Errorf("err = %v, want a cancellation", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled to remain discoverable", err)
	}
}

// TestReusedSessionCancellationInterruptsTheCommand covers the reuse
// path: a cancellation on a reused connection interrupts the blocked
// command instead of waiting for the command or operation deadline.
//
// The sequence is: a successful operation leaves the connection in the
// reusable idle state; a second operation on the same connection stalls;
// the caller cancels; the operation must fail promptly with a
// cancellation, the connection must be discarded, and the next operation
// must succeed on a fresh connection.
func TestReusedSessionCancellationInterruptsTheCommand(t *testing.T) {
	for _, tlsMode := range []bool{true, false} {
		name := "implicit TLS"
		if !tlsMode {
			name = "STARTTLS"
		}
		t.Run(name, func(t *testing.T) {
			fx := newDeadlineFixture(t, tlsMode, stageExamine)
			// The first EXAMINE is served normally; the second one
			// stalls, on the reused connection.
			fx.setStallAt(2)
			host, port := fx.addr()
			be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, tlsMode)
			t.Cleanup(func() { _ = be.Close() })

			// 1-2: the first operation succeeds and leaves the
			// connection reusable.
			res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
			if err != nil {
				t.Fatalf("the first ListMessages: %v", err)
			}
			if res.UIDValidity == 0 {
				t.Fatal("the first operation returned no UIDVALIDITY")
			}
			if got := fx.connectionCount(); got != 1 {
				t.Fatalf("the server saw %d connections, want 1", got)
			}

			// 3-4: the second operation reuses the connection and
			// stalls in EXAMINE.
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := be.ListMessages(ctx, "test", "INBOX", mailbox.ListOptions{Limit: 1})
				done <- err
			}()
			fx.waitForStage(t)

			// 5-6: the cancellation must interrupt the blocked command
			// promptly, well before the command or operation deadline.
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				elapsed := time.Since(start)
				if !errors.Is(err, mailbox.ErrCanceled) {
					t.Errorf("err = %v, want a cancellation", err)
				}
				if elapsed > 5*time.Second {
					t.Errorf("the cancellation took %s, want it to interrupt the command", elapsed)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the cancelled operation never returned")
			}

			// 7: the cancelled operation reused the existing connection
			// and did not dial a second one; the connection was then
			// discarded, and the next operation succeeds on a fresh
			// connection.
			if got := fx.connectionCount(); got != 1 {
				t.Errorf("the server saw %d connections after the cancelled operation, want 1: the operation must have reused the connection", got)
			}
			be.mu.Lock()
			client := be.client
			be.mu.Unlock()
			if client != nil {
				t.Error("the connection was retained after the cancellation")
			}
			if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err != nil {
				t.Fatalf("the operation after the cancellation: %v", err)
			}
			if got := fx.connectionCount(); got != 2 {
				t.Errorf("the server saw %d connections, want 2: the next operation must dial a fresh connection", got)
			}
		})
	}
}

// TestCancellationDuringDialPublishesNothing covers a cancellation while
// the connection is being established: the operation fails with a
// cancellation and no socket is adopted.
func TestCancellationDuringDialPublishesNothing(t *testing.T) {
	// A server that accepts the TCP connection and never completes the
	// TLS handshake: the dial succeeds and the handshake stalls.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Never speak TLS: the handshake stalls until the
				// socket is closed.
				buf := make([]byte, 1)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)
	t.Cleanup(func() { _ = be.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	cancelAfter(t, cancel, 200*time.Millisecond)
	_, err = be.ListMessages(ctx, "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err == nil {
		t.Fatal("expected an error after the caller cancelled")
	}
	if !errors.Is(err, mailbox.ErrCanceled) {
		t.Errorf("err = %v, want a cancellation", err)
	}
	// The connection was never adopted.
	be.mu.Lock()
	client := be.client
	be.mu.Unlock()
	if client != nil {
		t.Error("a client was retained after a cancelled dial")
	}
}

// TestDialCancellation is an optional environment smoke test for the dial
// cancellation.
//
// The address is in the documentation test range, which no host routes. A
// network stack that routes or refuses it decides the outcome: the
// connect may block until the cancellation, or it may be refused
// immediately. The second case is not evidence about the code, so it is
// reported as a skip rather than a failure.
//
// The deterministic proof of the dial cancellation is
// TestDialCancellationIsDeterministic, which drives the dial through the
// controlled seam.
func TestDialCancellation(t *testing.T) {
	host, portStr, _ := net.SplitHostPort(dialCancellationAddress)
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)
	t.Cleanup(func() { _ = be.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelAfter(t, cancel, 300*time.Millisecond)
	start := time.Now()
	_, err := be.ListMessages(ctx, "test", "INBOX", mailbox.ListOptions{Limit: 1})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error after the caller cancelled")
	}
	if elapsed > 20*time.Second {
		t.Errorf("the cancelled dial took %s, want it to be interrupted", elapsed)
	}
	if ctx.Err() == nil {
		t.Skipf("the environment refused the black-hole address before the cancellation: %v", err)
	}
	if !errors.Is(err, mailbox.ErrCanceled) && !errors.Is(err, mailbox.ErrTimeout) {
		t.Errorf("err = %v, want a cancellation or a timeout", err)
	}
}

// TestQueueExpiryDoesNotTouchTheActiveOperation covers the queue
// allowance: a second operation fails fast while the first one is in
// progress, and the first one is unaffected.
func TestQueueExpiryDoesNotTouchTheActiveOperation(t *testing.T) {
	be, fx := fixtureBackend(t, true, stageExamine, 300*time.Millisecond, 5*time.Second)

	first := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
		first <- err
	}()
	fx.waitForStage(t)

	start := time.Now()
	_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected the queued operation to fail")
	}
	if !errors.Is(err, mailbox.ErrTimeout) {
		t.Errorf("err = %v, want a timeout for the queue wait", err)
	}
	if !strings.Contains(err.Error(), "queue") {
		t.Errorf("err = %v, want the queue phase to be named", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the queued operation waited %s, want it bounded by the queue allowance", elapsed)
	}

	// The first operation still owns the connection: releasing the
	// server lets it finish.
	fx.releaseStage()
	select {
	case err := <-first:
		t.Logf("the first operation ended with %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the first operation never returned")
	}
}

// TestBackendCloseInterruptsEveryStage covers the shutdown: the backend
// is permanently closed, the blocked operation is interrupted, and no
// IMAP CLOSE or LOGOUT is written.
func TestBackendCloseInterruptsEveryStage(t *testing.T) {
	stages := []struct {
		name        string
		implicitTLS bool
		stage       deadlineStage
	}{
		{"implicit TLS greeting", true, stageGreeting},
		{"implicit TLS login", true, stageLogin},
		{"implicit TLS examine", true, stageExamine},
		{"implicit TLS fetch", true, stageFetch},
		{"STARTTLS handshake", false, stageStartTLS},
		{"STARTTLS login", false, stageLogin},
		{"STARTTLS examine", false, stageExamine},
		{"STARTTLS fetch", false, stageFetch},
	}
	for _, tc := range stages {
		t.Run(tc.name, func(t *testing.T) {
			be, fx := fixtureBackend(t, tc.implicitTLS, tc.stage, 30*time.Second, 30*time.Second)

			done := make(chan error, 1)
			go func() {
				_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
				done <- err
			}()
			fx.waitForStage(t)

			if err := be.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			select {
			case err := <-done:
				if err == nil {
					t.Error("the interrupted operation reported success")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Close did not interrupt the operation")
			}

			// The backend is permanently closed.
			if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err == nil {
				t.Error("an operation after Close succeeded")
			}

			// No IMAP CLOSE or LOGOUT was written.
			for _, cmd := range fx.commandLog() {
				if cmd == "CLOSE" || cmd == "LOGOUT" {
					t.Errorf("the shutdown wrote an IMAP %s command", cmd)
				}
			}
		})
	}
}

// slowCloseConn is a connection whose Close blocks until the test
// releases it. It lets a test observe whether a shutdown waits for the
// cancellation watcher: the watcher closes this socket, so the watcher
// is still running while the close is blocked.
type slowCloseConn struct {
	net.Conn
	gate chan struct{}
}

func (c *slowCloseConn) Close() error {
	<-c.gate
	return c.Conn.Close()
}

// TestBackendCloseJoinsTheWatcher establishes the shutdown ordering: when
// Close returns, the cancellation watcher of the interrupted operation
// has already terminated, so a late callback cannot reach a reusable or
// replacement connection.
//
// The watcher's socket close is blocked by the test, so the watcher is
// provably still running while Close is called: without the join, Close
// returns while the watcher is running, and the test observes it.
func TestBackendCloseJoinsTheWatcher(t *testing.T) {
	fx := newDeadlineFixture(t, true, stageExamine)
	// The second EXAMINE stalls, so the second operation blocks on the
	// reused connection.
	fx.setStallAt(2)
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)
	t.Cleanup(func() { _ = be.Close() })

	// A first operation leaves the connection reusable.
	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err != nil {
		t.Fatalf("the first ListMessages: %v", err)
	}

	// Replace the connection's socket with one whose close blocks, so
	// the watcher is observably still running.
	be.mu.Lock()
	real := be.conn
	gate := make(chan struct{})
	be.conn = &slowCloseConn{Conn: real, gate: gate}
	be.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(ctx, "test", "INBOX", mailbox.ListOptions{Limit: 1})
		done <- err
	}()
	fx.waitForStage(t)

	be.mu.Lock()
	op := be.currentOp
	be.mu.Unlock()
	if op == nil {
		t.Fatal("the backend has no current operation")
	}

	// The cancellation makes the watcher abort, and the abort blocks in
	// the socket close.
	cancel()
	waitFor(t, 5*time.Second, "the watcher to start", func() bool {
		return op.currentState() != opActive
	})

	closed := make(chan error, 1)
	go func() { closed <- be.Close() }()

	// The watcher is blocked in the socket close, so Close must not
	// return yet.
	select {
	case err := <-closed:
		t.Fatalf("Close returned while the cancellation watcher was still running: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Releasing the socket lets the watcher finish, and Close returns
	// only after it has.
	close(gate)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	if !op.watcherDone.Load() {
		t.Error("Close returned while the cancellation watcher was still running")
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted operation never returned")
	}
}

// TestSuccessfulOperationClearsTheDeadlineBeforeIdleReuse covers the
// idle reuse: after a successful operation the connection carries no
// deadline, so an expired command ceiling cannot terminate it.
func TestSuccessfulOperationClearsTheDeadlineBeforeIdleReuse(t *testing.T) {
	be, fx := fixtureBackend(t, true, stageNone, 30*time.Second, 200*time.Millisecond)

	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err != nil {
		t.Fatalf("ListMessages: %v", err)
	}

	be.mu.Lock()
	w := be.deadlines
	be.mu.Unlock()
	if w == nil {
		t.Fatal("the connection has no deadline wrapper")
	}
	w.mu.Lock()
	armed := w.armed
	applied := w.applied
	w.mu.Unlock()
	if armed {
		t.Error("the deadline wrapper is still armed after a successful operation")
	}
	if !applied.IsZero() {
		t.Error("a deadline is still applied to the idle connection")
	}

	// The connection is reused rather than replaced.
	before := fx.connectionCount()
	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err != nil {
		t.Fatalf("the second ListMessages: %v", err)
	}
	if got := fx.connectionCount(); got != before {
		t.Errorf("the server saw %d connections, want the idle connection to be reused", got)
	}
}

// TestFetchStreamingTimeoutProducesNoPartialResult covers the streaming
// case: a FETCH that stalls after a partial response ends with a
// timeout and no partial listing.
func TestFetchStreamingTimeoutProducesNoPartialResult(t *testing.T) {
	for _, tlsMode := range []bool{true, false} {
		name := "implicit TLS"
		if !tlsMode {
			name = "STARTTLS"
		}
		t.Run(name, func(t *testing.T) {
			be, _ := fixtureBackend(t, tlsMode, stageFetch, 300*time.Millisecond, 300*time.Millisecond)

			res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
			if err == nil {
				t.Fatal("expected an error for a stalled FETCH")
			}
			if !errors.Is(err, mailbox.ErrTimeout) {
				t.Errorf("err = %v, want a timeout", err)
			}
			if len(res.Messages) != 0 {
				t.Errorf("got %d messages, want no partial result", len(res.Messages))
			}
		})
	}
}

// TestBudgetViolationIsNotReclassifiedAsATimeout covers the precedence:
// the aggregate-byte violation keeps its own classification.
func TestBudgetViolationIsNotReclassifiedAsATimeout(t *testing.T) {
	// The budget fixture floods before the login.
	srv := &listFixtureServer{
		messages:       budgetMessages(1),
		searchUIDs:     []uint32{1},
		floodResponses: idleFloodResponses,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()
	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err == nil {
		t.Fatal("expected an error for a flood before the login")
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want the aggregate budget violation", err)
	}
	if errors.Is(err, mailbox.ErrTimeout) {
		t.Error("the budget violation was reclassified as a timeout")
	}
}

// TestBudgetViolationSurvivesConcurrentCancellation covers the same
// precedence when a cancellation follows the violation: the violation is
// the reported error, because it is the cause that matters.
func TestBudgetViolationSurvivesConcurrentCancellation(t *testing.T) {
	srv := &listFixtureServer{
		messages:       budgetMessages(1),
		searchUIDs:     []uint32{1},
		floodResponses: idleFloodResponses,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()
	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The cancellation arrives after the flood has been read, so the
	// aggregate-byte violation is already recorded when the operation
	// fails. The violation must be the reported error: a later
	// cancellation must not replace it.
	cancelAfter(t, cancel, 300*time.Millisecond)
	_, err := be.ListMessages(ctx, "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err == nil {
		t.Fatal("expected an error for a flood before the login")
	}
	if !errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want the aggregate budget violation", err)
	}
}

// TestTransportFailureIsNotATimeout covers the classification of a real
// reader failure: a server that closes the connection is a transport
// failure, not a timeout.
func TestTransportFailureIsNotATimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and close immediately: the client sees a
			// disconnect during the greeting.
			_ = c.Close()
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)
	t.Cleanup(func() { _ = be.Close() })

	_, err = be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err == nil {
		t.Fatal("expected an error for a server that disconnects")
	}
	if errors.Is(err, mailbox.ErrTimeout) {
		t.Errorf("err = %v, want a transport failure rather than a timeout", err)
	}
	if errors.Is(err, mailbox.ErrServerDataBudgetExceeded) {
		t.Errorf("err = %v, want a transport failure rather than a budget violation", err)
	}
}

// TestReconnectionObservesAFreshUIDValidity covers the reconnection
// after a cancellation: the next operation uses a new connection and
// observes the server's new UIDVALIDITY.
func TestReconnectionObservesAFreshUIDValidity(t *testing.T) {
	fx := newDeadlineFixture(t, true, stageExamine)
	fx.setStallAt(2)
	fx.varyUIDValidityOn()
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)
	t.Cleanup(func() { _ = be.Close() })

	first, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("the first ListMessages: %v", err)
	}
	if first.UIDValidity == 0 {
		t.Fatal("the first operation returned no UIDVALIDITY")
	}

	// The second operation stalls on the reused connection and is
	// cancelled.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(ctx, "test", "INBOX", mailbox.ListOptions{Limit: 1})
		done <- err
	}()
	fx.waitForStage(t)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled operation never returned")
	}

	// The next operation reconnects and sees the new UIDVALIDITY.
	third, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("the operation after the cancellation: %v", err)
	}
	if third.UIDValidity == first.UIDValidity {
		t.Errorf("the UIDVALIDITY is %d after the reconnection, want a fresh value", third.UIDValidity)
	}
}

// TestFixtureShutdownTerminatesEveryGoroutine proves that the fixture
// releases its resources: after the shutdown no connection goroutine is
// left behind and no socket remains open.
func TestFixtureShutdownTerminatesEveryGoroutine(t *testing.T) {
	fx := newDeadlineFixture(t, true, stageExamine)
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)

	// Block the fixture in EXAMINE.
	done := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
		done <- err
	}()
	fx.waitForStage(t)

	if alive := fx.aliveGoroutines(); alive != 1 {
		t.Fatalf("the fixture has %d live goroutines, want 1", alive)
	}

	// The shutdown releases the stage, closes the socket and joins the
	// goroutines.
	fx.shutdown()
	if alive := fx.aliveGoroutines(); alive != 0 {
		t.Errorf("the fixture has %d live goroutines after the shutdown, want 0", alive)
	}
	_ = be.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the blocked operation never returned")
	}
}
