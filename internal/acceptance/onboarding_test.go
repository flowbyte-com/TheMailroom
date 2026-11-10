// Package acceptance implements the end-to-end onboarding acceptance for a
// fresh environment.
//
// It is deliberately an ordinary Go test: it builds the binary from the
// current commit with the release linker flags, runs the real setup and
// doctor commands, connects an MCP client over stdio, and exercises a
// read-only workflow against the repository's deterministic fixtures. It
// needs no network and no live mail provider.
package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/release"
)

// moduleRoot returns the repository root from this package's directory.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

// buildBinary builds the command from the current commit with the release
// linker flags, and returns its path and the metadata it should report.
//
// The linker flags matter: a plain "go build" carries the development
// defaults, so the version identity check would be meaningless.
func buildBinary(t *testing.T, root string) (string, release.Metadata) {
	t.Helper()

	meta, err := release.ResolveMetadata(root)
	if err != nil {
		t.Fatalf("resolve metadata: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "themailroom")

	args := append([]string{"build"}, release.BuildFlags()...)
	args = append(args, "-ldflags", release.LDFlags(meta), "-o", bin, "./cmd/themailroom")
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the command: %v\n%s", err, out)
	}
	return bin, meta
}

// run executes the binary and returns its combined output and exit code.
func run(t *testing.T, bin string, stdin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return string(out), code
}

// answers joins scripted answers into setup input.
func answers(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

var versionLine = regexp.MustCompile(`themailroom (\S+) \(commit ([0-9a-f]{40}), built (\S+)\)`)

// TestOnboardingAcceptance is the reproducible acceptance procedure.
func TestOnboardingAcceptance(t *testing.T) {
	if testing.Short() {
		t.Skip("the acceptance builds a binary and starts a server")
	}
	root := moduleRoot(t)
	bin, meta := buildBinary(t, root)

	t.Run("1-2 binary identity", func(t *testing.T) {
		out, code := run(t, bin, "", "-version")
		if code != 0 {
			t.Fatalf("-version exit = %d\n%s", code, out)
		}
		m := versionLine.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("-version output is not understood: %q", out)
		}
		if m[1] != meta.Version {
			t.Errorf("version = %q, want %q", m[1], meta.Version)
		}
		if m[2] != meta.Commit {
			t.Errorf("commit = %q, want %q", m[2], meta.Commit)
		}
		if m[3] != meta.BuildDate {
			t.Errorf("build date = %q, want %q", m[3], meta.BuildDate)
		}
	})

	t.Run("3-7 generic setup", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "themailroom.yaml")
		out, code := run(t, bin, answers(
			"acceptance", "acceptance@example.test",
			"mail.example.test", "1", "993", "acceptance",
			"pass show mail/acceptance",
			"n", path, "n",
		), "setup", "-config", path)
		if code != 0 {
			t.Fatalf("setup exit = %d\n%s", code, out)
		}

		// 5: the generated file is accepted by the production loader.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Parse(data)
		if err != nil {
			t.Fatalf("config.Parse rejected the generated file: %v", err)
		}

		// 6: local doctor succeeds.
		doc, docCode := run(t, bin, "", "doctor", "-config", path)
		if docCode != 0 {
			t.Fatalf("doctor exit = %d\n%s", docCode, doc)
		}

		// 7: no unsolicited sending or attachment capability.
		acc := cfg.Accounts[0]
		if len(cfg.Accounts) != 1 {
			t.Fatalf("accounts = %d, want 1", len(cfg.Accounts))
		}
		if got := strings.Join(acc.Permissions, ","); got != "read" {
			t.Errorf("permissions = %q, want read only", got)
		}
		// The loader defaults SMTP.Port and SMTP.StartTLS for every
		// account, so the absence of a configuration is shown by the host
		// and by the document itself, not by the port.
		if acc.SMTP.Host != "" || acc.SMTP.Username != "" {
			t.Errorf("SMTP was configured without being asked: %q", acc.SMTP.Host)
		}
		if strings.Contains(string(data), "smtp:") {
			t.Errorf("the generated file contains an SMTP block:\n%s", data)
		}
		if len(acc.AttachmentSendPaths) != 0 {
			t.Errorf("attachment sending was granted: %v", acc.AttachmentSendPaths)
		}
	})

	t.Run("8-9 preset and custom domain", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "themailroom.yaml")
		out, code := run(t, bin, answers(
			"acceptance", "acceptance@custom-domain.test",
			"", "", "", "",
			"pass show mail/acceptance",
			"n", path, "n",
		), "setup", "-preset", "purelymail", "-config", path)
		if code != 0 {
			t.Fatalf("setup exit = %d\n%s", code, out)
		}

		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		acc := cfg.Accounts[0]
		if acc.IMAP.Host != "imap.purelymail.com" || acc.IMAP.Port != 993 || !acc.IMAP.TLS {
			t.Errorf("IMAP = %s:%d tls=%v, want imap.purelymail.com:993 implicit TLS",
				acc.IMAP.Host, acc.IMAP.Port, acc.IMAP.TLS)
		}
		if acc.IMAP.Username != "acceptance@custom-domain.test" {
			t.Errorf("username = %q, want the full custom-domain address", acc.IMAP.Username)
		}
		if acc.Provider != "purelymail" {
			t.Errorf("provider = %q", acc.Provider)
		}
		if acc.SMTP.Host != "" {
			t.Errorf("SMTP was configured without being asked: %q", acc.SMTP.Host)
		}
		if doc, docCode := run(t, bin, "", "doctor", "-config", path); docCode != 0 {
			t.Fatalf("doctor exit = %d\n%s", docCode, doc)
		}
	})

	t.Run("10-14 MCP session and read-only workflow", func(t *testing.T) {
		cfgPath := writeFixtureConfig(t, root)
		session := connect(t, bin, cfgPath)
		defer func() {
			if err := session.Close(); err != nil {
				t.Errorf("clean shutdown: %v", err)
			}
		}()

		// 11-12: initialize happened during connect; verify the tool list.
		tools, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		got := make(map[string]bool, len(tools.Tools))
		for _, tool := range tools.Tools {
			got[tool.Name] = true
		}
		expected := []string{
			"list_accounts", "list_mailboxes", "list_messages",
			"read_message", "read_attachment", "search_messages", "send_message",
		}
		for _, name := range expected {
			if !got[name] {
				t.Errorf("the server does not advertise %q", name)
			}
		}
		if len(tools.Tools) != len(expected) {
			t.Errorf("tools = %d, want exactly %d", len(tools.Tools), len(expected))
		}

		// 13: a safe read-only workflow against the deterministic fixture.
		accounts := callTool(t, session, "list_accounts", map[string]any{})
		list, _ := accounts["accounts"].([]any)
		if len(list) != 1 {
			t.Fatalf("list_accounts accounts = %v", accounts["accounts"])
		}

		mailboxes := callTool(t, session, "list_mailboxes", map[string]any{"account_id": "acceptance"})
		boxes, _ := mailboxes["mailboxes"].([]any)
		if len(boxes) == 0 {
			t.Fatalf("list_mailboxes returned no mailboxes")
		}
		first, _ := boxes[0].(map[string]any)
		mailbox, _ := first["name"].(string)
		if mailbox == "" {
			t.Fatalf("the first mailbox has no name: %v", first)
		}

		messages := callTool(t, session, "list_messages", map[string]any{
			"account_id": "acceptance", "mailbox": mailbox, "limit": 1,
		})
		uidValidity, _ := messages["uid_validity"].(float64)
		msgs, _ := messages["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatalf("list_messages returned no messages for %q", mailbox)
		}
		summary, _ := msgs[0].(map[string]any)
		uid, _ := summary["uid"].(float64)

		message := callTool(t, session, "read_message", map[string]any{
			"account_id": "acceptance", "mailbox": mailbox,
			"uid": uid, "uid_validity": uidValidity,
		})
		if _, ok := message["headers"]; !ok {
			t.Errorf("read_message returned no headers: %v", message)
		}
	})

	t.Run("15 negative cases", func(t *testing.T) {
		t.Run("invalid configuration", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "broken.yaml")
			if err := os.WriteFile(path, []byte("accounts: [\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, code := run(t, bin, "", "doctor", "-config", path)
			if code == 0 {
				t.Fatalf("doctor accepted an invalid configuration\n%s", out)
			}
		})

		t.Run("permission denial", func(t *testing.T) {
			cfgPath := writeFixtureConfig(t, root)
			session := connect(t, bin, cfgPath)
			defer session.Close()

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "send_message",
				Arguments: map[string]any{
					"account_id": "acceptance",
					"to":         []string{"recipient@example.test"},
					"subject":    "should not be sent",
					"text":       "should not be sent",
				},
			})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !res.IsError {
				t.Fatal("send_message succeeded on a read-only account")
			}
			text := textOf(res)
			if !strings.Contains(text, "lacks capability") {
				t.Errorf("denial text = %q, want it to name the missing capability", text)
			}
		})

		t.Run("interrupted setup", func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "themailroom.yaml")

			stdin, stdinWriter, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			defer stdinWriter.Close()

			cmd := exec.Command(bin, "setup", "-config", path)
			cmd.Stdin = stdin
			var out strings.Builder
			cmd.Stdout = &out
			cmd.Stderr = &out
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			_ = stdin.Close()
			if _, err := stdinWriter.Write([]byte("acceptance\nacceptance@example.test\nmail.")); err != nil {
				t.Fatalf("write partial input: %v", err)
			}
			time.Sleep(300 * time.Millisecond)
			if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
				t.Fatalf("signal: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Errorf("interrupted setup exited 0\n%s", out.String())
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("interrupted setup did not exit within 5s\n%s", out.String())
			}

			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Errorf("interrupted setup left files behind: %v", entries)
			}
		})
	})
}

// writeFixtureConfig writes a memory-backend configuration that reads the
// repository's deterministic message fixtures.
func writeFixtureConfig(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "themailroom.yaml")
	body := fmt.Sprintf(`server:
  log_level: error
accounts:
  - id: acceptance
    backend: memory
    email: acceptance@example.test
    mailbox_root: %s
    permissions: [read]
`, filepath.Join(root, "testdata", "messages"))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture configuration: %v", err)
	}
	return path
}

// connect starts the server over stdio and completes the MCP handshake.
func connect(t *testing.T, bin, cfgPath string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	client := mcp.NewClient(&mcp.Implementation{Name: "onboarding-acceptance", Version: "1"}, nil)
	transport := &mcp.CommandTransport{Command: exec.Command(bin, "-config", cfgPath)}
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("MCP initialize: %v", err)
	}
	return session
}

// callTool calls a tool and returns its decoded structured result.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %s", name, textOf(res))
	}
	text := textOf(res)
	if text == "" {
		t.Fatalf("%s returned no content", name)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("%s returned content that is not JSON: %v\n%s", name, err, text)
	}
	return decoded
}

// textOf returns the first text content of a tool result.
func textOf(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			return text.Text
		}
	}
	return ""
}
