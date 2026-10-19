package tools

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	memserver "github.com/emersion/go-imap/backend/memory"
	imapsrv "github.com/emersion/go-imap/server"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/imapclient"
)

// imapE2EEnv wires a full MCP server with an IMAP account backed by
// the in-process go-imap server.
type imapE2EEnv struct {
	session *mcp.ClientSession
	server  *mcp.Server
	reg     *accounts.Registry
	cleanup func()
}

func setupIMAPEnv(t *testing.T) *imapE2EEnv {
	t.Helper()
	addr, srvCleanup := startIMAPServer(t)

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var p int
	_, _ = fmt.Sscanf(port, "%d", &p)

	// Memory messages directory is required by validateAccount; we
	// give it a real (but unused) tmp dir for the memory backend
	// fallback. In practice, this account will use the IMAP
	// backend; the memory backend fields are ignored.
	mbx := t.TempDir()

	yaml := `
server:
  log_level: error
accounts:
  - id: imap
    backend: imap
    email: user@example.test
    imap:
      host: __HOST__
      port: __PORT__
      tls: true
      username: username
      password: password
    permissions: [read]
    mailbox_root: __MBX__
    limits:
      max_message_bytes: 26214400
      max_attachments: 50
      max_search_results: 500
      max_batch_operations: 50
`
	yaml = strings.ReplaceAll(yaml, "__HOST__", host)
	yaml = strings.ReplaceAll(yaml, "__PORT__", fmt.Sprintf("%d", p))
	yaml = strings.ReplaceAll(yaml, "__MBX__", mbx)

	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Default New(cfg) does not let us pass the test-only TLS
	// config. The imapclient package exposes Options for tests;
	// we need to plumb that through. For now, the cleanest path is
	// to bypass config-driven construction and call imapclient.New
	// directly, then attach the resulting backend to the account
	// we just registered.
	reg, err := accounts.New(cfg)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	// The imap account's backend is wired by accounts.New; check it
	// is a real IMAP backend.
	acc, err := reg.Get("imap")
	if err != nil {
		t.Fatalf("get imap: %v", err)
	}
	if acc.Backend() == nil {
		t.Fatal("imap backend is nil")
	}

	// Override the backend with one that has InsecureSkipVerify set,
	// so we can talk to the self-signed test server.
	cert := generateCert(t)
	be, err := imapclient.New(cfg.Accounts[0], imapclient.Options{
		DialTimeout: 5 * time.Second,
		TLSConfig:   &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{cert}},
	})
	if err != nil {
		t.Fatalf("imapclient.New: %v", err)
	}
	acc.SetBackendForTest(be)

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
	return &imapE2EEnv{
		session: session,
		server:  srv,
		reg:     reg,
		cleanup: func() {
			session.Close()
			srvCleanup()
		},
	}
}

func (e *imapE2EEnv) call(t *testing.T, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	res, err := e.session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		var text string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		return nil, &toolErr{msg: text}
	}
	if res.StructuredContent != nil {
		return res.StructuredContent.(map[string]any), nil
	}
	return nil, nil
}

func startIMAPServer(t *testing.T) (string, func()) {
	t.Helper()
	be := memserver.New()
	srv := imapsrv.New(be)
	srv.AllowInsecureAuth = false

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{generateCert(t)},
		MinVersion:   tls.VersionTLS12,
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	return ln.Addr().String(), func() { _ = srv.Close() }
}

func generateCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(2 * time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestIMAPEndToEnd(t *testing.T) {
	env := setupIMAPEnv(t)
	defer env.cleanup()

	// list_mailboxes
	got, err := env.call(t, "list_mailboxes", map[string]any{"account_id": "imap"})
	if err != nil {
		t.Fatalf("list_mailboxes: %v", err)
	}
	mbs := got["mailboxes"].([]any)
	if len(mbs) == 0 {
		t.Fatal("no mailboxes")
	}

	// list_messages
	got, err = env.call(t, "list_messages", map[string]any{
		"account_id": "imap",
		"mailbox":    "INBOX",
		"limit":      5,
	})
	if err != nil {
		t.Fatalf("list_messages: %v", err)
	}
	msgs := got["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	first := msgs[0].(map[string]any)
	uid, _ := first["uid"].(float64)
	if uid == 0 {
		t.Error("UID missing")
	}

	// read_message
	uv, _ := got["uid_validity"].(float64)
	if uv == 0 {
		t.Fatal("list_messages did not surface a live UIDVALIDITY")
	}
	got, err = env.call(t, "read_message", map[string]any{
		"account_id":   "imap",
		"mailbox":      "INBOX",
		"uid":          uid,
		"uid_validity": uv,
	})
	if err != nil {
		t.Fatalf("read_message: %v", err)
	}
	if got["text"] == nil {
		t.Error("text body missing")
	}

	// search_messages
	got, err = env.call(t, "search_messages", map[string]any{
		"account_id": "imap",
		"mailbox":    "INBOX",
		"query":      map[string]any{"subject": "little message"},
	})
	if err != nil {
		t.Fatalf("search_messages: %v", err)
	}
	matches := got["matches"].([]any)
	if len(matches) == 0 {
		t.Errorf("search returned 0 matches")
	}
}

func TestIMAPEndToEndMissingMailbox(t *testing.T) {
	env := setupIMAPEnv(t)
	defer env.cleanup()

	_, err := env.call(t, "list_messages", map[string]any{
		"account_id": "imap",
		"mailbox":    "NoSuchMailbox",
		"limit":      5,
	})
	if err == nil {
		t.Fatal("expected error for missing mailbox")
	}
}

// findRepoRootIMAP is unused; tools_test.go in the same package
// already exposes findRepoRoot. Kept as a no-op for future use.
var _ = func() {}
