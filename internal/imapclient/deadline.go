package imapclient

// The IMAP deadline and cancellation machinery.
//
// The go-imap client has no notion of a context: every command is
// synchronous and the only way to interrupt one is to break the
// connection underneath it. This file supplies the three pieces that
// makes an interruptible, finite IMAP operation possible.
//
//   - deadlineConn clamps every deadline the go-imap client sets on the
//     connection to the deadlines the backend armed for the current
//     operation. The go-imap client overwrites the socket deadline at
//     the start of every command, so the clamp is the only way to
//     guarantee that a later command never extends an earlier absolute
//     operation deadline.
//
//   - opRun is the per-operation state: the immutable socket reference
//     the cancellation may close, the recorded terminal cause and the
//     single terminal decision.
//
//   - The watcher goroutine, owned by the operation, turns a cancelled
//     context into a closed socket. It is joined before the operation
//     returns, so no callback can outlive its operation.
//
// The deadline wrapper sits directly above the aggregate byte budget and
// below TLS, so it sees every deadline the go-imap client sets and every
// socket error the transport produces:
//
//	implicit TLS:  raw socket -> serverDataBudget -> deadlineConn -> tls.Conn -> imap.Conn
//	STARTTLS:      raw socket -> serverDataBudget -> deadlineConn -> imap.Conn
//	               (after the fork's Upgrade: tls.Conn -> deadlineConn -> ...)
//
// The wrapper never calls upward into TLS, so it may hold its own lock
// while it applies a deadline without re-entering itself.

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// The internal timeout causes.
//
// A deadline expiry is observed at the socket, before the go-imap client
// can replace it with a generic connection-closed error. The wrapper
// records which deadline expired, so a per-command timeout and an overall
// operation timeout stay distinguishable and a genuine EOF is never
// mistaken for a timeout.
var (
	// errCommandTimeout reports that one command exceeded its ceiling.
	errCommandTimeout = errors.New("imap: the command deadline was exceeded")

	// errOperationTimeout reports that the operation exceeded its
	// absolute end-to-end deadline.
	errOperationTimeout = errors.New("imap: the operation deadline was exceeded")

	// errQueueTimeout reports that the account's operation gate could
	// not be acquired within the queue allowance.
	errQueueTimeout = errors.New("imap: the account operation queue wait expired")

	// errOperationCanceled reports that the caller's context cancelled
	// the operation.
	errOperationCanceled = errors.New("imap: the operation was canceled")

	// errBackendClosed reports that the backend was closed.
	errBackendClosed = errors.New("imap: the backend is closed")
)

// timeoutKind is the identity of an expired deadline.
type timeoutKind int

const (
	timeoutNone timeoutKind = iota
	// timeoutCommand is a per-command ceiling that expired.
	timeoutCommand
	// timeoutOperation is the absolute operation deadline that expired.
	timeoutOperation
)

// opState is the terminal state of an operation.
type opState int

const (
	// opActive means the operation has not reached a terminal decision.
	opActive opState = iota
	// opAborting means a cancellation won the terminal decision and the
	// watcher is closing the socket.
	opAborting
	// opAborted means the cancellation won and the socket is closed.
	opAborted
	// opFinished means the operation completed successfully.
	opFinished
	// opFailed means the operation completed with an error.
	opFailed
)

// opRun is the per-operation state.
//
// The socket reference is immutable once published: the watcher may only
// ever close the socket this operation published, so a late cancellation
// can never close a connection another operation adopted.
type opRun struct {
	mu    sync.Mutex
	state opState
	// cause is the recorded terminal cause. It is written at most once,
	// by whichever party first explains the failure.
	cause error
	// sock is the immutable per-operation socket reference.
	sock net.Conn

	finished chan struct{}
	wg       sync.WaitGroup
	stopOnce sync.Once

	// watcherDone records that the cancellation watcher has terminated.
	// It makes the shutdown ordering observable: when it is set, no
	// callback of this operation can still run.
	watcherDone atomic.Bool
}

// newOpRun returns an operation state with no socket published yet.
func newOpRun() *opRun {
	return &opRun{finished: make(chan struct{})}
}

// publish makes the operation's socket visible to the cancellation
// watcher.
//
// It succeeds only while the operation is still active, so a cancellation
// that arrived during the dial cannot publish a socket afterwards. The
// caller closes a socket it could not publish.
func (o *opRun) publish(sock net.Conn) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.state != opActive {
		return false
	}
	o.sock = sock
	return true
}

// abort records a cancellation and closes the operation's socket.
//
// It is the watcher and Backend.Close's only way to interrupt a blocked
// command: closing a socket writes nothing to the connection, so it
// cannot race the command writer the way an interleaved CLOSE or LOGOUT
// would. The first caller to take the lock wins; later ones are no-ops.
func (o *opRun) abort(cause error) {
	o.mu.Lock()
	if o.state != opActive {
		o.mu.Unlock()
		return
	}
	o.state = opAborting
	if o.cause == nil {
		o.cause = cause
	}
	sock := o.sock
	o.mu.Unlock()

	if sock != nil {
		_ = sock.Close()
	}

	o.mu.Lock()
	// The socket close may have taken long enough for the operation to
	// reach a terminal decision in the meantime. A late completion of
	// the abort must not overwrite that decision, so the state is only
	// advanced when the abort is still the current one.
	if o.state == opAborting {
		o.state = opAborted
	}
	o.mu.Unlock()
}

// currentState returns the operation's state under its lock.
func (o *opRun) currentState() opState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state
}

// isActive reports whether the operation has not reached a terminal
// decision.
//
// It is used when a socket is published: a socket that cannot be
// published is closed by the caller and never becomes a session.
func (o *opRun) isActive() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.state == opActive
}

// cancelCause returns the reason the operation was aborted.
//
// The recorded cause wins; otherwise the supplied fallback is used, so
// the result is never nil.
func (o *opRun) cancelCause(fallback error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cause != nil {
		return o.cause
	}
	if fallback != nil {
		return fallback
	}
	return errOperationCanceled
}

// recordCause records why the operation failed, at most once.
//
// The deadline wrapper calls it when it observes a socket timeout, so the
// identity of the expired deadline survives the go-imap client's
// connection-closed wrapping. It is a no-op once a cause is recorded, so a
// later, unrelated failure cannot overwrite the first explanation.
func (o *opRun) recordCause(cause error) {
	if cause == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cause == nil {
		o.cause = cause
	}
}

// recordedCause returns the recorded terminal cause, if any.
func (o *opRun) recordedCause() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.cause
}

// commit makes the single terminal decision for the operation.
//
// The decision is made under the operation's lock, and the cancellation
// state is re-checked under that lock, so a cancellation that has already
// been observed wins over a successful-looking callback. A committed
// outcome is never rewritten: once the state is no longer active, the
// recorded cause is returned unchanged.
func (o *opRun) commit(ctx context.Context, outcome error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch o.state {
	case opActive:
		// The operation has not been interrupted. The supplied outcome
		// is the final error, chosen by the caller after every security
		// violation has been inspected.
		if outcome != nil {
			o.state = opFailed
			o.cause = outcome
			return outcome
		}
		if err := context.Cause(ctx); err != nil {
			o.state = opFailed
			o.cause = err
			return err
		}
		o.state = opFinished
		return nil
	case opAborting, opAborted:
		// The watcher interrupted the I/O. The interruption explains
		// why the command stopped, but it does not replace the error
		// the caller selected after inspecting the security
		// violations: an aggregate-budget violation and a decoder-limit
		// failure are the cause that matters, and a concurrent
		// cancellation must not mask them.
		if outcome != nil {
			o.state = opFailed
			o.cause = outcome
			return outcome
		}
		if o.cause != nil {
			o.state = opFailed
			return o.cause
		}
		o.state = opFailed
		return errOperationCanceled
	default:
		// The outcome was already committed. It is never rewritten.
		return o.cause
	}
}

// start launches the cancellation watcher.
//
// The watcher selects on the operation's context and on the completion
// signal, so it always terminates: the operation closes the completion
// signal on every path.
func (o *opRun) start(ctx context.Context) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer o.watcherDone.Store(true)
		select {
		case <-ctx.Done():
			o.abort(context.Cause(ctx))
		case <-o.finished:
		}
	}()
}

// stop disarms the watcher and waits for it to finish.
//
// It is called after the terminal decision, so a watcher that has already
// started aborting has finished closing the socket before the connection
// can be discarded or reused, and a watcher that has not started can
// never start afterwards.
func (o *opRun) stop() {
	o.stopOnce.Do(func() { close(o.finished) })
	o.wg.Wait()
}

// deadlineConn clamps the deadlines the go-imap client sets on the
// connection to the ones the backend armed.
//
// The go-imap client calls SetDeadline exactly once per command, at the
// start of the command, and passes the zero time when its own Timeout is
// zero. That single call is therefore the command-boundary signal, and
// the wrapper answers it with the armed command ceiling.
//
// Every transition holds the wrapper's lock across the underlying
// SetDeadline, so the wrapper's state and the socket's deadline always
// move together. While the wrapper is idle no deadline request is
// honoured at all: an idle connection carries no deadline, so a stale
// request cannot reapply one to a connection that is about to be reused.
type deadlineConn struct {
	net.Conn

	mu sync.Mutex
	// armed reports whether an operation is in progress.
	armed bool
	// abs is the absolute operation deadline. It is written once per
	// operation and never extended.
	abs time.Time
	// cmd is the armed command ceiling. It is written by the backend
	// before each command and released when the command finishes.
	cmd time.Time
	// applied is the deadline the wrapper last applied to the socket.
	// It is what expired when the transport reports a timeout, so the
	// expiry can be classified exactly.
	applied time.Time
	// op is the operation the deadlines belong to.
	op *opRun
}

// armOperation arms the absolute operation deadline for op.
//
// The socket deadline is set immediately, so everything from here on -
// the greeting, the authentication, the commands and the streaming reads -
// is bounded by the operation deadline.
func (c *deadlineConn) armOperation(abs time.Time, op *opRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = true
	c.abs = abs
	c.cmd = time.Time{}
	c.op = op
	c.applied = abs
	_ = c.Conn.SetDeadline(abs)
}

// armCommand arms the ceiling of the command that is about to run.
//
// The ceiling is an absolute instant, so no later deadline request can
// renew it.
func (c *deadlineConn) armCommand(until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return
	}
	c.cmd = until
	c.applied = c.effectiveLocked(until)
	_ = c.Conn.SetDeadline(c.applied)
}

// commandFinished releases the command ceiling.
//
// The absolute operation deadline remains in force, so the connection
// between two commands of the same operation is still bounded, and a
// command ceiling cannot outlive its command.
func (c *deadlineConn) commandFinished() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return
	}
	c.cmd = time.Time{}
	c.applied = c.abs
	_ = c.Conn.SetDeadline(c.abs)
}

// finishOperation removes every operation deadline.
//
// The connection carries no deadline afterwards, so an expired command
// ceiling cannot terminate an idle connection that is about to be reused.
func (c *deadlineConn) finishOperation() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = false
	c.abs = time.Time{}
	c.cmd = time.Time{}
	c.applied = time.Time{}
	c.op = nil
	_ = c.Conn.SetDeadline(time.Time{})
}

// effectiveLocked returns the deadline that may actually be applied: the
// earliest of the requested instant, the absolute operation deadline and
// the armed command ceiling.
func (c *deadlineConn) effectiveLocked(requested time.Time) time.Time {
	eff := requested
	if !c.abs.IsZero() {
		if eff.IsZero() || c.abs.Before(eff) {
			eff = c.abs
		}
	}
	if !c.cmd.IsZero() {
		if eff.IsZero() || c.cmd.Before(eff) {
			eff = c.cmd
		}
	}
	return eff
}

// SetDeadline applies a clamped deadline.
//
// The go-imap client calls it once per command, before the command is
// written. A zero request means "no deadline requested": the wrapper
// answers it with the armed command ceiling, or with the absolute
// operation deadline when no command ceiling is armed.
func (c *deadlineConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		// The connection is idle. A deadline request now would terminate
		// a reusable idle connection when it expires, so it is ignored.
		return nil
	}
	requested := t
	if requested.IsZero() {
		if !c.cmd.IsZero() {
			requested = c.cmd
		} else {
			requested = c.abs
		}
	}
	c.applied = c.effectiveLocked(requested)
	return c.Conn.SetDeadline(c.applied)
}

// SetReadDeadline applies a clamped read deadline.
//
// The go-imap client does not use it; it is implemented so that no caller
// can extend the operation deadline through the read side either.
func (c *deadlineConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return nil
	}
	c.applied = c.effectiveLocked(t)
	return c.Conn.SetReadDeadline(c.applied)
}

// SetWriteDeadline applies a clamped write deadline.
//
// The go-imap client does not use it; it is implemented so that no caller
// can extend the operation deadline through the write side either.
func (c *deadlineConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return nil
	}
	c.applied = c.effectiveLocked(t)
	return c.Conn.SetWriteDeadline(c.applied)
}

// Read reads from the connection and records a deadline expiry.
//
// The go-imap client turns a socket timeout into a generic
// connection-closed error, so the cause is recorded here, at the socket,
// where the identity of the expired deadline is still known.
func (c *deadlineConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.observeDeadline(err)
	}
	return n, err
}

// Write writes to the connection and records a deadline expiry.
func (c *deadlineConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		c.observeDeadline(err)
	}
	return n, err
}

// observeDeadline records the cause of a socket timeout.
//
// Only a timeout is classified: a genuine EOF or reset is a transport
// failure and is never turned into a timeout, however close it happened
// to be to an expected deadline.
func (c *deadlineConn) observeDeadline(err error) {
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		return
	}
	c.mu.Lock()
	op := c.op
	applied := c.applied
	kind := timeoutNone
	switch {
	case applied.IsZero():
		// No deadline was in force, so the failure was not a timeout.
	case !c.cmd.IsZero() && applied.Equal(c.cmd):
		kind = timeoutCommand
	case !c.abs.IsZero() && applied.Equal(c.abs):
		kind = timeoutOperation
	default:
		// A shorter deadline was requested by the caller of the
		// connection - the go-imap client's own per-command timeout.
		// It is a per-command bound, so it is reported as one.
		kind = timeoutCommand
	}
	c.mu.Unlock()

	if op == nil || kind == timeoutNone {
		return
	}
	switch kind {
	case timeoutCommand:
		op.recordCause(errCommandTimeout)
	case timeoutOperation:
		op.recordCause(errOperationTimeout)
	}
}
