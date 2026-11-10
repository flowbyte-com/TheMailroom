package docscheck

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// skillPath is the repository-owned agent skill.
const skillPath = "../../skills/themailroom/SKILL.md"

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

// TestSkillReferencesResolve covers every relative link in the skill, so a
// moved document cannot leave a dangling reference.
func TestSkillReferencesResolve(t *testing.T) {
	_, _, body := readSkill(t)

	link := regexp.MustCompile(`\]\(([^)]+)\)`)
	found := 0
	for _, m := range link.FindAllStringSubmatch(body, -1) {
		target := m[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		found++
		// Strip any fragment.
		if i := strings.IndexByte(target, '#'); i >= 0 {
			target = target[:i]
		}
		path := filepath.Join(filepath.Dir(skillPath), target)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("the skill links to %q, which does not exist: %v", m[1], err)
		}
	}
	if found == 0 {
		t.Error("the skill has no relative documentation references")
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
// onboarding documents, so a moved file cannot leave a dangling pointer.
func TestOnboardingDocumentationLinksResolve(t *testing.T) {
	documents := []string{
		"../../README.md",
		"../../docs/INSTALL.md",
		"../../docs/ACCEPTANCE.md",
		"../../docs/PROVIDERS.md",
	}
	link := regexp.MustCompile(`\]\(([^)]+)\)`)

	for _, doc := range documents {
		data, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for _, m := range link.FindAllStringSubmatch(string(data), -1) {
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
	}
}
