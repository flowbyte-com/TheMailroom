package docscheck

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// skillPath is the repository-owned agent skill.
const skillPath = "../../skills/themailroom/SKILL.md"

// repositoryPath is the repository the documentation belongs to. A
// version-pinned link to another repository would not be verifiable here.
const repositoryPath = "flowbyte-com/TheMailroom"

// markdownLink matches a markdown link target.
var markdownLink = regexp.MustCompile(`\]\(([^)]+)\)`)

// bareURL matches a URL written as plain text, for example inside a
// shell command in a code block.
var bareURL = regexp.MustCompile("https?://[^\\s\"')\\]]+")

// candidateURLs returns every URL a document refers to: the targets of
// its markdown links and the URLs written as plain text. The same URL
// found twice is reported once.
func candidateURLs(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(target string) {
		target = strings.TrimSpace(target)
		if target == "" || seen[target] {
			return
		}
		seen[target] = true
		out = append(out, target)
	}
	for _, m := range markdownLink.FindAllStringSubmatch(text, -1) {
		target := m[1]
		if i := strings.IndexByte(target, '#'); i >= 0 {
			target = target[:i]
		}
		add(target)
	}
	for _, m := range bareURL.FindAllString(text, -1) {
		add(m)
	}
	return out
}

// skillFrontmatter is the YAML block at the top of SKILL.md.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	License     string `yaml:"license"`
}

// readSkill reads the skill and splits its frontmatter from its body.
func readSkill(t *testing.T) (skillFrontmatter, string, string) {
	t.Helper()
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillPath, err)
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatalf("%s does not start with a YAML frontmatter block", skillPath)
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("%s has an unterminated frontmatter block", skillPath)
	}
	front := rest[:end]
	body := rest[end+len("\n---\n"):]

	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		t.Fatalf("%s frontmatter is not valid YAML: %v", skillPath, err)
	}
	return fm, front, body
}

// readVersion returns the version the repository is preparing. The
// documentation references are pinned to it, so the two must agree.
func readVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../VERSION")
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	version := strings.TrimSpace(string(data))
	if version == "" {
		t.Fatal("VERSION is empty")
	}
	return version
}

// pinnedLink is a parsed version-pinned link into this repository.
type pinnedLink struct {
	// URL is the link as written.
	URL string
	// Ref is the git reference the link is pinned to, for example
	// "v1.0.0-rc.2".
	Ref string
	// Path is the repository path the link points at, for example
	// "docs/TOOLS.md". It is empty for a link to an archive, which
	// carries no repository path.
	Path string
	// Archive reports whether the link is a source archive rather than a
	// file.
	Archive bool
}

// parsePinnedLink parses a link that is pinned to a reference of this
// repository. It reports ok=false for anything else: an unpinned link, a
// link to another repository, or a link that is not a file or an archive.
//
// Both the raw form and the browsable form are recognised, so the test
// constrains the pin rather than one particular spelling of the URL.
func parsePinnedLink(target string) (pinnedLink, bool) {
	u, err := url.Parse(target)
	if err != nil {
		return pinnedLink{}, false
	}

	var segs []string
	switch u.Host {
	case "raw.githubusercontent.com":
		segs = strings.Split(strings.Trim(u.Path, "/"), "/")
	case "github.com":
		segs = strings.Split(strings.Trim(u.Path, "/"), "/")
		switch {
		case len(segs) >= 4 && segs[2] == "blob":
			segs = append(segs[:2:2], segs[3:]...)
		case len(segs) >= 5 && segs[2] == "archive":
			// /owner/repo/archive/refs/tags/<ref>.tar.gz
			if segs[3] != "refs" || segs[4] != "tags" {
				return pinnedLink{}, false
			}
			ref := strings.TrimSuffix(strings.TrimPrefix(segs[5], "v"), ".tar.gz")
			if len(segs) != 6 {
				return pinnedLink{}, false
			}
			if segs[0]+"/"+segs[1] != repositoryPath {
				return pinnedLink{}, false
			}
			return pinnedLink{URL: target, Ref: "v" + ref, Archive: true}, true
		default:
			return pinnedLink{}, false
		}
	default:
		return pinnedLink{}, false
	}

	if len(segs) < 4 {
		return pinnedLink{}, false
	}
	if segs[0]+"/"+segs[1] != repositoryPath {
		return pinnedLink{}, false
	}
	return pinnedLink{URL: target, Ref: segs[2], Path: strings.Join(segs[3:], "/")}, true
}

// checkPinnedLinks verifies every version-pinned link in text.
//
// It checks that the reference the link is pinned to is the version the
// repository is preparing, and that the repository path it points at
// exists. A link that names a file which does not exist, or that is
// pinned to the wrong release, would send a reader to a document that is
// not there or is not the one this release documents.
//
// It returns the number of pinned links it found, so a caller can require
// that a document carries any at all.
func checkPinnedLinks(t *testing.T, source, text string) int {
	t.Helper()
	want := "v" + readVersion(t)
	found := 0

	for _, target := range candidateURLs(text) {
		pinned, ok := parsePinnedLink(target)
		if !ok {
			continue
		}
		found++
		if pinned.Ref != want {
			t.Errorf("%s pins %q to %q, but this release is %q", source, pinned.URL, pinned.Ref, want)
		}
		if pinned.Path == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join("../..", filepath.FromSlash(pinned.Path))); err != nil {
			t.Errorf("%s pins %q to %q, which does not exist in the repository: %v",
				source, pinned.URL, pinned.Path, err)
		}
	}
	return found
}

// TestSkillFrontmatterIsValid covers the Agent Skills frontmatter: a name
// that matches the directory, a description that says when to use the
// skill, and a licence.
func TestSkillFrontmatterIsValid(t *testing.T) {
	fm, _, body := readSkill(t)

	if fm.Name != "themailroom" {
		t.Errorf("name = %q, want themailroom", fm.Name)
	}
	if dir := filepath.Base(filepath.Dir(skillPath)); dir != fm.Name {
		t.Errorf("the skill directory %q does not match name %q", dir, fm.Name)
	}
	if len(strings.TrimSpace(fm.Description)) < 40 {
		t.Errorf("description is too short to say when to use the skill: %q", fm.Description)
	}
	if !strings.Contains(strings.ToLower(fm.Description), "use when") {
		t.Errorf("description does not say when to use the skill: %q", fm.Description)
	}
	if fm.License == "" {
		t.Error("license is empty")
	}
	if strings.TrimSpace(body) == "" {
		t.Error("the skill body is empty")
	}
}

// TestSkillReferencesAreVersionPinned covers the skill's documentation
// references.
//
// The skill is distributed as a single file that an operator may copy out
// of a checkout, so a repository-relative reference such as
// ../../docs/TOOLS.md would break the moment the file is moved. Every
// reference must therefore be pinned to the release this repository is
// preparing, and it must name a file that exists.
func TestSkillReferencesAreVersionPinned(t *testing.T) {
	_, _, body := readSkill(t)

	relative := 0
	for _, target := range candidateURLs(body) {
		if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			if _, ok := parsePinnedLink(target); !ok {
				t.Errorf("the skill links to %q, which is not pinned to a release of %s", target, repositoryPath)
			}
			continue
		}
		relative++
		t.Errorf("the skill links to %q, which depends on a checkout layout; the skill must be usable as a standalone file",
			target)
	}
	if relative > 0 {
		t.Errorf("the skill has %d relative reference(s); it must have none", relative)
	}

	if found := checkPinnedLinks(t, "the skill", body); found == 0 {
		t.Error("the skill has no version-pinned documentation references")
	}

	// The skill is read both before the tag is pushed and after the
	// release is published, so it must not state how things stand at the
	// moment of writing.
	checkNoStateBoundPhrases(t, "the skill", body)
	lower := strings.ToLower(flatten(body))
	for _, want := range []string{
		"become available once that tag exists",
		"for an untagged development build",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the skill does not give the conditional wording %q", want)
		}
	}
}

// TestSkillIsStandalone covers the skill as a file on its own: it must
// parse, carry its frontmatter, and depend on no sibling file.
func TestSkillIsStandalone(t *testing.T) {
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read %s: %v", skillPath, err)
	}

	// Copy the skill alone into an empty directory: whatever it needs, it
	// must not need from a neighbour.
	dir := t.TempDir()
	installed := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(installed, data, 0o644); err != nil {
		t.Fatalf("write the standalone skill: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the standalone directory holds %d entries, want 1", len(entries))
	}

	// The installed file must still parse, with its frontmatter intact.
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatal("the standalone skill does not start with a frontmatter block")
	}
	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatal("the standalone skill has an unterminated frontmatter block")
	}
	var fm skillFrontmatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		t.Fatalf("the standalone frontmatter is not valid YAML: %v", err)
	}
	if fm.Name != "themailroom" {
		t.Errorf("the standalone skill name = %q, want themailroom", fm.Name)
	}
	if fm.Description == "" || fm.License == "" {
		t.Error("the standalone skill lost its description or its licence")
	}

	// Nothing in it may refer to a file next to it.
	body := rest[end+len("\n---\n"):]
	for _, target := range candidateURLs(body) {
		if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") ||
			strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
			continue
		}
		t.Errorf("the standalone skill refers to %q, which is not available beside it", target)
	}
}

// TestSkillNamesTheRealToolSurface pins the seven tools and the commands the
// skill tells an agent to use.
func TestSkillNamesTheRealToolSurface(t *testing.T) {
	_, _, body := readSkill(t)

	for _, tool := range []string{
		"list_accounts", "list_mailboxes", "list_messages", "read_message",
		"read_attachment", "search_messages", "send_message",
	} {
		if !strings.Contains(body, tool) {
			t.Errorf("the skill does not name the %s tool", tool)
		}
	}
	for _, command := range []string{"themailroom setup", "themailroom doctor", "-preset purelymail"} {
		if !strings.Contains(body, command) {
			t.Errorf("the skill does not mention %q", command)
		}
	}
}

// TestSkillStatesTheSafetyRules covers the operating rules that must not be
// lost from the skill.
func TestSkillStatesTheSafetyRules(t *testing.T) {
	_, _, body := readSkill(t)
	lower := strings.ToLower(body)

	cases := map[string]string{
		"password in chat":        "password",
		"credential command":      "credential command",
		"explicit authorisation":  "explicit",
		"ambiguous retry":         "ambiguous",
		"not proof of delivery":   "proof of delivery",
		"uidvalidity":             "uid_validity",
		"untrusted email content": "untrusted",
	}
	for name, want := range cases {
		if !strings.Contains(lower, want) {
			t.Errorf("the skill does not cover %s (missing %q)", name, want)
		}
	}
	if !strings.Contains(lower, "not implemented") {
		t.Error("the skill does not say that reply, forward and mutation are not implemented")
	}
}

// TestSkillDescribesTheToolErrorContract pins the tool-error contract that
// docs/TOOLS.md establishes: a tool execution failure sets isError and
// carries human-readable prose in content, the typed business-error codes
// stay in the server-side audit log, a tool error has no structuredContent,
// and a malformed protocol request is a JSON-RPC error rather than a
// result.
//
// An earlier revision of the skill claimed the client-visible content
// carries a typed code. That is false, and an agent acting on it would parse
// prose for a field that is not there, so the wording is pinned here.
func TestSkillDescribesTheToolErrorContract(t *testing.T) {
	_, _, body := readSkill(t)
	lower := strings.ToLower(body)

	// The facts the skill must state.
	for _, want := range []string{
		"iserror",           // the portable error flag
		"structuredcontent", // structured output exists, and only on success
		"json-rpc error",    // protocol failures are not tool results
		"audit log",         // where the typed codes actually live
		"client payload",    // the codes are absent from it
		"prose",             // the client-visible text is human-readable
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("the skill does not state %q", want)
		}
	}

	// The claims that are false and must not come back.
	for _, forbidden := range []string{
		"content carries a `code`",
		"carries a `code` such as",
		"content carries a code",
	} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("the skill claims the client-visible content carries a typed code (%q)", forbidden)
		}
	}

	// It must point at the authoritative contract, by a reference that
	// survives being copied out of a checkout.
	pinned := false
	for _, target := range candidateURLs(body) {
		link, ok := parsePinnedLink(target)
		if !ok {
			continue
		}
		if link.Path == "docs/TOOLS.md" {
			pinned = true
		}
	}
	if !pinned {
		t.Error("the skill does not link to docs/TOOLS.md")
	}
}

// TestSkillDoesNotClaimUnsafeAutomation covers claims that would mislead an
// agent into believing it can act on its own.
func TestSkillDoesNotClaimUnsafeAutomation(t *testing.T) {
	_, _, body := readSkill(t)
	lower := strings.ToLower(body)

	if strings.Contains(lower, "automatically install") {
		t.Error("the skill claims it can install automatically")
	}
	if strings.Contains(lower, "your password:") {
		t.Error("the skill asks for a password")
	}
	if strings.Contains(lower, "retry the submission") || strings.Contains(lower, "retry automatically") {
		t.Error("the skill suggests retrying a submission")
	}
}

// TestOnboardingDocumentationLinksResolve checks relative links in the
// onboarding documents, so a moved file cannot leave a dangling pointer,
// and checks the version-pinned links they carry.
func TestOnboardingDocumentationLinksResolve(t *testing.T) {
	documents := []string{
		"../../README.md",
		"../../docs/INSTALL.md",
		"../../docs/ACCEPTANCE.md",
		"../../docs/PROVIDERS.md",
	}

	for _, doc := range documents {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		text := string(data)
		for _, m := range markdownLink.FindAllStringSubmatch(text, -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if i := strings.IndexByte(target, '#'); i >= 0 {
				target = target[:i]
			}
			if strings.TrimSpace(target) == "" {
				continue
			}
			path := filepath.Join(filepath.Dir(doc), target)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s links to %q, which does not exist: %v", doc, m[1], err)
			}
		}
		checkPinnedLinks(t, doc, text)
	}
}

// TestSkillInstallationDocumentationIsPinned covers the standalone skill
// installation procedure: the URLs it offers must be pinned to the release
// this repository is preparing, so they cannot point at a different build.
func TestSkillInstallationDocumentationIsPinned(t *testing.T) {
	data, err := os.ReadFile("../../docs/INSTALL.md")
	if err != nil {
		t.Fatalf("read docs/INSTALL.md: %v", err)
	}
	text := string(data)

	found := checkPinnedLinks(t, "docs/INSTALL.md", text)
	if found == 0 {
		t.Error("docs/INSTALL.md has no version-pinned skill URLs")
	}

	// The procedure must cover the whole path, and must be explicit that
	// the skill does not bring the executable or the credentials.
	for _, want := range []string{
		"Install the agent skill",
		"SKILL.md",
		"frontmatter",
		"raw.githubusercontent.com",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("docs/INSTALL.md does not mention %q", want)
		}
	}
	// The procedure is read before the tag exists and after the release
	// is published, so it must not state the current publication state.
	checkNoStateBoundPhrases(t, "docs/INSTALL.md", text)

	lower := strings.ToLower(flatten(text))
	for _, want := range []string{
		"become available once that tag exists",
		"for an untagged development build",
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("docs/INSTALL.md does not give the conditional wording %q", want)
		}
	}
	if !strings.Contains(lower, "does not install the executable") {
		t.Error("docs/INSTALL.md does not say that installing the skill does not install the executable")
	}
	if !strings.Contains(lower, "does not") || !strings.Contains(lower, "configure credentials") {
		t.Error("docs/INSTALL.md does not say that installing the skill does not configure credentials")
	}
}
