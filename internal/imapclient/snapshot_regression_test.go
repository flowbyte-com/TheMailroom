package imapclient

// Regression coverage for the go-imap v1.2.1-themailroom.6 snapshot
// corrections.
//
// The fork now returns an independent snapshot from EXAMINE, installs a
// replacement snapshot for an unsolicited EXISTS/RECENT update, and keeps
// Client.Mailbox nil while a selection is unfinished. The tests below run
// against TheMailroom's real backend connection and fixture, not against a
// second copy of the fork's own test suite.

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/client"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// TestExamineSnapshotImmutableAfterUnsolicitedExists reproduces the
// previously race-sensitive order: EXAMINE has completed, the application
// holds the returned status, and the server then sends an unsolicited
// EXISTS. The application reads the old snapshot while the update is
// processed.
func TestExamineSnapshotImmutableAfterUnsolicitedExists(t *testing.T) {
	const initialMessages = 3
	srv := &listFixtureServer{
		messages: budgetMessages(initialMessages),
		exists:   initialMessages,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 10)
	defer be.Close()

	var (
		cl            *client.Client
		initial       *imap.MailboxStatus
		fresh         *imap.MailboxStatus
		selectedState imap.ConnState
	)
	err := be.withClient(t.Context(), opList, func(c *client.Client) error {
		cl = c
		var err error
		initial, err = c.Select("INBOX", true)
		if err != nil {
			return fmt.Errorf("EXAMINE: %w", err)
		}
		if initial == nil {
			return errors.New("EXAMINE returned a nil snapshot")
		}
		selectedState = c.State()
		if got := c.Mailbox(); got != initial {
			return fmt.Errorf("Client.Mailbox() = %p, want the completion snapshot %p", got, initial)
		}
		if initial.Messages != initialMessages || initial.UidValidity != 1 || !initial.ReadOnly {
			return fmt.Errorf("initial EXAMINE snapshot = %d messages, UIDVALIDITY %d, read-only %v; want %d, 1, true",
				initial.Messages, initial.UidValidity, initial.ReadOnly, initialMessages)
		}

		// Keep reading the published snapshot while the reader processes the
		// update. Under an in-place implementation this is the data race the
		// fork correction removes.
		stop := make(chan struct{})
		var readers sync.WaitGroup
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = initial.Messages
				_ = initial.Recent
				_ = initial.UidValidity
				_ = initial.ReadOnly
				_ = len(initial.Items)
				if latest := c.Mailbox(); latest != nil {
					_ = latest.Messages
					_ = latest.UidValidity
					_ = latest.ReadOnly
				}
				runtime.Gosched()
			}
		}()

		if err := srv.sendUnsolicitedExists(1); err != nil {
			close(stop)
			readers.Wait()
			return fmt.Errorf("send post-selection EXISTS: %w", err)
		}

		deadline := time.Now().Add(5 * time.Second)
		for {
			fresh = c.Mailbox()
			if fresh != nil && fresh.Messages == 1 {
				break
			}
			if time.Now().After(deadline) {
				close(stop)
				readers.Wait()
				return errors.New("timed out waiting for the post-selection EXISTS snapshot")
			}
			time.Sleep(time.Millisecond)
		}
		close(stop)
		readers.Wait()

		if initial.Messages != initialMessages {
			return fmt.Errorf("EXAMINE snapshot mutated in place: Messages = %d, want %d",
				initial.Messages, initialMessages)
		}
		if fresh == initial {
			return errors.New("post-selection EXISTS reused the EXAMINE snapshot pointer")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot regression: %v", err)
	}

	if selectedState != imap.SelectedState {
		t.Errorf("client state after EXAMINE = %v, want %v", selectedState, imap.SelectedState)
	}
	if initial.Messages != initialMessages {
		t.Errorf("EXAMINE snapshot Messages = %d, want %d", initial.Messages, initialMessages)
	}
	if initial.UidValidity != 1 {
		t.Errorf("EXAMINE snapshot UIDVALIDITY = %d, want 1", initial.UidValidity)
	}
	if !initial.ReadOnly {
		t.Error("EXAMINE snapshot is not read-only")
	}
	if fresh == initial {
		t.Error("post-selection EXISTS reused the EXAMINE snapshot pointer")
	}
	if fresh.Messages != 1 {
		t.Errorf("latest snapshot Messages = %d, want 1", fresh.Messages)
	}
	if fresh.UidValidity != 1 {
		t.Errorf("latest snapshot UIDVALIDITY = %d, want 1", fresh.UidValidity)
	}
	if !fresh.ReadOnly {
		t.Error("latest snapshot is not read-only")
	}
	if latest := cl.Mailbox(); latest != fresh {
		t.Error("Client.Mailbox() did not return the latest post-selection snapshot")
	}

	// A later BYE must not resurrect the selected session or mutate either
	// snapshot. The tagged EXAMINE completion remains the command result; the
	// client state still reflects the logout.
	srv.endSessions()
	waitFor(t, 5*time.Second, "the client to observe the BYE", func() bool {
		return cl.State() == imap.LogoutState
	})
	if m := cl.Mailbox(); m != nil {
		t.Errorf("Client.Mailbox() = %#v after BYE, want nil", m)
	}
	if initial.Messages != initialMessages || fresh.Messages != 1 {
		t.Errorf("a later BYE mutated a returned snapshot: initial Messages = %d, fresh Messages = %d",
			initial.Messages, fresh.Messages)
	}
}

// TestListMessagesBoundsAfterUnsolicitedExists runs the real ListMessages
// operation while the server sends an unsolicited EXISTS before the FETCH
// response. It checks the page, the UIDVALIDITY binding, the fresh status
// available through Client.Mailbox, the scan preflight, and the FETCH
// delivery bound.
func TestListMessagesBoundsAfterUnsolicitedExists(t *testing.T) {
	subjectsFor := func(msgs []listFixtureMessage) []string {
		out := make([]string, len(msgs))
		for i, m := range msgs {
			out[i] = m.subject
		}
		return out
	}

	t.Run("correct page and fresh status", func(t *testing.T) {
		const budget = 10
		msgs := budgetMessages(4)
		srv := &listFixtureServer{
			messages:          msgs,
			exists:            len(msgs),
			existsBeforeFetch: 1,
		}
		addr, cleanup := srv.start(t)
		defer cleanup()

		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 2})
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		if res.UIDValidity != 1 {
			t.Errorf("ListMessages UIDValidity = %d, want 1", res.UIDValidity)
		}
		if got, want := subjects(res.Messages), subjectsFor(msgs[:2]); !equalStrings(got, want) {
			t.Errorf("ListMessages subjects = %v, want %v", got, want)
		}
		if served := srv.servedCount(); served != len(msgs) {
			t.Errorf("server delivered %d summaries, want %d", served, len(msgs))
		}
		if got := srv.lastFetch(); !strings.Contains(strings.ToUpper(got), "UID FETCH") ||
			!strings.Contains(got, "1:*") {
			t.Errorf("fetch command = %q, want a UID FETCH of 1:*", got)
		}

		var fresh *imap.MailboxStatus
		if err := be.withClient(t.Context(), opPlain, func(c *client.Client) error {
			fresh = c.Mailbox()
			if fresh == nil {
				return errors.New("Client.Mailbox() returned nil after a successful listing")
			}
			return nil
		}); err != nil {
			t.Fatalf("read fresh mailbox status: %v", err)
		}
		if fresh.Messages != 1 {
			t.Errorf("fresh snapshot Messages = %d, want the post-EXAMINE EXISTS value 1", fresh.Messages)
		}
		if fresh.UidValidity != 1 {
			t.Errorf("fresh snapshot UIDVALIDITY = %d, want 1", fresh.UidValidity)
		}
		if !fresh.ReadOnly {
			t.Error("fresh snapshot is not read-only")
		}
	})

	t.Run("scan preflight still refuses", func(t *testing.T) {
		const budget = 3
		srv := &listFixtureServer{
			messages:          budgetMessages(budget + 1),
			exists:            budget + 1,
			existsBeforeFetch: 1,
		}
		addr, cleanup := srv.start(t)
		defer cleanup()

		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 2})
		requireListTooLarge(t, err)
		if len(res.Messages) != 0 {
			t.Errorf("got %d messages, want 0: a refused listing must not return a partial page", len(res.Messages))
		}
		if n := srv.fetchCount(); n != 0 {
			t.Errorf("server saw %d fetches, want 0 for an over-budget mailbox", n)
		}
	})

	t.Run("fetch delivery still refuses", func(t *testing.T) {
		const budget = 3
		srv := &listFixtureServer{
			messages:          budgetMessages(budget + 1),
			exists:            1,
			existsBeforeFetch: 1,
		}
		addr, cleanup := srv.start(t)
		defer cleanup()

		be := listFixtureBackend(t, addr, budget)
		defer be.Close()

		res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 2})
		requireListTooLarge(t, err)
		if len(res.Messages) != 0 {
			t.Errorf("got %d messages, want 0: a refused listing must not return a partial page", len(res.Messages))
		}
		waitFor(t, 5*time.Second, "the server to deliver more than the budget", func() bool {
			return srv.servedCount() >= budget+1
		})
	})
}

// TestExamineTaggedCompletionFollowedByBYE checks the fork's issue #1
// ordering through TheMailroom's backend: a tagged EXAMINE completion
// followed immediately by BYE still yields a successful, complete snapshot,
// while the connection state reflects the logout and no current mailbox is
// published.
func TestExamineTaggedCompletionFollowedByBYE(t *testing.T) {
	srv := &listFixtureServer{
		messages:        budgetMessages(3),
		exists:          3,
		byeAfterExamine: true,
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 10)
	defer be.Close()

	var (
		cl     *client.Client
		status *imap.MailboxStatus
	)
	err := be.withClient(t.Context(), opList, func(c *client.Client) error {
		cl = c
		var err error
		status, err = c.Select("INBOX", true)
		return err
	})
	if err != nil {
		t.Fatalf("EXAMINE followed by BYE returned an error: %v", err)
	}
	if status == nil {
		t.Fatal("EXAMINE followed by BYE returned a nil snapshot")
	}
	if status.Messages != 3 {
		t.Errorf("completion snapshot Messages = %d, want 3", status.Messages)
	}
	if status.UidValidity != 1 {
		t.Errorf("completion snapshot UIDVALIDITY = %d, want 1", status.UidValidity)
	}
	if !status.ReadOnly {
		t.Error("completion snapshot is not read-only")
	}

	waitFor(t, 5*time.Second, "the client to process the BYE", func() bool {
		return cl.State() == imap.LogoutState
	})
	if m := cl.Mailbox(); m != nil {
		t.Errorf("Client.Mailbox() = %#v after BYE, want nil", m)
	}
	if status.Messages != 3 || status.UidValidity != 1 || !status.ReadOnly {
		t.Errorf("returned completion snapshot changed after BYE: %d messages, UIDVALIDITY %d, read-only %v",
			status.Messages, status.UidValidity, status.ReadOnly)
	}
}
