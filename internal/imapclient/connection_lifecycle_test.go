package imapclient

import (
	"testing"
	"time"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

// TestFailedReconnectDropsObsoleteSessionState checks the connection
// lifecycle when the server ends a session and the replacement
// connection then fails.
//
// The backend owns the client and the socket that backs it together. A
// session the server has ended is invalid: the client, the socket and
// the selection metadata that belonged to it must all be dropped before
// the replacement connection is attempted. If the replacement fails, the
// backend must not be left holding an obsolete client without an owned
// socket, nor a UIDVALIDITY that was observed on a session that no
// longer exists.
//
// The operation is not retried: one failed reconnect is one failed
// attempt.
func TestFailedReconnectDropsObsoleteSessionState(t *testing.T) {
	for _, mode := range []struct {
		name     string
		startTLS bool
	}{
		{name: "implicit TLS", startTLS: false},
		{name: "STARTTLS", startTLS: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			srv := &listFixtureServer{
				messages: budgetMessages(3),
				startTLS: mode.startTLS,
			}
			addr, cleanup := srv.start(t)
			defer cleanup()

			be := listFixtureBackendMode(t, addr, 100, mode.startTLS, 30*time.Second)
			defer be.Close()

			// A successful listing establishes the connection and the
			// selection.
			res, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
			if err != nil {
				t.Fatalf("ListMessages: %v", err)
			}
			if len(res.Messages) != 3 {
				t.Fatalf("got %d messages, want 3", len(res.Messages))
			}
			if uv := be.SelectedUIDValidity(); uv != 1 {
				t.Fatalf("SelectedUIDValidity = %d, want 1", uv)
			}
			be.mu.Lock()
			sel, validity := be.selectedMailbox, be.selectedValidity
			be.mu.Unlock()
			if sel != "INBOX" || validity != 1 {
				t.Fatalf("selected state = (%q, %d), want (\"INBOX\", 1)", sel, validity)
			}

			// The server ends the session. The listing above has
			// returned, so the command it issued has completed on the
			// client and the BYE cannot race that completion.
			srv.endSessions()
			waitFor(t, 10*time.Second, "the client to observe the logout", func() bool {
				be.mu.Lock()
				defer be.mu.Unlock()
				return be.client != nil && isLoggedOut(be.client)
			})

			// The next connection attempt fails at authentication.
			srv.setRejectLogin(true)
			if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10}); err == nil {
				t.Fatal("expected an error from the failed reconnect")
			}
			if n := srv.rejectedLoginCount(); n != 1 {
				t.Errorf("server refused %d logins, want 1: the operation must not retry", n)
			}
			if n := srv.loginCount(); n != 2 {
				t.Errorf("server received %d logins, want 2: the initial one and the failed reconnect, no retries", n)
			}
			if n := srv.connectionCount(); n != 2 {
				t.Errorf("server saw %d connections, want 2: one session and one failed reconnect, no retries", n)
			}

			// The obsolete session must be gone: no client, no socket
			// and no selection metadata.
			be.mu.Lock()
			client, conn := be.client, be.conn
			sel, validity = be.selectedMailbox, be.selectedValidity
			be.mu.Unlock()
			if client != nil {
				t.Error("the obsolete client was retained after the failed reconnect")
			}
			if conn != nil {
				t.Error("the obsolete socket was retained after the failed reconnect")
			}
			if sel != "" {
				t.Errorf("selected mailbox = %q, want it to be cleared", sel)
			}
			if validity != 0 {
				t.Errorf("selected UIDVALIDITY = %d, want it to be cleared", validity)
			}
			if uv := be.SelectedUIDValidity(); uv != 0 {
				t.Errorf("SelectedUIDValidity = %d, want 0 after the failed reconnect", uv)
			}

			// A later successful reconnect restores a valid session.
			srv.setRejectLogin(false)
			res, err = be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10})
			if err != nil {
				t.Fatalf("ListMessages after the failed reconnect: %v", err)
			}
			if len(res.Messages) != 3 {
				t.Errorf("got %d messages, want 3", len(res.Messages))
			}
			if uv := be.SelectedUIDValidity(); uv != 1 {
				t.Errorf("SelectedUIDValidity = %d, want 1 after the reconnect", uv)
			}
			be.mu.Lock()
			sel, validity = be.selectedMailbox, be.selectedValidity
			be.mu.Unlock()
			if sel != "INBOX" || validity != 1 {
				t.Errorf("selected state = (%q, %d), want (\"INBOX\", 1) after the reconnect", sel, validity)
			}
		})
	}
}

// TestReconnectAfterLogoutKeepsClientAndSocketPaired checks the
// client/socket pairing across a plain reconnect.
//
// The backend must never hold a client without a socket, and never hold
// a socket that belongs to a previous connection.
func TestReconnectAfterLogoutKeepsClientAndSocketPaired(t *testing.T) {
	srv := &listFixtureServer{
		messages: budgetMessages(3),
	}
	addr, cleanup := srv.start(t)
	defer cleanup()

	be := listFixtureBackend(t, addr, 100)
	defer be.Close()

	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10}); err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// The listing above has returned, so the command it issued has
	// completed on the client and the BYE cannot race that completion.
	srv.endSessions()
	waitFor(t, 10*time.Second, "the client to observe the logout", func() bool {
		be.mu.Lock()
		defer be.mu.Unlock()
		return be.client != nil && isLoggedOut(be.client)
	})

	// The next call reconnects and succeeds.
	if _, err := be.ListMessages(t.Context(), "test", "INBOX", mailbox.ListOptions{Limit: 10}); err != nil {
		t.Fatalf("ListMessages after the logout: %v", err)
	}
	be.mu.Lock()
	client, conn := be.client, be.conn
	be.mu.Unlock()
	if client == nil {
		t.Error("the client is nil after a successful reconnect")
	}
	if conn == nil {
		t.Error("the socket is nil after a successful reconnect")
	}
	if n := srv.connectionCount(); n != 2 {
		t.Errorf("server saw %d connections, want 2", n)
	}
}
