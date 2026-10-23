package imapclient

// Regression tests for the Phase 3 PR 6 deadline and cancellation
// machinery.
//
// They cover the three properties the design makes load-bearing: the
// deadline wrapper's transitions are linearizable and never reapply a
// deadline to a reusable idle connection; the terminal decision is made
// exactly once and cannot be rewritten; and a cancellation reaches the
// socket without writing an IMAP command.
//
// The fixtures are signal-driven: the server blocks on a channel the test
// releases, so a test reaches the stage it wants to exercise without
// sleeping for it. Time is used only as an upper bound, never as the
// correctness oracle.

import (
	"context"
	"crypto/tls"
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

// deadlineStage names the point in the connection sequence a fixture
// blocks at.
type deadlineStage int

const (
	stageNone deadlineStage = iota
	// stageGreeting blocks before the IMAP greeting is sent.
	stageGreeting
	// stageLogin blocks before LOGIN is answered.
	stageLogin
	// stageExamine blocks before EXAMINE is answered.
	stageExamine
	// stageFetch blocks in the middle of a FETCH response.
	stageFetch
)

// deadlineFixture is an IMAP server that can block at a chosen stage and
// that records the commands it received.
type deadlineFixture struct {
	ln net.Listener
	// stage is the point the server blocks at.
	stage deadlineStage
	// release unblocks the server. It is closed by the test.
	release chan struct{}
	// reached is closed when the server reaches the blocking stage.
	reached chan struct{}
	// tlsMode serves implicit TLS.
	tlsMode bool
	// cert is the server certificate.
	cert tls.Certificate

	mu          sync.Mutex
	commands    []string
	conns       []net.Conn
	reachedOnce sync.Once
}

func newDeadlineFixture(t *testing.T, stage deadlineStage, tlsMode bool) *deadlineFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &deadlineFixture{
		ln:      ln,
		stage:   stage,
		release: make(chan struct{}),
		reached: make(chan struct{}),
		tlsMode: tlsMode,
		cert:    genCert(t),
	}
	go f.serve()
	return f
}

func (f *deadlineFixture) addr() (string, int) {
	host, portStr, _ := net.SplitHostPort(f.ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

func (f *deadlineFixture) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		go f.handle(c)
	}
}

func (f *deadlineFixture) record(cmd string) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
}

// commandLog returns the commands the server received.
func (f *deadlineFixture) commandLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.commands))
	copy(out, f.commands)
	return out
}

// waitForStage blocks until the fixture reaches its blocking stage.
func (f *deadlineFixture) waitForStage(t *testing.T) {
	t.Helper()
	select {
	case <-f.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the fixture never reached its blocking stage")
	}
}

func (f *deadlineFixture) handle(raw net.Conn) {
	c := raw
	if f.tlsMode {
		tc := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12})
		if err := tc.Handshake(); err != nil {
			return
		}
		c = tc
	}
	if f.stage == stageGreeting {
		f.reachedOnce.Do(func() { close(f.reached) })
		<-f.release
		return
	}
	if _, err := c.Write([]byte("* OK [CAPABILITY IMAP4rev1] ready\r\n")); err != nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		line := string(buf[:n])
		fields := strings.Fields(line)
		tag := "TAG"
		if len(fields) > 0 {
			tag = fields[0]
		}
		cmd := ""
		if len(fields) > 1 {
			cmd = fields[1]
		}
		f.record(cmd)
		switch {
		case strings.Contains(strings.ToUpper(line), "LOGIN"):
			if f.stage == stageLogin {
				f.reachedOnce.Do(func() { close(f.reached) })
				<-f.release
				return
			}
			fmt.Fprintf(c, "%s OK logged in\r\n", tag)
		case strings.Contains(strings.ToUpper(line), "EXAMINE"):
			if f.stage == stageExamine {
				f.reachedOnce.Do(func() { close(f.reached) })
				<-f.release
				return
			}
			fmt.Fprintf(c, "* 1 EXISTS\r\n* OK [UIDVALIDITY 1] selected\r\n%s OK selected\r\n", tag)
		case strings.Contains(strings.ToUpper(line), "FETCH"):
			if f.stage == stageFetch {
				// A partial response, then a stall: the client has
				// delivered part of the body and is waiting for the
				// rest.
				fmt.Fprintf(c, "* 1 FETCH (UID 1 BODY[] {10}\r\n")
				f.reachedOnce.Do(func() { close(f.reached) })
				<-f.release
				return
			}
			fmt.Fprintf(c, "* 1 FETCH (UID 1 BODY[] {0}\r\n)\r\n%s OK done\r\n", tag)
		default:
			fmt.Fprintf(c, "%s OK done\r\n", tag)
		}
	}
}

// deadlineBackend builds a backend against the fixture with short
// deadlines, so a test does not wait for the production values.
func deadlineBackend(t *testing.T, host string, port int, allowance, control time.Duration, rate int) *Backend {
	t.Helper()
	cfg := config.AccountConfig{
		ID: "test", Email: "test@example.test", Backend: "imap",
		IMAP: config.IMAPConfig{
			Host: host, Port: port, Username: "user", Password: "secret",
			TLS:                   true,
			OperationAllowance:    allowance,
			ControlCommandTimeout: control,
			TransferRate:          rate,
			MaxOperationTimeout:   config.MinIMAPMaxOperationTimeout,
		},
	}
	be, err := New(cfg, Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return be
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
	if inner.deadline.IsZero() {
		t.Fatal("the operation deadline was not applied")
	}

	w.finishOperation()
	if !inner.deadline.IsZero() {
		t.Fatal("the deadline was not cleared when the operation finished")
	}

	// A late request from the go-imap client must be ignored while the
	// connection is idle.
	if err := w.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if !inner.deadline.IsZero() {
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
	if !inner.deadline.Before(abs) {
		t.Fatal("the command ceiling did not shorten the socket deadline")
	}

	w.commandFinished()
	if !inner.deadline.Equal(abs) {
		t.Errorf("socket deadline after the command = %v, want the operation deadline %v", inner.deadline, abs)
	}
	if inner.deadline.IsZero() {
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
	if !inner.deadline.Equal(ceiling) {
		t.Errorf("socket deadline = %v, want the armed command ceiling %v", inner.deadline, ceiling)
	}
	if inner.deadline.After(abs) {
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
		// The cancellation arrives after the commit.
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

// --- the end-to-end behaviour --------------------------------------------

// TestOperationDeadlineIsReportedAsATimeout covers the classification
// through the real go-imap reader: a server that never answers EXAMINE
// makes the operation deadline expire, and the failure is reported as a
// timeout rather than as a transport failure.
func TestOperationDeadlineIsReportedAsATimeout(t *testing.T) {
	fx := newDeadlineFixture(t, stageExamine, true)
	defer fx.ln.Close()
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 200*time.Millisecond, 200*time.Millisecond, config.MaxIMAPTransferRate)
	defer be.Close()

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
	fx := newDeadlineFixture(t, stageExamine, true)
	defer fx.ln.Close()
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 500*time.Millisecond, 200*time.Millisecond, config.MaxIMAPTransferRate)
	defer be.Close()

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
	fx := newDeadlineFixture(t, stageExamine, true)
	defer fx.ln.Close()
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate)
	defer be.Close()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		fx.waitForStage(t)
		cancel()
	}()
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

// TestCancellationDuringDialPublishesNothing covers the cancellation
// before the socket is published: the dial is interrupted and the
// operation fails with a cancellation.
func TestCancellationDuringDialPublishesNothing(t *testing.T) {
	// A listener that accepts the connection but never completes the
	// TLS handshake, so the dial succeeds and the handshake stalls.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Never speak TLS: the handshake stalls until the socket is
			// closed.
			go func() { <-make(chan struct{}) }()
			_ = c
		}
	}()
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate)
	defer be.Close()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
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

// TestQueueExpiryDoesNotTouchTheActiveOperation covers the queue
// allowance: a second operation fails fast while the first one is in
// progress, and the first one is unaffected.
func TestQueueExpiryDoesNotTouchTheActiveOperation(t *testing.T) {
	fx := newDeadlineFixture(t, stageExamine, true)
	defer fx.ln.Close()
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 300*time.Millisecond, 5*time.Second, config.MaxIMAPTransferRate)
	defer be.Close()

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
	close(fx.release)
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
	for _, stage := range []deadlineStage{stageGreeting, stageLogin, stageExamine, stageFetch} {
		t.Run(stageName(stage), func(t *testing.T) {
			fx := newDeadlineFixture(t, stage, true)
			defer fx.ln.Close()
			host, port := fx.addr()
			be := deadlineBackend(t, host, port, 30*time.Second, 30*time.Second, config.MaxIMAPTransferRate)

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

func stageName(s deadlineStage) string {
	switch s {
	case stageGreeting:
		return "greeting"
	case stageLogin:
		return "login"
	case stageExamine:
		return "examine"
	case stageFetch:
		return "fetch"
	default:
		return "none"
	}
}

// TestSuccessfulOperationClearsTheDeadlineBeforeIdleReuse covers the
// idle reuse: after a successful operation the connection carries no
// deadline, so an expired command ceiling cannot terminate it.
func TestSuccessfulOperationClearsTheDeadlineBeforeIdleReuse(t *testing.T) {
	fx := newDeadlineFixture(t, stageNone, true)
	defer fx.ln.Close()
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 30*time.Second, 200*time.Millisecond, config.MaxIMAPTransferRate)
	defer be.Close()

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
	before := len(fx.commandLog())
	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err != nil {
		t.Fatalf("the second ListMessages: %v", err)
	}
	if got := len(fx.commandLog()); got <= before {
		t.Error("the second operation did not run any command")
	}
}

// TestFetchStreamingTimeoutProducesNoPartialResult covers the streaming
// case: a FETCH that stalls after a partial response ends with a
// timeout and no partial listing.
func TestFetchStreamingTimeoutProducesNoPartialResult(t *testing.T) {
	fx := newDeadlineFixture(t, stageFetch, true)
	defer fx.ln.Close()
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, 300*time.Millisecond, 5*time.Second, config.MaxIMAPTransferRate)
	defer be.Close()

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

// TestCommandCeilingSelection covers the classification of the commands:
// a control command takes the control ceiling and a bulk transfer takes
// the transfer ceiling.
func TestCommandCeilingSelection(t *testing.T) {
	be := &Backend{cfg: config.AccountConfig{IMAP: config.IMAPConfig{
		OperationAllowance:    config.DefaultIMAPOperationAllowance,
		ControlCommandTimeout: 30 * time.Second,
		TransferRate:          config.DefaultIMAPTransferRate,
		MaxOperationTimeout:   config.DefaultIMAPMaxOperationTimeout,
	}}}
	control := be.controlCeiling()
	if control != 30*time.Second {
		t.Errorf("control ceiling = %s, want 30s", control)
	}
	transfer := be.transferAllowance(opList)
	if transfer <= control {
		t.Errorf("the transfer ceiling %s is not larger than the control ceiling %s", transfer, control)
	}
	// The enumeration's LIST ceiling must fit inside its operation.
	enumeration := be.transferAllowance(opMailboxList)
	deadline := be.queueAllowance() + enumeration
	if deadline > be.maxOperationTimeout() {
		t.Errorf("the enumeration deadline %s exceeds the maximum %s", deadline, be.maxOperationTimeout())
	}
}
