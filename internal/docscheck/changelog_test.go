package docscheck

import (
	"os"
	"strings"
	"testing"
)

// changelogPath is the project changelog.
const changelogPath = "../../CHANGELOG.md"

// flatten collapses whitespace so that a phrase is matched the same way
// however the source wraps it.
func flatten(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// readChangelog reads the changelog.
func readChangelog(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(changelogPath)
	if err != nil {
		t.Fatalf("read %s: %v", changelogPath, err)
	}
	return string(data)
}

// changelogSection returns the part of the changelog under the given
// heading, up to the next heading of the same or a higher level.
func changelogSection(t *testing.T, text, heading string) string {
	t.Helper()
	start := strings.Index(text, heading)
	if start < 0 {
		t.Fatalf("the changelog has no section %q", heading)
	}
	rest := text[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// TestChangelogRC2StatesThePublicationRules pins the way the RC2 section
// describes configuration publication.
//
// The rules are the ones the implementation actually applies, and they
// are not uniform: the publish is atomic on POSIX but not on the
// exclusive-create fallback, an ordinary setup and a forced replacement
// are governed by different directory rules, and a symbolic link is
// refused outright. A summary that flattens those differences would
// describe a system that does not exist.
func TestChangelogRC2StatesThePublicationRules(t *testing.T) {
	section := changelogSection(t, readChangelog(t), "## [1.0.0-rc.2]")
	lower := strings.ToLower(flatten(section))

	required := map[string]string{
		"hard-link publish":                 "the publish is a hard link",
		"hard link is atomic":               "it is atomic",
		"fallback is an exclusive create":   "the fallback is an exclusive",
		"fallback is not atomic":            "it is not atomic",
		"existing destination refused":      "an existing destination is refused unless `-force`",
		"symlink refused even with force":   "a symbolic link is refused even with `-force`",
		"non-sticky world-writable refused": "world-writable and not sticky",
		"force needs a private directory":   "group and other users cannot write to",
		"mode 0600":                         "mode `0600`",
		"windows refuses":                   "windows setup refuses to write",
	}
	for name, want := range required {
		if !strings.Contains(lower, want) {
			t.Errorf("the RC2 section does not state the %s rule (missing %q)", name, want)
		}
	}

	// The unqualified claims that are false.
	forbidden := map[string]string{
		"the publish is always atomic":                      "publishes it atomically",
		"the diagnostic command is the only socket opener":  "is the only path that opens a socket",
		"the credential is resolved only at authentication": "resolved only at the authentication stage",
	}
	for name, want := range forbidden {
		if strings.Contains(lower, want) {
			t.Errorf("the RC2 section still claims %s (%q)", name, want)
		}
	}
}

// TestChangelogRC2StatesTheDiagnosticBoundary pins the distinction the
// reviewer drew: the local doctor opens no connection and runs no
// credential command, `-network` explicitly enables the network checks,
// and the MCP server is not thereby prevented from opening sockets.
func TestChangelogRC2StatesTheDiagnosticBoundary(t *testing.T) {
	section := changelogSection(t, readChangelog(t), "## [1.0.0-rc.2]")
	lower := strings.ToLower(flatten(section))

	for name, want := range map[string]string{
		"local opens no connection":        "open no network connection",
		"local runs no credential command": "execute no credential command",
		"network is explicit":              "explicitly",
		"the server is not restricted":     "the mcp server opens connections",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the RC2 section does not state the %s point (missing %q)", name, want)
		}
	}
}

// TestChangelogRC2DoesNotClaimToBePublished pins the release status.
//
// The version is prepared, but no RC2 release exists yet, so the section
// must not read as though one had been published.
func TestChangelogRC2DoesNotClaimToBePublished(t *testing.T) {
	section := changelogSection(t, readChangelog(t), "## [1.0.0-rc.2]")
	flat := flatten(section)

	if !strings.Contains(flat, "No RC2 release exists yet") {
		t.Error("the RC2 section does not say that no RC2 release exists yet")
	}
	if !strings.Contains(flat, "prepared as `1.0.0-rc.2`") {
		t.Error("the RC2 section does not say that the version is prepared")
	}
	if !strings.Contains(flat, "`v1.0.0-rc.2` tag") {
		t.Error("the RC2 section does not name the future tag")
	}

	lower := strings.ToLower(flat)
	for _, forbidden := range []string{
		"the release is published under",
		"is published under the `v1.0.0-rc.2` tag",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("the RC2 section claims the release is published (%q)", forbidden)
		}
	}
}

// TestChangelogKeepsTheRC1HistoryAndTheInventoryCorrection pins the two
// things the correction must not disturb: the historical RC1 entry, and
// the correction that adds read_attachment to the V1 inventory without
// claiming it arrived after RC1.
func TestChangelogKeepsTheRC1HistoryAndTheInventoryCorrection(t *testing.T) {
	text := readChangelog(t)

	if !strings.Contains(text, "## [1.0.0-rc.1] - 2026-11-10") {
		t.Error("the historical RC1 entry is missing")
	}
	rc1 := flatten(changelogSection(t, text, "## [1.0.0-rc.1]"))
	if !strings.Contains(rc1, "The first release candidate of TheMailroom V1") {
		t.Error("the RC1 entry no longer reads as the first release candidate")
	}
	if !strings.Contains(rc1, "published under the `v1.0.0-rc.1` tag") {
		t.Error("the RC1 entry lost its statement about the tag it was published under")
	}

	// The V1 inventory lists six read tools.
	inventory := flatten(changelogSection(t, text, "## [1.0.0] - Unreleased"))
	if !strings.Contains(inventory, "`read_attachment`") {
		t.Error("the V1 inventory does not list read_attachment")
	}
	for _, tool := range []string{
		"`list_accounts`", "`list_mailboxes`", "`list_messages`",
		"`read_message`", "`read_attachment`", "`search_messages`",
	} {
		if !strings.Contains(inventory, tool) {
			t.Errorf("the V1 inventory does not list %s", tool)
		}
	}

	// The correction says it was present in RC1, so no claim is made that
	// it arrived afterwards.
	rc2 := flatten(changelogSection(t, text, "## [1.0.0-rc.2]"))
	if !strings.Contains(rc2, "It was present in `v1.0.0-rc.1`") {
		t.Error("the RC2 section does not say that read_attachment was present in RC1")
	}
	if strings.Contains(rc2, "added `read_attachment`") ||
		strings.Contains(rc2, "introduced `read_attachment`") {
		t.Error("the RC2 section claims read_attachment was introduced after RC1")
	}
}
