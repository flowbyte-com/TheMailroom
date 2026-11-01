package docscheck

import (
	"os"
	"strings"
	"testing"
)

// TestNoObsoleteNoDeadlineClaims pins the three stale statements that
// survived the Phase 3 PR 6 documentation update. The implemented policy
// is stated in TestDeadlineDocumentationStatesTheImplementedPolicy.
func TestNoObsoleteNoDeadlineClaims(t *testing.T) {
	obsolete := []struct {
		file   string
		phrase string
	}{
		{
			file:   "../../README.md",
			phrase: "no command deadline is in force today",
		},
		{
			file:   "../../README.md",
			phrase: "the optional per-command timeout is not set by the production configuration",
		},
		{
			file:   "../../docs/ARCHITECTURE.md",
			phrase: "no command deadline is in force today",
		},
		{
			file:   "../../docs/ARCHITECTURE.md",
			phrase: "the per-command IMAP timeout is an optional control that the production construction path does not set",
		},
		{
			file:   "../../docs/THREAT_MODEL.md",
			phrase: "a command deadline is an optional control that is not configured by default",
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

// TestDeadlineDocumentationStatesTheImplementedPolicy ensures the
// correction did not merely delete the stale text: the operator-facing
// documents must still describe the implemented finite deadlines.
func TestDeadlineDocumentationStatesTheImplementedPolicy(t *testing.T) {
	required := []struct {
		file   string
		phrase string
	}{
		{
			file:   "../../README.md",
			phrase: "The IMAP deadlines are finite and always active",
		},
		{
			file:   "../../docs/ARCHITECTURE.md",
			phrase: "Every operation has an absolute end-to-end deadline",
		},
		{
			file:   "../../docs/THREAT_MODEL.md",
			phrase: "Command and operation deadlines are implemented and always finite.",
		},
	}
	for _, tc := range required {
		data, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		if !containsNormalized(data, tc.phrase) {
			t.Errorf("%s does not state the implemented deadline policy: missing %q", tc.file, tc.phrase)
		}
	}
}

// containsNormalized compares text and phrase after collapsing every run
// of whitespace, because Markdown wraps the same sentence across lines.
func containsNormalized(data []byte, phrase string) bool {
	text := strings.Join(strings.Fields(string(data)), " ")
	needle := strings.Join(strings.Fields(phrase), " ")
	return strings.Contains(strings.ToLower(text), strings.ToLower(needle))
}
