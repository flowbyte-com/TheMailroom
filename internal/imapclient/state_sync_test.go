package imapclient

// Concurrency regressions for the backend state that is shared with
// shutdown and session discard.
//
// These tests use the real operation paths. ListMessages is the caller
// that records the selected mailbox and UIDVALIDITY; the LIST scan-budget
// overrun is the caller that aborts the connection. The public
// SelectedUIDValidity accessor and Backend.Close are the concurrent
// readers and writers of the same state.

import (
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// TestSelectedUIDValidityIsSynchronizedWithListMessages keeps readers of
// the selected mailbox and UIDVALIDITY running while a real ListMessages
// call records a new selection. The readers and the writer must use the
// same state mutex.
func TestSelectedUIDValidityIsSynchronizedWithListMessages(t *testing.T) {
	srv := &listFixtureServer{messages: budgetMessages(1)}
	addr, cleanup := srv.start(t)
	defer cleanup()
	be := listFixtureBackend(t, addr, 10)
	defer be.Close()

	// Pause the real ListMessages call at the selection write. The
	// readers are started while the writer is parked, so the two sides
	// are genuinely concurrent when the writer is released.
	paused := make(chan struct{})
	release := make(chan struct{})
	be.selectedHook = func() {
		close(paused)
		<-release
	}

	done := make(chan error, 1)
	go func() {
		_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
		done <- err
	}()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("ListMessages did not reach the selection write")
	}

	stop := make(chan struct{})
	started := make(chan struct{}, 4)
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			_ = be.SelectedUIDValidity()
			started <- struct{}{}
			for {
				select {
				case <-stop:
					return
				default:
					_ = be.SelectedUIDValidity()
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("a SelectedUIDValidity reader did not start")
		}
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListMessages did not return")
	}
	close(stop)
	readers.Wait()
}

// TestListMessagesAndCloseDoNotRace exercises the shutdown path against a
// real listing operation. Close and the operation may both record or
// clear the selected mailbox state; both must use the state mutex, and no
// operation may start after the closure.
func TestListMessagesAndCloseDoNotRace(t *testing.T) {
	for round := 0; round < 10; round++ {
		srv := &listFixtureServer{messages: budgetMessages(3)}
		addr, cleanup := srv.start(t)
		be := listFixtureBackend(t, addr, 10)

		done := make(chan error, 1)
		go func() {
			_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1})
			done <- err
		}()

		// Start the close while the operation is between its EXAMINE and
		// its first FETCH, the window in which setSelected runs.
		waitFor(t, 5*time.Second, "the operation to send EXAMINE", func() bool {
			return srv.commandCount("EXAMINE") >= 1
		})
		closed := make(chan error, 1)
		go func() { closed <- be.Close() }()

		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("ListMessages did not return after Close")
		}
		select {
		case err := <-closed:
			if err != nil {
				t.Errorf("Close: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Close did not return")
		}

		// Close is idempotent, and the closed backend refuses new
		// operations without adopting a connection.
		if err := be.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
		if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 1}); err == nil {
			t.Error("an operation after Close succeeded")
		}

		cleanup()
	}
}

// TestAbortConnIsSynchronizedWithClose drives the real list-scan overrun
// path, which calls abortConn while Close is concurrent. The socket
// reference must be taken under the state mutex; the close itself is
// performed after the mutex is released.
func TestAbortConnIsSynchronizedWithClose(t *testing.T) {
	const budget = 8
	for round := 0; round < 10; round++ {
		// The server under-reports the mailbox size and delivers more
		// than the budget. It stalls after the over-budget message, so
		// the client's abort is what ends the transfer.
		srv := &listFixtureServer{
			messages:   budgetMessages(budget + 1),
			exists:     1,
			stallAfter: budget + 1,
		}
		addr, cleanup := srv.start(t)
		be := listFixtureBackend(t, addr, budget)

		done := make(chan error, 1)
		go func() {
			_, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 5})
			done <- err
		}()

		waitFor(t, 5*time.Second, "the server to deliver over budget", func() bool {
			return srv.servedCount() >= budget+1
		})

		closed := make(chan error, 1)
		go func() { closed <- be.Close() }()

		select {
		case err := <-done:
			if err == nil {
				t.Error("the over-budget ListMessages reported success")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("ListMessages did not return after the abort")
		}
		select {
		case err := <-closed:
			if err != nil {
				t.Errorf("Close: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Close did not return")
		}
		cleanup()
	}
}
