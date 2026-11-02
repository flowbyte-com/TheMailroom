package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// The wire shape of list_accounts and the message summaries is a public
// contract: agents read these fields to decide what to do next. The
// regressions below pin that shape so a change to the view structs
// cannot silently alter it.

// TestListAccountsExposesEffectiveListScanBudget pins that
// list_accounts reports the scan budget the server actually enforces.
//
// The field was previously absent from the MCP wire shape even though
// the documented account limits included it, so an agent could not read
// the budget it was being refused against.
func TestListAccountsExposesEffectiveListScanBudget(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "list_accounts", map[string]any{})
	if err != nil {
		t.Fatalf("list_accounts: %v", err)
	}
	accounts, ok := got["accounts"].([]any)
	if !ok {
		t.Fatalf("accounts missing or wrong type: %T", got["accounts"])
	}
	if len(accounts) == 0 {
		t.Fatal("no accounts returned")
	}
	for _, a := range accounts {
		m := a.(map[string]any)
		limits, ok := m["limits"].(map[string]any)
		if !ok {
			t.Fatalf("limits missing or wrong type: %T", m["limits"])
		}
		budget, ok := limits["max_list_scan_messages"]
		if !ok {
			t.Fatalf("limits.max_list_scan_messages is absent; got limits %v", limits)
		}
		// The default configuration applies no explicit limit, so the
		// effective budget is the documented default.
		if budget != float64(10000) {
			t.Errorf("max_list_scan_messages = %v, want 10000", budget)
		}
	}
}

// TestListAccountsReportsConfiguredListScanBudget pins that an
// explicitly configured budget is reported verbatim, so the number an
// operator reads is the number the server enforces.
func TestListAccountsReportsConfiguredListScanBudget(t *testing.T) {
	env := setupEnvWithLimits(t, "max_list_scan_messages: 4242")
	defer env.cleanup()

	got, err := env.call(t, "list_accounts", map[string]any{})
	if err != nil {
		t.Fatalf("list_accounts: %v", err)
	}
	limits := accountLimits(t, got, "full")
	if got := limits["max_list_scan_messages"]; got != float64(4242) {
		t.Errorf("max_list_scan_messages = %v, want 4242", got)
	}
}

// TestListAccountsListScanBudgetMatchesEnforcedBudget pins that the
// reported budget equals the budget the IMAP client enforces for the
// same account. The two are resolved through one helper, so they
// cannot drift apart.
func TestListAccountsListScanBudgetMatchesEnforcedBudget(t *testing.T) {
	env := setupEnvWithLimits(t, "max_list_scan_messages: 777")
	defer env.cleanup()

	got, err := env.call(t, "list_accounts", map[string]any{})
	if err != nil {
		t.Fatalf("list_accounts: %v", err)
	}
	limits := accountLimits(t, got, "full")
	reported := limits["max_list_scan_messages"].(float64)

	acc, err := env.registry.Get("full")
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	enforced := float64(acc.Limits.EffectiveMaxListScanMessages())
	if reported != enforced {
		t.Errorf("reported budget %v != enforced budget %v", reported, enforced)
	}
}

// accountLimits returns the limits object of the named account in a
// list_accounts result. The accounts are not returned in a guaranteed
// order, so a test must look one up by ID.
func accountLimits(t *testing.T, got map[string]any, id string) map[string]any {
	t.Helper()
	accounts, ok := got["accounts"].([]any)
	if !ok {
		t.Fatalf("accounts missing or wrong type: %T", got["accounts"])
	}
	for _, a := range accounts {
		m := a.(map[string]any)
		if m["id"] == id {
			limits, ok := m["limits"].(map[string]any)
			if !ok {
				t.Fatalf("limits missing or wrong type: %T", m["limits"])
			}
			return limits
		}
	}
	t.Fatalf("account %q not found in %v", id, accounts)
	return nil
}

// TestListMessagesWireShape pins the exact set of keys a message
// summary carries.
//
// The in-memory backend parses every message, so it knows whether
// attachments are present and must say so. The key is present with
// either value; it is never omitted for a known answer.
//
// The optional keys carry omitempty semantics: a summary with no
// recipients, flags or subject omits them.
func TestListMessagesWireShape(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "list_messages", map[string]any{
		"account_id": "full",
		"mailbox":    "INBOX",
		"limit":      float64(50),
	})
	if err != nil {
		t.Fatalf("list_messages: %v", err)
	}
	messages := got["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("no messages returned")
	}

	required := []string{"uid", "internal_date", "size", "has_attachments"}
	optional := []string{"flags", "from", "to", "subject"}
	known := map[string]bool{}
	for _, k := range append(append([]string{}, required...), optional...) {
		known[k] = true
	}

	sawTrue, sawFalse := false, false
	for _, item := range messages {
		m := item.(map[string]any)
		for _, k := range required {
			if _, ok := m[k]; !ok {
				t.Errorf("summary key %q is missing: %v", k, m)
			}
		}
		for k := range m {
			if !known[k] {
				t.Errorf("unexpected summary key %q", k)
			}
		}
		has, ok := m["has_attachments"]
		if !ok {
			t.Fatalf("has_attachments is absent; the memory backend knows the answer: %v", m)
		}
		switch has {
		case true:
			sawTrue = true
		case false:
			sawFalse = true
		default:
			t.Errorf("has_attachments = %v (%T), want a boolean", has, has)
		}
	}
	if !sawTrue {
		t.Error("no message reported has_attachments=true; the multipart fixture has attachments")
	}
	if !sawFalse {
		t.Error("no message reported has_attachments=false; the plain fixture has none")
	}
}

// TestListMessagesOmitsUnknownHasAttachments pins that a summary whose
// attachment presence the backend did not determine omits the field
// rather than reporting false.
//
// A false value asserts that the message has no attachments. For the
// IMAP list and search paths, which fetch envelopes only, that is not
// known, so the field must be absent.
func TestListMessagesOmitsUnknownHasAttachments(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	// Replace the memory backend with one whose summaries leave the
	// attachment presence unknown, exactly as the IMAP list and search
	// paths do.
	acc, err := env.registry.Get("full")
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	acc.SetBackendForTest(&unknownAttachmentBackend{Backend: acc.Backend()})

	got, err := env.call(t, "list_messages", map[string]any{
		"account_id": "full",
		"mailbox":    "INBOX",
		"limit":      float64(50),
	})
	if err != nil {
		t.Fatalf("list_messages: %v", err)
	}
	messages := got["messages"].([]any)
	if len(messages) == 0 {
		t.Fatal("no messages returned")
	}
	for _, item := range messages {
		m := item.(map[string]any)
		if v, ok := m["has_attachments"]; ok {
			t.Errorf("has_attachments = %v, want the field omitted when unknown", v)
		}
	}
}

// TestSearchMessagesOmitsUnknownHasAttachments is the search-path
// counterpart of TestListMessagesOmitsUnknownHasAttachments.
func TestSearchMessagesOmitsUnknownHasAttachments(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	acc, err := env.registry.Get("full")
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	acc.SetBackendForTest(&unknownAttachmentBackend{Backend: acc.Backend()})

	got, err := env.call(t, "search_messages", map[string]any{
		"account_id": "full",
		"mailbox":    "INBOX",
		"query":      map[string]any{"subject": "Welcome"},
	})
	if err != nil {
		t.Fatalf("search_messages: %v", err)
	}
	matches := got["matches"].([]any)
	if len(matches) == 0 {
		t.Fatal("no matches returned")
	}
	for _, item := range matches {
		m := item.(map[string]any)
		if v, ok := m["has_attachments"]; ok {
			t.Errorf("has_attachments = %v, want the field omitted when unknown", v)
		}
	}
}

// TestListAccountsWireShape pins the exact key set of an account entry.
func TestListAccountsWireShape(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "list_accounts", map[string]any{})
	if err != nil {
		t.Fatalf("list_accounts: %v", err)
	}
	accounts := got["accounts"].([]any)
	if len(accounts) == 0 {
		t.Fatal("no accounts returned")
	}

	m := accounts[0].(map[string]any)
	want := map[string]bool{
		"id": true, "email": true, "permissions": true,
		"limits": true, "attachment_send_enabled": true,
	}
	// provider is omitempty and is empty for a memory account.
	want["provider"] = true
	for k := range m {
		if !want[k] {
			t.Errorf("unexpected account key %q", k)
		}
		delete(want, k)
	}
	for k := range want {
		if k == "provider" {
			continue
		}
		t.Errorf("account key %q is missing", k)
	}

	limits := m["limits"].(map[string]any)
	wantLimits := map[string]bool{
		"max_message_bytes": true, "max_attachment_bytes": true,
		"max_total_attachment_bytes": true, "max_attachments": true,
		"max_recipients": true, "max_search_results": true,
		"max_batch_operations": true, "max_list_scan_messages": true,
		"max_mime_nesting": true, "max_mime_parts": true,
	}
	for k := range limits {
		if !wantLimits[k] {
			t.Errorf("unexpected limits key %q", k)
		}
		delete(wantLimits, k)
	}
	for k := range wantLimits {
		t.Errorf("limits key %q is missing", k)
	}
}

// TestListAccountsWireShapeIsJSONSerialisable guards the wire shape
// against a value that would serialise to something other than a
// number, such as a float rendered in exponent form.
func TestListAccountsWireShapeIsJSONSerialisable(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	got, err := env.call(t, "list_accounts", map[string]any{})
	if err != nil {
		t.Fatalf("list_accounts: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"max_list_scan_messages":10000`) {
		t.Errorf("wire JSON does not carry the scan budget as a plain number: %s", raw)
	}
}
