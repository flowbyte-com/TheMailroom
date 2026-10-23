package imapclient

// The fixture for the Phase 3 PR 6 deadline and cancellation tests.
//
// The fixture is an in-process IMAP server that can block at a chosen
// stage of the connection sequence. It is signal-driven: the server
// blocks on a channel the test releases, so a test reaches the stage it
// wants to exercise without sleeping for it.
//
// The fixture owns every resource it creates. Its shutdown releases the
// blocking stages, closes the accepted sockets, stops the listener and
// joins the connection goroutines, so a test never leaves a goroutine
// behind and never relies on the test binary eventually exiting.

import (
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/config"
)

// deadlineStage names the point in the connection sequence a fixture
// blocks at.
type deadlineStage int

const (
	// stageNone never blocks.
	stageNone deadlineStage = iota
	// stageGreeting blocks before the IMAP greeting is sent.
	stageGreeting
	// stageStartTLS blocks in the TLS upgrade that follows STARTTLS.
	stageStartTLS
	// stageLogin blocks before LOGIN is answered.
	stageLogin
	// stageExamine blocks before EXAMINE is answered.
	stageExamine
	// stageFetch blocks in the middle of a FETCH response.
	stageFetch
)

// stageName names a stage for a test failure message.
func stageName(s deadlineStage) string {
	switch s {
	case stageGreeting:
		return "greeting"
	case stageStartTLS:
		return "starttls"
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

// deadlineFixture is an IMAP server that can block at a chosen stage.
type deadlineFixture struct {
	ln net.Listener
	// implicitTLS serves implicit TLS. Otherwise the fixture speaks
	// plaintext and upgrades on STARTTLS.
	implicitTLS bool
	// stage is the point the server blocks at.
	stage deadlineStage
	// stallAt is the occurrence of the stage's command that blocks.
	// One is the first occurrence; two blocks the second one, so a
	// reused connection can be stalled by a later command. The count is
	// per command, not per connection.
	stallAt int
	// stageHits counts the occurrences of the stage's command.
	stageHits int
	// varyUIDValidity makes every connection report a different
	// UIDVALIDITY, so a test can prove that a reconnection observed a
	// fresh value.
	varyUIDValidity bool

	// release unblocks the server. It is closed by the shutdown, so a
	// blocked handler always returns.
	release     chan struct{}
	releaseOnce sync.Once
	// reached is closed when the server reaches the blocking stage.
	reached     chan struct{}
	reachedOnce sync.Once
	cert        tls.Certificate

	mu          sync.Mutex
	conns       []net.Conn
	commands    []string
	connections int
	uidValidity uint32

	// wg tracks the connection goroutines, so the shutdown can join
	// them and a test can prove that they all terminated.
	wg sync.WaitGroup
	// acceptWG tracks the accept loop, so the shutdown can stop it and
	// join it before it waits for the connection goroutines. Joining the
	// accept loop first is what keeps WaitGroup.Add from racing with
	// Wait.
	acceptWG sync.WaitGroup
	// alive counts the connection goroutines that have not returned.
	alive int
	// shutdownTimedOut records that the bounded join did not complete,
	// so the shutdown verification can fail the test instead of
	// silently succeeding.
	shutdownTimedOut bool

	closeOnce sync.Once
}

// newDeadlineFixture starts a fixture and registers its shutdown with
// the test, so the resources are released even when the test fails.
func newDeadlineFixture(t *testing.T, implicitTLS bool, stage deadlineStage) *deadlineFixture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &deadlineFixture{
		ln:          ln,
		implicitTLS: implicitTLS,
		stage:       stage,
		release:     make(chan struct{}),
		reached:     make(chan struct{}),
		cert:        genCert(t),
		uidValidity: 1,
		stallAt:     1,
	}
	t.Cleanup(func() {
		f.shutdown()
		if f.shutdownTimedOut {
			t.Errorf("the fixture did not shut down within its bound: %d connection goroutines are still running", f.aliveGoroutines())
		}
		if n := f.aliveGoroutines(); n != 0 {
			t.Errorf("the fixture left %d connection goroutines behind", n)
		}
	})
	f.acceptWG.Add(1)
	go f.serve()
	return f
}

// releaseStage unblocks the fixture's blocking stage.
//
// It is safe to call more than once, and the shutdown calls it too, so
// a test never closes the channel itself.
func (f *deadlineFixture) releaseStage() {
	f.releaseOnce.Do(func() { close(f.release) })
}

// shutdown releases every resource the fixture owns.
//
// It releases the blocking stages, closes the accepted sockets, stops the
// accept loop and joins it, and then joins the connection goroutines. The
// join is bounded: a goroutine that does not terminate is recorded, and
// the fixture's cleanup turns that into a test failure rather than a
// silent success.
func (f *deadlineFixture) shutdown() {
	f.closeOnce.Do(func() {
		f.releaseStage()
		f.mu.Lock()
		conns := append([]net.Conn(nil), f.conns...)
		f.mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
		// Stop the accept loop and join it, so that no further
		// connection goroutine can be added while the shutdown waits for
		// them.
		_ = f.ln.Close()
		f.acceptWG.Wait()

		done := make(chan struct{})
		go func() {
			f.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			f.mu.Lock()
			f.shutdownTimedOut = true
			f.mu.Unlock()
		}
	})
}

// setStallAt makes the given occurrence of the stage's command block.
// It must be called before the first operation.
func (f *deadlineFixture) setStallAt(n int) {
	f.mu.Lock()
	f.stallAt = n
	f.mu.Unlock()
}

// varyUIDValidityOn makes every connection report a different
// UIDVALIDITY, so a test can prove that a reconnection observed a fresh
// value. It must be called before the first operation.
func (f *deadlineFixture) varyUIDValidityOn() {
	f.mu.Lock()
	f.varyUIDValidity = true
	f.mu.Unlock()
}

// aliveGoroutines returns the number of connection goroutines that have
// not returned.
func (f *deadlineFixture) aliveGoroutines() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive
}

func (f *deadlineFixture) addr() (string, int) {
	host, portStr, _ := net.SplitHostPort(f.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return host, port
}

// connectionCount returns the number of accepted connections.
func (f *deadlineFixture) connectionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connections
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
		t.Fatalf("the fixture never reached its blocking stage %s", stageName(f.stage))
	}
}

// shouldBlock counts an occurrence of the stage's command and reports
// whether the fixture must block on it.
func (f *deadlineFixture) shouldBlock() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stageHits++
	return f.stageHits == f.stallAt
}

// blockUntilRelease parks the handler until the fixture is released.
func (f *deadlineFixture) blockUntilRelease() {
	f.reachedOnce.Do(func() { close(f.reached) })
	<-f.release
}

func (f *deadlineFixture) serve() {
	defer f.acceptWG.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.connections++
		if f.varyUIDValidity {
			f.uidValidity++
		}
		f.alive++
		f.mu.Unlock()
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() {
				f.mu.Lock()
				f.alive--
				f.mu.Unlock()
			}()
			f.handle(c)
		}()
	}
}

func (f *deadlineFixture) handle(raw net.Conn) {
	c := raw
	if f.implicitTLS {
		tc := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12})
		if err := tc.Handshake(); err != nil {
			return
		}
		c = tc
	}
	if f.stage == stageGreeting && f.shouldBlock() {
		f.blockUntilRelease()
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
		f.mu.Lock()
		f.commands = append(f.commands, cmd)
		uid := f.uidValidity
		f.mu.Unlock()
		switch {
		case strings.Contains(strings.ToUpper(line), "STARTTLS"):
			if _, err := c.Write([]byte(tag + " OK begin TLS\r\n")); err != nil {
				return
			}
			if f.stage == stageStartTLS && f.shouldBlock() {
				f.blockUntilRelease()
				return
			}
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{f.cert}, MinVersion: tls.VersionTLS12})
			if err := tc.Handshake(); err != nil {
				return
			}
			c = tc
		case strings.Contains(strings.ToUpper(line), "LOGIN"):
			if f.stage == stageLogin && f.shouldBlock() {
				f.blockUntilRelease()
				return
			}
			if _, err := c.Write([]byte(tag + " OK logged in\r\n")); err != nil {
				return
			}
		case strings.Contains(strings.ToUpper(line), "EXAMINE"):
			if f.stage == stageExamine && f.shouldBlock() {
				f.blockUntilRelease()
				return
			}
			if _, err := c.Write([]byte("* 1 EXISTS\r\n* OK [UIDVALIDITY " +
				strconv.FormatUint(uint64(uid), 10) + "] selected\r\n" + tag + " OK selected\r\n")); err != nil {
				return
			}
		case strings.Contains(strings.ToUpper(line), "FETCH"):
			if f.stage == stageFetch && f.shouldBlock() {
				// A partial response, then a stall: the client has
				// delivered part of the body and is waiting for the
				// rest.
				if _, err := c.Write([]byte("* 1 FETCH (UID 1 BODY[] {10}\r\n")); err != nil {
					return
				}
				f.blockUntilRelease()
				return
			}
			if _, err := c.Write([]byte("* 1 FETCH (UID 1 BODY[] {0}\r\n)\r\n" + tag + " OK done\r\n")); err != nil {
				return
			}
		default:
			if _, err := c.Write([]byte(tag + " OK done\r\n")); err != nil {
				return
			}
		}
	}
}

// deadlineBackend builds a backend against the fixture with short
// deadlines, so a test does not wait for the production values.
func deadlineBackend(t *testing.T, host string, port int, allowance, control time.Duration, rate int, implicitTLS bool) *Backend {
	t.Helper()
	cfg := deadlineAccountConfig(host, port, allowance, control, rate, implicitTLS)
	be, err := New(cfg, Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return be
}

// deadlineAccountConfig builds the account configuration the deadline
// tests use.
func deadlineAccountConfig(host string, port int, allowance, control time.Duration, rate int, implicitTLS bool) config.AccountConfig {
	return config.AccountConfig{
		ID: "test", Email: "test@example.test", Backend: "imap",
		IMAP: config.IMAPConfig{
			Host: host, Port: port, Username: "user", Password: "secret",
			TLS:                   implicitTLS,
			StartTLS:              !implicitTLS,
			OperationAllowance:    allowance,
			ControlCommandTimeout: control,
			TransferRate:          rate,
			MaxOperationTimeout:   config.MinIMAPMaxOperationTimeout,
		},
	}
}

// fixtureBackend builds a backend and a fixture for the given TLS mode
// and stage, and registers the fixture's shutdown with the test.
func fixtureBackend(t *testing.T, implicitTLS bool, stage deadlineStage, allowance, control time.Duration) (*Backend, *deadlineFixture) {
	t.Helper()
	fx := newDeadlineFixture(t, implicitTLS, stage)
	host, port := fx.addr()
	be := deadlineBackend(t, host, port, allowance, control, config.MaxIMAPTransferRate, implicitTLS)
	t.Cleanup(func() { _ = be.Close() })
	return be, fx
}

// dialCancellationAddress is an address in the documentation test range
// that no host routes. A connection to it either hangs or is refused
// immediately; the test handles both.
const dialCancellationAddress = "192.0.2.1:993"

func formatAddress(host string, port int) string {
	return fmt.Sprintf("%s:%d", host, port)
}
