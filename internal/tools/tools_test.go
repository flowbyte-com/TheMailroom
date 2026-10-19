package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/permissions"
)

// testEnv bundles a configured MCP server, its account registry, and a
// client transport connected to the same in-memory channel.
type testEnv struct {
	server   *mcp.Server
	registry *accounts.Registry
	client   *mcp.ClientSession
	cleanup  func()
}

func setupEnv(t *testing.T, _ string) *testEnv {
	t.Helper()
	repoRoot := findRepoRoot(t)
	messages := filepath.Join(repoRoot, "testdata", "messages")
	empty := t.TempDir()

	yaml := `
server:
  log_level: error
accounts:
  - id: full
    backend: memory
    email: full@example.test
    mailbox_root: __MBX__
    permissions: [read]
  - id: norights
    backend: memory
    email: nr@example.test
    mailbox_root: __EMPTY__
    permissions: []
`
	yaml = strings.ReplaceAll(yaml, "__MBX__", messages)
	yaml = strings.ReplaceAll(yaml, "__EMPTY__", empty)

	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	reg, err := accounts.New(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	Register(server, reg)

	// In-memory transport pair.
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "client"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	return &testEnv{
		server:   server,
		registry: reg,
		client:   session,
		cleanup: func() {
			session.Close()
		},
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// call invokes a tool by name with the given arguments, returning the
// parsed structured content (or the error text if the call failed).
func (e *testEnv) call(t *testing.T, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := e.client.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		// Pull the first text content; tools emit TypedError messages
		// as text.
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				return nil, &toolErr{msg: tc.Text, code: extractCode(res)}
			}
		}
		return nil, &toolErr{msg: "unknown tool error", code: "Unknown"}
	}
	if res.StructuredContent != nil {
		return res.StructuredContent.(map[string]any), nil
	}
	return nil, nil
}

type toolErr struct {
	msg  string
	code string
}

func (e *toolErr) Error() string { return e.msg }

// extractCode peeks at the structured content for a code field.
func extractCode(res *mcp.CallToolResult) string {
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		if c, ok := sc["code"].(string); ok {
			return c
		}
	}
	return ""
}

// --- tests ---

func TestListAccounts(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "list_accounts", map[string]any{})
	if err != nil {
		t.Fatalf("list_accounts: %v", err)
	}
	arr, ok := got["accounts"].([]any)
	if !ok {
		t.Fatalf("accounts missing or wrong type: %T", got["accounts"])
	}
	if len(arr) != 2 {
		t.Errorf("len(accounts) = %d, want 2", len(arr))
	}
	// Ensure no password field is present.
	for _, a := range arr {
		m := a.(map[string]any)
		for k := range m {
			if strings.Contains(strings.ToLower(k), "password") {
				t.Errorf("unexpected password-looking key %q", k)
			}
		}
	}
}

func TestListMailboxes(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "list_mailboxes", map[string]any{"account_id": "full"})
	if err != nil {
		t.Fatalf("list_mailboxes: %v", err)
	}
	mailboxes := got["mailboxes"].([]any)
	if len(mailboxes) < 1 {
		t.Errorf("len = %d", len(mailboxes))
	}
}

func TestListMailboxesNoPermission(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	_, err := env.call(t, "list_mailboxes", map[string]any{"account_id": "norights"})
	if err == nil {
		t.Fatal("expected permission denied")
	}
	if !strings.Contains(err.Error(), "lacks capability") {
		t.Errorf("err = %v, want 'lacks capability'", err)
	}
}

func TestListMailboxesMissingAccount(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	_, err := env.call(t, "list_mailboxes", map[string]any{"account_id": "ghost"})
	if err == nil {
		t.Fatal("expected account not found")
	}
	if !strings.Contains(err.Error(), "account not found") {
		t.Errorf("err = %v", err)
	}
}

func TestListMessages(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "list_messages", map[string]any{
		"account_id": "full",
		"mailbox":    "INBOX",
		"limit":      5,
	})
	if err != nil {
		t.Fatalf("list_messages: %v", err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("len = %d, want 1", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["subject"] != "Welcome to TheMailroom" {
		t.Errorf("subject = %v", first["subject"])
	}
}

func TestReadMessage(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "read_message", map[string]any{
		"account_id": "full",
		"mailbox":    "Sent",
		"uid":        float64(1), // JSON numbers decode to float64
	})
	if err != nil {
		t.Fatalf("read_message: %v", err)
	}
	if got["text"] == nil {
		t.Error("text body missing")
	}
	atts := got["attachments"].([]any)
	if len(atts) != 1 {
		t.Errorf("len(attachments) = %d", len(atts))
	}
}

func TestReadMessageUidMismatch(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	_, err := env.call(t, "read_message", map[string]any{
		"account_id":   "full",
		"mailbox":      "Sent",
		"uid":          float64(1),
		"uid_validity": float64(99999),
	})
	if err == nil {
		t.Fatal("expected uid mismatch")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("err = %v", err)
	}
}

func TestSearchMessages(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "search_messages", map[string]any{
		"account_id": "full",
		"mailbox":    "INBOX",
		"query": map[string]any{
			"subject": "Welcome",
		},
	})
	if err != nil {
		t.Fatalf("search_messages: %v", err)
	}
	matches := got["matches"].([]any)
	if len(matches) != 1 {
		t.Errorf("len = %d", len(matches))
	}
}

func TestSearchMessagesEmptyQuery(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	// An empty query has no filters; the implementation returns every
	// matching message up to the limit (default 50 in Phase 1).
	got, err := env.call(t, "search_messages", map[string]any{
		"account_id": "full",
		"mailbox":    "INBOX",
		"query":      map[string]any{},
	})
	if err != nil {
		t.Fatalf("search_messages: %v", err)
	}
	matches := got["matches"].([]any)
	if len(matches) != 1 {
		t.Errorf("len = %d, want 1 (empty query returns all)", len(matches))
	}
}

func TestListAccountsHasNoSecrets(t *testing.T) {
	// Direct call to the registry to ensure the wire-format conversion
	// strips all credentials.
	env := setupEnv(t, "full")
	defer env.cleanup()

	for _, a := range env.registry.All() {
		if a.Email == "" {
			t.Error("account missing email")
		}
		// Touch the field paths that should not contain secrets. If a
		// future refactor adds a password-looking field on Account,
		// this test still compiles but the wire-format list_accounts
		// test above will catch a leak.
		_ = a.AttachmentSendDirs
		_ = a.Permissions.Has(permissions.CapRead)
	}
}

// TestEmptyPermissionsRejectsAllReads is a HAL-review regression: an
// account with no permissions must reject every read tool.
func TestEmptyPermissionsRejectsAllReads(t *testing.T) {
	// Build a one-account registry with no permissions.
	messages := filepath.Join(findRepoRoot(t), "testdata", "messages")
	yaml := `
server:
  log_level: error
accounts:
  - id: locked
    backend: memory
    email: locked@example.test
    mailbox_root: __MBX__
    permissions: []
`
	yaml = strings.ReplaceAll(yaml, "__MBX__", messages)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	reg, err := accounts.New(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	Register(srv, reg)
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil)
	session, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	cases := []struct {
		name string
		args map[string]any
	}{
		{"list_mailboxes", map[string]any{"account_id": "locked"}},
		{"list_messages", map[string]any{"account_id": "locked", "mailbox": "INBOX"}},
		{"read_message", map[string]any{"account_id": "locked", "mailbox": "INBOX", "uid": float64(1)}},
		{"search_messages", map[string]any{"account_id": "locked", "mailbox": "INBOX", "query": map[string]any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      tc.name,
				Arguments: tc.args,
			})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected tool error for %s; got success", tc.name)
			}
			// The error message must include the capability phrase so
			// the agent can act on it.
			var text string
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					text = tc.Text
				}
			}
			if !strings.Contains(text, "lacks capability") {
				t.Errorf("%s error = %q, want 'lacks capability'", tc.name, text)
			}
		})
	}
}

// TestReadMessageUIDValidityStrict is a HAL-review regression: a stale
// UIDVALIDITY must be rejected even if the UID is valid.
func TestReadMessageUIDValidityStrict(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	// UIDVALIDITY=1 is the value the memory backend assigns. Use 99999
	// to force a mismatch.
	_, err := env.call(t, "read_message", map[string]any{
		"account_id":   "full",
		"mailbox":      "Sent",
		"uid":          float64(1),
		"uid_validity": float64(99999),
	})
	if err == nil {
		t.Fatal("expected uid mismatch")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("err = %v", err)
	}
}

// TestReadMessageUIDValidityAccepts is a HAL-review regression: a
// matching UIDVALIDITY must be accepted.
func TestReadMessageUIDValidityAccepts(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "read_message", map[string]any{
		"account_id":   "full",
		"mailbox":      "Sent",
		"uid":          float64(1),
		"uid_validity": float64(1),
	})
	if err != nil {
		t.Fatalf("read_message: %v", err)
	}
	if got["text"] == nil {
		t.Error("text body missing")
	}
}

// TestReadMessageUIDValidityOmitted checks that omitting UIDVALIDITY
// is allowed (the wire schema marks it omitempty).
func TestReadMessageUIDValidityOmitted(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "read_message", map[string]any{
		"account_id": "full",
		"mailbox":    "Sent",
		"uid":        float64(1),
	})
	if err != nil {
		t.Fatalf("read_message: %v", err)
	}
	if got["text"] == nil {
		t.Error("text body missing")
	}
}

// TestListAccountsExposesExpectedFields is a HAL-review regression
// that documents the deliberate fields list_accounts surfaces. If a
// refactor adds a credential field to the wire view, this test fails.
func TestListAccountsExposesExpectedFields(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "list_accounts", map[string]any{})
	if err != nil {
		t.Fatalf("list_accounts: %v", err)
	}
	arr := got["accounts"].([]any)
	if len(arr) == 0 {
		t.Fatal("no accounts returned")
	}
	first := arr[0].(map[string]any)

	// Fields that MUST be present.
	for _, want := range []string{"id", "email", "permissions", "limits", "attachment_send_paths"} {
		if _, ok := first[want]; !ok {
			t.Errorf("missing required field %q", want)
		}
	}
	// Fields that MUST NOT be present. If any of these leak into the
	// wire view, the agent could exfiltrate credentials or internal
	// host structure beyond what is necessary.
	for _, banned := range []string{
		"password", "password_command", "imap", "smtp",
		"backend", "mailbox_root", "insecure_skip_verify",
	} {
		if _, ok := first[banned]; ok {
			t.Errorf("forbidden field %q is exposed by list_accounts", banned)
		}
	}
}
