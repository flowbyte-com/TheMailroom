package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/config"
)

func repoRoot(t *testing.T) string {
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

func TestNewAndServe(t *testing.T) {
	messages := filepath.Join(repoRoot(t), "testdata", "messages")
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
  - id: empty
    backend: memory
    email: empty@example.test
    mailbox_root: __EMPTY__
    permissions: []
`
	yaml = strings.ReplaceAll(yaml, "__MBX__", messages)
	yaml = strings.ReplaceAll(yaml, "__EMPTY__", empty)

	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	reg, err := accounts.New(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	srv, err := New(reg, Options{Implementation: "themailroom-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if srv.Registry() != reg {
		t.Error("Registry() returned a different registry")
	}
	if srv.MCPServer() == nil {
		t.Error("MCPServer() returned nil")
	}
}

// TestInMemoryServe spins the server over an in-memory transport and
// confirms a tools/list round-trip works.
func TestInMemoryServe(t *testing.T) {
	messages := filepath.Join(repoRoot(t), "testdata", "messages")
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
`
	yaml = strings.ReplaceAll(yaml, "__MBX__", messages)
	yaml = strings.ReplaceAll(yaml, "__EMPTY__", empty)

	cfg, _ := config.Parse([]byte(yaml))
	reg, err := accounts.New(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	srv, err := New(reg, Options{Implementation: "themailroom-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.MCPServer().Connect(context.Background(), serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil)
	session, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 6 {
		t.Errorf("len(Tools) = %d, want 6", len(res.Tools))
	}
	foundAttachment := false
	for _, tool := range res.Tools {
		if tool.Name == "read_attachment" {
			foundAttachment = true
			break
		}
	}
	if !foundAttachment {
		t.Error("read_attachment tool is not registered")
	}
}
