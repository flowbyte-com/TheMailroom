package imapclient

// The corrective regressions for the final concurrency and test-harness
// review.
//
// Each test pins one of the corrected invariants:
//
//   - the operation registration and the backend closure share one
//     synchronised transition, so a watcher can never start after Close
//     and Close can never miss a started watcher;
//   - a late completion of an abort cannot overwrite an already
//     committed terminal outcome;
//   - the fixture's shutdown is provable: the accept loop is joined
//     before the connection goroutines, and an unfinished handler is
//     reported instead of silently succeeding;
//   - the dial cancellation is driven by a controlled seam, so it does
//     not depend on an external address refusing or black-holing the
//     connection.

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// TestBackendCloseAndOperationRegistrationAreAtomic covers the
// registration and closure boundary.
//
// The first sub-case closes the backend while the registration is
// paused. The registration must then be refused: no watcher may start
// after the closure, and no command may be sent.
//
// The second sub-case lets the registration complete and then closes the
// backend. Close must observe the started watcher and join it before it
// returns.
func TestBackendCloseAndOperationRegistrationAreAtomic(t *testing.T) {
	t.Run("a registration after the closure is refused", func(t *testing.T) {
		be, fx := fixtureBackend(t, true, stageExamine, 30*time.Second, 30*time.Second)

		// Pause the registration.
		paused := make(chan struct{})
		release := make(chan struct{})
		var captured *opRun
		be.registerHook = func(op *opRun) {
			captured = op
			close(paused)
			<-release
		}

		done := make(chan error, 1)
		go func() {
			_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
			done <- err
		}()
		<-paused

		// The backend is closed while the registration is paused.
		if err := be.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		// The registration resumes after the closure.
		close(release)
		select {
		case err := <-done:
			if !errors.Is(err, mailbox.ErrBackendClosed) {
				t.Errorf("err = %v, want the backend-closed error", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the refused operation never returned")
		}

		if captured == nil {
			t.Fatal("the registration hook never ran")
		}
		// The watcher must never start. Wait for a state change and
		// require that it does not come: a watcher that starts after the
		// closure is the defect this test pins.
		started := false
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if captured.currentState() != opActive {
				started = true
				break
			}
			time.Sleep(time.Millisecond)
		}
		if started {
			t.Errorf("a watcher started after the backend was closed (state %v)", captured.currentState())
		}
		if captured.watcherDone.Load() {
			t.Error("a watcher ran after the backend was closed")
		}
		// No command may have been sent after the closure.
		for _, cmd := range fx.commandLog() {
			t.Errorf("a command was sent after the backend was closed: %s", cmd)
		}
	})

	t.Run("Close joins the watcher of a registered operation", func(t *testing.T) {
		be, fx := fixtureBackend(t, true, stageExamine, 30*time.Second, 30*time.Second)

		var captured *opRun
		registered := make(chan struct{})
		be.registerHook = func(op *opRun) {
			captured = op
		}

		done := make(chan error, 1)
		go func() {
			_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
			done <- err
		}()
		// Wait for the operation to be registered and to reach the
		// blocking stage.
		fx.waitForStage(t)
		close(registered)
		waitFor(t, 5*time.Second, "the operation to be registered", func() bool {
			return captured != nil
		})

		if err := be.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// When Close returns, the watcher has terminated.
		if !captured.watcherDone.Load() {
			t.Error("Close returned while the cancellation watcher was still running")
		}
		select {
		case err := <-done:
			if err == nil {
				t.Error("the interrupted operation reported success")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the interrupted operation never returned")
		}
	})
}

// TestConcurrentBackendCloseWaitsForTheShutdown covers the shutdown
// completion guarantee.
//
// A second Close must not return before the shutdown sequence has
// finished: the sockets must be closed, the active watcher must have
// terminated and the connection metadata must be discarded before any
// caller returns.
func TestConcurrentBackendCloseWaitsForTheShutdown(t *testing.T) {
	fx := newDeadlineFixture(t, true, stageExamine)
	// The second EXAMINE stalls, so the second operation blocks on the
	// reused connection.
	fx.setStallAt(2)
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)

	// A first operation leaves the connection reusable.
	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err != nil {
		t.Fatalf("the first ListMessages: %v", err)
	}

	// Replace the connection's socket with one whose close blocks, so
	// the shutdown is observably still in progress. The replacement must
	// happen before the next operation publishes the socket, because
	// that reference is immutable.
	be.mu.Lock()
	real := be.conn
	gate := make(chan struct{})
	be.conn = &slowCloseConn{Conn: real, gate: gate}
	be.mu.Unlock()

	// A second operation blocks on the reused connection.
	done := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
		done <- err
	}()
	fx.waitForStage(t)
	var op *opRun
	waitFor(t, 5*time.Second, "the operation to be registered", func() bool {
		be.mu.Lock()
		op = be.currentOp
		be.mu.Unlock()
		return op != nil
	})
	if op == nil {
		t.Fatal("the backend has no current operation")
	}

	// The first Close starts the shutdown and blocks in the socket close.
	first := make(chan error, 1)
	go func() { first <- be.Close() }()
	waitFor(t, 5*time.Second, "the shutdown to start", func() bool {
		return be.isClosed()
	})

	// The second Close must wait for the same shutdown.
	second := make(chan error, 1)
	go func() { second <- be.Close() }()

	select {
	case err := <-first:
		t.Fatalf("the first Close returned before the shutdown completed: %v", err)
	case err := <-second:
		t.Fatalf("the second Close returned before the shutdown completed: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// Releasing the socket lets the shutdown complete.
	close(gate)
	for i, ch := range []chan error{first, second} {
		select {
		case err := <-ch:
			if err != nil {
				t.Errorf("Close %d: %v", i+1, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Close %d did not return", i+1)
		}
	}

	// The watcher has terminated, the backend is permanently closed and
	// no later operation succeeds.
	if !op.watcherDone.Load() {
		t.Error("the watcher was still running when Close returned")
	}
	if !be.isClosed() {
		t.Error("the backend is not closed")
	}
	be.mu.Lock()
	client := be.client
	conn := be.conn
	be.mu.Unlock()
	if client != nil {
		t.Error("a client was retained after the shutdown")
	}
	if conn != nil {
		t.Error("a socket was retained after the shutdown")
	}
	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err == nil {
		t.Error("an operation after the shutdown succeeded")
	}

	select {
	case err := <-done:
		if err == nil {
			t.Error("the interrupted operation reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the interrupted operation never returned")
	}
}

// TestFixtureShutdownClosesConnectionsAcceptedDuringTheShutdown covers
// the accept-versus-shutdown overlap.
//
// The accept loop is held after a connection has been accepted but before
// it is registered, so the shutdown and an in-flight accept overlap. The
// connection must still be closed and its handler must terminate.
func TestFixtureShutdownClosesConnectionsAcceptedDuringTheShutdown(t *testing.T) {
	fx := newDeadlineFixture(t, true, stageNone)
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)

	// Hold the accept loop after the connection is accepted.
	held := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fx.setAcceptHook(func() {
		once.Do(func() {
			close(held)
			<-release
		})
	})

	// An operation connects; the fixture accepts and then holds.
	operation := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
		operation <- err
	}()
	<-held

	// The shutdown overlaps with the in-flight accept.
	shutdownDone := make(chan struct{})
	go func() {
		fx.shutdown()
		close(shutdownDone)
	}()

	// The shutdown cannot complete while the accept loop is held.
	select {
	case <-shutdownDone:
		t.Fatal("the shutdown completed while the accept loop was held")
	case <-time.After(300 * time.Millisecond):
	}

	// Releasing the accept loop lets the connection be registered; the
	// shutdown must still close it.
	close(release)
	select {
	case <-shutdownDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the shutdown did not complete")
	}

	if fx.shutdownTimedOut {
		t.Error("the fixture did not shut down within its bound")
	}
	if alive := fx.aliveGoroutines(); alive != 0 {
		t.Errorf("the fixture has %d live goroutines after the shutdown, want 0", alive)
	}

	_ = be.Close()
	select {
	case <-operation:
	case <-time.After(10 * time.Second):
		t.Fatal("the operation never returned")
	}
}

// TestAbortCompletionDoesNotOverwriteTheCommittedOutcome covers the
// terminal-state ordering.
//
// The abort's socket close blocks. While it is blocked, the operation
// commits a security violation. Releasing the close must not overwrite
// that decision, and a later cleanup must not change it either.
func TestAbortCompletionDoesNotOverwriteTheCommittedOutcome(t *testing.T) {
	op := newOpRun()
	gate := make(chan struct{})
	op.sock = &slowCloseConn{Conn: &net.TCPConn{}, gate: gate}

	ctx, cancel := context.WithCancel(context.Background())
	op.start(ctx)
	cancel()

	// The watcher begins the abort and blocks in the socket close.
	waitFor(t, 5*time.Second, "the abort to start", func() bool {
		return op.currentState() == opAborting
	})

	// The security violation is committed while the close is blocked.
	budget := mailbox.ErrServerDataBudgetExceeded
	err := op.commit(context.Background(), budget)
	if !errors.Is(err, budget) {
		t.Fatalf("commit error = %v, want the budget violation", err)
	}

	// Releasing the close lets the watcher finish.
	close(gate)
	waitFor(t, 5*time.Second, "the watcher to terminate", func() bool {
		return op.watcherDone.Load()
	})

	// The committed outcome and the terminal state are unchanged.
	if state := op.currentState(); state != opFailed {
		t.Errorf("the operation state = %v, want the committed failure", state)
	}
	if cause := op.recordedCause(); !errors.Is(cause, budget) {
		t.Errorf("the recorded cause = %v, want the budget violation", cause)
	}
	// A later cleanup cannot change the result.
	if err := op.commit(context.Background(), nil); !errors.Is(err, budget) {
		t.Errorf("the committed outcome = %v, want the budget violation", err)
	}
	if state := op.currentState(); state != opFailed {
		t.Errorf("the operation state = %v, want the committed failure", state)
	}
}

// TestDialCancellationIsDeterministic covers the dial cancellation
// through the controlled seam.
//
// The seam signals that the dial has started and then blocks until the
// context is cancelled, so the test proves that the cancellation
// interrupts the dial and that no connection is adopted or published.
func TestDialCancellationIsDeterministic(t *testing.T) {
	be := deadlineBackend(t, "127.0.0.1", 993, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)
	t.Cleanup(func() { _ = be.Close() })

	started := make(chan struct{})
	be.dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(ctx, "test", "INBOX", mailbox.ListOptions{Limit: 1})
		done <- err
	}()

	// The dial has started: the cancellation is delivered while it is in
	// progress.
	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, mailbox.ErrCanceled) {
			t.Errorf("err = %v, want a cancellation", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled to remain discoverable", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled dial never returned")
	}

	// No connection was adopted or published.
	be.mu.Lock()
	client := be.client
	conn := be.conn
	be.mu.Unlock()
	if client != nil {
		t.Error("a client was retained after a cancelled dial")
	}
	if conn != nil {
		t.Error("a socket was retained after a cancelled dial")
	}
}

// TestFixtureShutdownJoinsTheAcceptLoop proves the fixture shutdown is
// complete: the accept loop is joined, every connection handler has
// terminated, and nothing is left behind.
func TestFixtureShutdownJoinsTheAcceptLoop(t *testing.T) {
	fx := newDeadlineFixture(t, true, stageExamine)
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate, true)

	// Block a handler in EXAMINE.
	done := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
		done <- err
	}()
	fx.waitForStage(t)

	if alive := fx.aliveGoroutines(); alive != 1 {
		t.Fatalf("the fixture has %d live goroutines, want 1", alive)
	}

	fx.shutdown()
	if fx.shutdownTimedOut {
		t.Error("the fixture did not shut down within its bound")
	}
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
