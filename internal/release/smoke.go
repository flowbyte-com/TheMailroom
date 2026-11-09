package release

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SmokeOptions configures the clean-install smoke test.
type SmokeOptions struct {
	// Binary is the artifact to install and run.
	Binary string
	// Metadata is the metadata the installed binary must report.
	Metadata Metadata
	// MailboxRoot is the directory of .eml fixtures the test account
	// reads. It may be empty, in which case the account has an empty
	// mailbox.
	MailboxRoot string
	// Timeout bounds the whole test. Zero selects a default.
	Timeout time.Duration
}

// smokeDefaultTimeout bounds the smoke test.
//
// The server has to start, answer a handshake and shut down. A generous
// bound keeps a slow machine from failing the test, while still failing
// rather than hanging forever.
const smokeDefaultTimeout = 60 * time.Second

// requiredTools are the tools the smoke test expects the server to
// advertise.
//
// The list is the public tool surface. A release that dropped a tool or
// renamed one would be a breaking change, and the smoke test is where
// that is noticed.
var requiredTools = []string{
	"list_accounts",
	"list_mailboxes",
	"list_messages",
	"read_message",
	"read_attachment",
	"search_messages",
	"send_message",
}

// SmokeInstall installs the artifact into a clean temporary directory and
// exercises it.
//
// A clean install means a fresh directory that contains nothing but the
// installed binary and a configuration file the test writes itself. The
// binary is installed by copying it and setting the executable bit, which
// is what an installer does, and it is then run from that directory, so
// the test cannot pass because of something that was already installed
// on the machine.
//
// The test performs three things:
//
//  1. "themailroom -version" reports the expected version, commit and
//     build date;
//  2. the server starts, completes an MCP initialize handshake and
//     advertises the expected tools;
//  3. the server exits cleanly when its input is closed.
func SmokeInstall(opts SmokeOptions) error {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = smokeDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "themailroom-smoke-")
	if err != nil {
		return fmt.Errorf("release: create install directory: %w", err)
	}
	defer os.RemoveAll(dir)

	installPath, err := installBinary(dir, opts.Binary)
	if err != nil {
		return err
	}

	configPath, err := writeSmokeConfig(dir, opts.MailboxRoot)
	if err != nil {
		return err
	}

	if err := checkVersionOutput(installPath, opts.Metadata); err != nil {
		return err
	}
	if err := checkMCPHandshake(ctx, installPath, configPath); err != nil {
		return err
	}
	return nil
}

// installBinary copies the artifact into the install directory under the
// name the server is installed as, and makes it executable.
func installBinary(dir, binary string) (string, error) {
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("release: create %s: %w", binDir, err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		return "", fmt.Errorf("release: read artifact: %w", err)
	}
	installPath := filepath.Join(binDir, "themailroom")
	if err := os.WriteFile(installPath, data, 0o755); err != nil {
		return "", fmt.Errorf("release: install %s: %w", installPath, err)
	}
	return installPath, nil
}

// writeSmokeConfig writes a minimal configuration for the installed
// server.
//
// The account uses the in-memory backend, so the smoke test needs no
// network, no credentials and no mail server.
//
// The mailbox root is made absolute: the server is started with its
// working directory set to the install directory, so a relative path
// would resolve against that directory rather than against the source
// tree.
func writeSmokeConfig(dir, mailboxRoot string) (string, error) {
	cfgDir := filepath.Join(dir, "etc")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return "", fmt.Errorf("release: create %s: %w", cfgDir, err)
	}
	var b strings.Builder
	b.WriteString("server:\n  log_level: error\n\naccounts:\n")
	b.WriteString("  - id: smoke\n")
	b.WriteString("    backend: memory\n")
	b.WriteString("    email: smoke@example.test\n")
	if mailboxRoot != "" {
		abs, err := filepath.Abs(mailboxRoot)
		if err != nil {
			return "", fmt.Errorf("release: resolve mailbox root: %w", err)
		}
		fmt.Fprintf(&b, "    mailbox_root: %s\n", abs)
	}
	b.WriteString("    permissions: [read]\n")

	path := filepath.Join(cfgDir, "themailroom.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("release: write configuration: %w", err)
	}
	return path, nil
}

// checkVersionOutput runs the installed binary with -version and checks
// that it reports the expected metadata.
//
// The comparison is exact. A substring comparison would be wrong: the
// development default is "0.1.0-dev", which contains "0.1.0", so a
// substring check would accept a binary that still reports the
// development default.
func checkVersionOutput(installPath string, want Metadata) error {
	cmd := exec.Command(installPath, "-version")
	cmd.Dir = filepath.Dir(installPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("release: %s -version failed: %w\n%s", installPath, err, out)
	}
	gotVersion, gotCommit, gotDate, err := parseVersionOutput(string(out))
	if err != nil {
		return fmt.Errorf("release: %s: %w", installPath, err)
	}
	if gotVersion != want.Version {
		return fmt.Errorf("release: %s -version reports version %q, want %q", installPath, gotVersion, want.Version)
	}
	if gotCommit != want.Commit {
		return fmt.Errorf("release: %s -version reports commit %q, want %q", installPath, gotCommit, want.Commit)
	}
	if gotDate != want.BuildDate {
		return fmt.Errorf("release: %s -version reports build date %q, want %q", installPath, gotDate, want.BuildDate)
	}
	return nil
}

// checkMCPHandshake starts the installed server and speaks MCP to it.
//
// The client is the official SDK, so the handshake is the real protocol
// exchange rather than a hand-rolled approximation of it.
//
// The server runs with its working directory set to the install
// directory, so the test cannot pass because of a configuration file or
// a fixture that happens to sit in the caller's working directory.
func checkMCPHandshake(ctx context.Context, installPath, configPath string) error {
	cmd := exec.Command(installPath, "-config", configPath)
	cmd.Dir = filepath.Dir(installPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{Name: "themailroom-smoke", Version: "1"}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("release: MCP handshake failed: %w\n%s", err, stderr.String())
	}

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		return fmt.Errorf("release: tools/list failed: %w\n%s", err, stderr.String())
	}
	advertised := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		advertised[tool.Name] = true
	}
	for _, name := range requiredTools {
		if !advertised[name] {
			_ = session.Close()
			return fmt.Errorf("release: the server does not advertise %q", name)
		}
	}

	// Closing the session closes the server's input. The server must then
	// exit cleanly; an unclean exit is a release defect.
	if err := session.Close(); err != nil {
		return fmt.Errorf("release: the server did not exit cleanly: %w\n%s", err, stderr.String())
	}
	return nil
}
