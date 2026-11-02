package docscheck

import (
	"os"
	"testing"
)

// TestNoObsoleteHasAttachmentsClaims pins the statements that became
// inaccurate when the summary's attachment presence was made tri-state.
//
// An IMAP list or search summary fetches envelopes only, so it does not
// determine whether a message has attachments. The field is now omitted
// in that case. A document that still says the field is "always false"
// states the opposite of the implemented policy: it presents an
// undetermined answer as a fact.
func TestNoObsoleteHasAttachmentsClaims(t *testing.T) {
	obsolete := []struct {
		file   string
		phrase string
	}{
		{
			file:   "../../docs/TOOLS.md",
			phrase: "has_attachments`; it is always `false`",
		},
		{
			file:   "../../docs/PHASE2_DESIGN.md",
			phrase: "do not populate `has_attachments` (it is always false)",
		},
	}
	for _, tc := range obsolete {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		if containsNormalized(data, tc.phrase) {
			t.Errorf("%s still contains the obsolete claim %q", tc.file, tc.phrase)
		}
	}
}

// TestHasAttachmentsDocumentationStatesTheImplementedPolicy ensures the
// correction did not merely delete the stale text: the operator-facing
// documents must still describe how attachment presence is reported.
func TestHasAttachmentsDocumentationStatesTheImplementedPolicy(t *testing.T) {
	required := []struct {
		file   string
		phrase string
	}{
		{
			file:   "../../docs/TOOLS.md",
			phrase: "therefore **omitted** from an IMAP summary rather than reported as `false`",
		},
		{
			file:   "../../README.md",
			phrase: "so they omit the field rather than assert `false`",
		},
	}
	for _, tc := range required {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		if !containsNormalized(data, tc.phrase) {
			t.Errorf("%s does not state the implemented attachment-presence policy: missing %q", tc.file, tc.phrase)
		}
	}
}

// TestScanBudgetDocumentationStatesTheImplementedPolicy pins that the
// documents state that list_accounts reports the effective scan
// budget, so an operator can read the budget the server enforces.
func TestScanBudgetDocumentationStatesTheImplementedPolicy(t *testing.T) {
	required := []struct {
		file   string
		phrase string
	}{
		{
			file:   "../../docs/TOOLS.md",
			phrase: "The effective budget is reported by `list_accounts` under `limits.max_list_scan_messages`",
		},
		{
			file:   "../../README.md",
			phrase: "The effective budget is reported by `list_accounts`",
		},
		{
			file:   "../../docs/ARCHITECTURE.md",
			phrase: "the budget `list_accounts` reports under `limits.max_list_scan_messages` are the same number",
		},
	}
	for _, tc := range required {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		if !containsNormalized(data, tc.phrase) {
			t.Errorf("%s does not state that list_accounts reports the effective scan budget: missing %q", tc.file, tc.phrase)
		}
	}
}

// containsNormalized compares text and phrase after collapsing every run
// of whitespace, because Markdown wraps the same sentence across lines.
// It is defined in docs_test.go and shared with this file.
