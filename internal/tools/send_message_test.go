package tools

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mimeparse"
	"github.com/flowbyte-com/themailroom/internal/smtpclient"
)

func requireToolErrorContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected tool error containing %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want substring %q", err, want)
	}
}

type recordingSender struct {
	mu     sync.Mutex
	calls  int
	req    smtpclient.Request
	result smtpclient.Result
	err    error
}

func (s *recordingSender) Send(_ context.Context, req smtpclient.Request) (smtpclient.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.req = req
	if s.err != nil {
		return smtpclient.Result{}, s.err
	}
	result := s.result
	if result.MessageID == "" {
		result.MessageID = req.MessageID
	}
	return result, nil
}

func (s *recordingSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *recordingSender) request() smtpclient.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.req
}

type sendEnv struct {
	session *mcp.ClientSession
	reg     *accounts.Registry
	cleanup func()
}

func newSendEnv(t *testing.T, yaml string, sender smtpclient.Sender, logger *slog.Logger) *sendEnv {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	reg, err := accounts.New(cfg)
	if err != nil {
		t.Fatalf("accounts.New: %v", err)
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "send-test"}, nil)
	Register(srv, reg, WithSender(sender), WithLogger(logger))
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client"}, nil)
	session, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	return &sendEnv{
		session: session,
		reg:     reg,
		cleanup: func() {
			session.Close()
			_ = reg.Close()
		},
	}
}

func (e *sendEnv) call(t *testing.T, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	return (&testEnv{client: e.session}).call(t, name, args)
}

func baseSendYAML(t *testing.T, smtpPort int) string {
	t.Helper()
	return sendYAMLWithAccounts(t, smtpPort, "")
}

func sendYAMLWithAccounts(t *testing.T, smtpPort int, extra string) string {
	t.Helper()
	return sendYAMLWithPermissions(t, smtpPort, "[send]", extra)
}

func sendYAMLWithPermissions(t *testing.T, smtpPort int, perms string, extra string) string {
	t.Helper()
	mbx := t.TempDir()
	return fmt.Sprintf(`
server:
  log_level: error
accounts:
  - id: sender
    backend: memory
    email: Sales@Example.TEST
    mailbox_root: %q
    permissions: %s
    smtp:
      host: 127.0.0.1
      port: %d
      starttls: true
      username: user@example.test
      password: s3cret
%s`, mbx, perms, smtpPort, extra)
}

func TestSendMessageRequiresSendPermission(t *testing.T) {
	mbx := t.TempDir()
	yaml := fmt.Sprintf(`
server:
  log_level: error
accounts:
  - id: reader
    backend: memory
    email: reader@example.test
    mailbox_root: %q
    permissions: [read]
`, mbx)
	sender := &recordingSender{}
	env := newSendEnv(t, yaml, sender, nil)
	defer env.cleanup()

	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "reader",
		"to":         []any{"alice@example.test"},
		"subject":    "test",
		"text":       "body",
	})
	if err == nil {
		t.Fatal("expected permission error")
	}
	requireToolErrorContains(t, err, "lacks capability")
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestSendMessageRequiresAttachmentSendPermission(t *testing.T) {
	sender := &recordingSender{}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()

	_, err := env.call(t, "send_message", map[string]any{
		"account_id":  "sender",
		"to":          []any{"alice@example.test"},
		"subject":     "test",
		"text":        "body",
		"attachments": []any{"note.txt"},
	})
	if err == nil {
		t.Fatal("expected permission error")
	}
	requireToolErrorContains(t, err, "lacks capability")
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestSendMessageRecipientPolicy(t *testing.T) {
	extra := `
    allowed_recipients: [alice@example.test, bob@example.test]
    limits:
      max_recipients: 1
`
	sender := &recordingSender{}
	env := newSendEnv(t, sendYAMLWithAccounts(t, 2525, extra), sender, nil)
	defer env.cleanup()

	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"to":         []any{"eve@example.test"},
		"subject":    "test",
		"text":       "body",
	})
	if err == nil {
		t.Fatal("expected recipient policy error")
	}
	requireToolErrorContains(t, err, "recipient is not allowed")

	_, err = env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"to":         []any{"alice@example.test", "bob@example.test"},
		"subject":    "test",
		"text":       "body",
	})
	if err == nil {
		t.Fatal("expected recipient limit error")
	}
	requireToolErrorContains(t, err, "too many unique recipients")
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestSendMessageNoRecipients(t *testing.T) {
	sender := &recordingSender{}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()

	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"subject":    "test",
	})
	if err == nil {
		t.Fatal("expected recipient error")
	}
	requireToolErrorContains(t, err, "invalid recipient list")
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestSendMessagePreflightAttachmentErrors(t *testing.T) {
	root := t.TempDir()
	secondRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "note.txt"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := fmt.Sprintf(`
    attachment_send_paths:
      - %q
      - %q
    limits:
      max_attachment_bytes: 3
      max_total_attachment_bytes: 100
      max_attachments: 1
      max_message_bytes: 200
`, root, secondRoot)
	sender := &recordingSender{}
	env := newSendEnv(t, sendYAMLWithPermissions(t, 2525, "[send, attachment_send]", extra), sender, nil)
	defer env.cleanup()

	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{
			name: "missing",
			args: map[string]any{"account_id": "sender", "to": []any{"alice@example.test"}, "attachments": []any{"missing.txt"}},
			code: codeAttachmentNotFound,
		},
		{
			name: "ambiguous",
			args: map[string]any{"account_id": "sender", "to": []any{"alice@example.test"}, "attachments": []any{"note.txt"}},
			code: codeInvalidInput,
		},
		{
			name: "too large",
			args: map[string]any{"account_id": "sender", "to": []any{"alice@example.test"}, "attachments": []any{"big.txt"}},
			code: codeAttachmentTooLarge,
		},
		{
			name: "too many",
			args: map[string]any{"account_id": "sender", "to": []any{"alice@example.test"}, "attachments": []any{"big.txt", "note.txt"}},
			code: codeTooManyAttachments,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.call(t, "send_message", tc.args)
			if err == nil {
				t.Fatal("expected preflight error")
			}
			want := map[string]string{
				codeAttachmentNotFound: "attachment not found",
				codeInvalidInput:       "invalid attachment reference",
				codeAttachmentTooLarge: "attachment exceeds size limit",
				codeTooManyAttachments: "too many attachments",
			}[tc.code]
			requireToolErrorContains(t, err, want)
		})
	}
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0 for preflight failures", sender.callCount())
	}
}

func TestSendMessageTooLargeBeforeSMTP(t *testing.T) {
	sender := &recordingSender{}
	env := newSendEnv(t, sendYAMLWithAccounts(t, 2525, `
    limits:
      max_message_bytes: 128
`), sender, nil)
	defer env.cleanup()

	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"to":         []any{"alice@example.test"},
		"subject":    strings.Repeat("s", 200),
		"text":       "body",
	})
	if err == nil {
		t.Fatal("expected message size error")
	}
	requireToolErrorContains(t, err, "serialized message exceeds size limit")
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestSendMessageSuccessContract(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("attachment body"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := fmt.Sprintf(`
    attachment_send_paths:
      - %q
`, root)
	sender := &recordingSender{}
	env := newSendEnv(t, sendYAMLWithPermissions(t, 2525, "[send, attachment_send]", extra), sender, nil)
	defer env.cleanup()

	got, err := env.call(t, "send_message", map[string]any{
		"account_id":  "sender",
		"to":          []any{"Alice@Example.TEST"},
		"cc":          []any{"carol@example.test"},
		"bcc":         []any{"bob@example.test"},
		"subject":     "Quarterly report",
		"text":        "Plain body",
		"html":        "<p>HTML body</p>",
		"attachments": []any{"note.txt"},
	})
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if got["status"] != "submitted" {
		t.Errorf("status = %v, want submitted", got["status"])
	}
	if got["account_id"] != "sender" {
		t.Errorf("account_id = %v", got["account_id"])
	}
	messageID, _ := got["message_id"].(string)
	if messageID == "" {
		t.Fatal("message_id missing")
	}
	if got["recipient_count"] != float64(3) {
		t.Errorf("recipient_count = %v, want 3", got["recipient_count"])
	}
	if got["attachment_count"] != float64(1) {
		t.Errorf("attachment_count = %v, want 1", got["attachment_count"])
	}
	if got["total_attachment_bytes"] != float64(len("attachment body")) {
		t.Errorf("total_attachment_bytes = %v", got["total_attachment_bytes"])
	}
	if sender.callCount() != 1 {
		t.Fatalf("sender called %d times, want 1", sender.callCount())
	}
	req := sender.request()
	if req.From != "Sales@example.test" {
		t.Errorf("From = %q, want canonical account identity", req.From)
	}
	wantRecipients := []string{"Alice@example.test", "carol@example.test", "bob@example.test"}
	if len(req.Recipients) != len(wantRecipients) {
		t.Fatalf("recipients = %v, want %v", req.Recipients, wantRecipients)
	}
	for i := range wantRecipients {
		if req.Recipients[i] != wantRecipients[i] {
			t.Errorf("recipients[%d] = %q, want %q", i, req.Recipients[i], wantRecipients[i])
		}
	}
	if req.MessageID != messageID {
		t.Errorf("sender MessageID = %q, tool MessageID = %q", req.MessageID, messageID)
	}

	decoded, err := mimeparse.Parse(req.Message, mimeparse.Limits{
		MaxMessageBytes:         1 << 20,
		MaxAttachmentBytes:      1 << 20,
		MaxTotalAttachmentBytes: 2 << 20,
		MaxAttachments:          10,
	})
	if err != nil {
		t.Fatalf("mimeparse.Parse: %v", err)
	}
	if decoded.Text != "Plain body" {
		t.Errorf("Text = %q", decoded.Text)
	}
	if decoded.HTML != "<p>HTML body</p>" {
		t.Errorf("HTML = %q", decoded.HTML)
	}
	if len(decoded.Attachments) != 1 || decoded.Attachments[0].Filename != "note.txt" {
		t.Fatalf("attachments = %+v", decoded.Attachments)
	}
	content, err := mimeparse.ExtractAttachment(req.Message, decoded.Attachments[0].Part, mimeparse.Limits{
		MaxMessageBytes:         1 << 20,
		MaxAttachmentBytes:      1 << 20,
		MaxTotalAttachmentBytes: 2 << 20,
		MaxAttachments:          10,
	})
	if err != nil {
		t.Fatalf("ExtractAttachment: %v", err)
	}
	if string(content.Content) != "attachment body" {
		t.Errorf("attachment content = %q", content.Content)
	}
	if decoded.Headers.MessageID != messageID {
		t.Errorf("Message-ID header = %q, want %s", decoded.Headers.MessageID, messageID)
	}
	rawLower := strings.ToLower(string(req.Message))
	if strings.Contains(rawLower, "\r\nbcc:") || strings.HasPrefix(rawLower, "bcc:") {
		t.Error("Bcc header was serialized")
	}
}

func TestSendMessageUnknownAccountDoesNotDial(t *testing.T) {
	sender := &recordingSender{}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()
	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "missing",
		"to":         []any{"alice@example.test"},
	})
	requireToolErrorContains(t, err, "account not found")
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestSendMessageAbsoluteAttachmentRejected(t *testing.T) {
	root := t.TempDir()
	extra := fmt.Sprintf(`
    attachment_send_paths:
      - %q
`, root)
	sender := &recordingSender{}
	env := newSendEnv(t, sendYAMLWithPermissions(t, 2525, "[send, attachment_send]", extra), sender, nil)
	defer env.cleanup()
	_, err := env.call(t, "send_message", map[string]any{
		"account_id":  "sender",
		"to":          []any{"alice@example.test"},
		"attachments": []any{"/etc/passwd"},
	})
	requireToolErrorContains(t, err, "invalid attachment reference")
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestSendMessageSMTPUTF8RejectedBeforeSender(t *testing.T) {
	sender := &recordingSender{}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()
	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"to":         []any{"ü@example.test"},
	})
	requireToolErrorContains(t, err, "SMTPUTF8")
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestMapErrorSMTPClasses(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{smtpclient.ErrTLSFailed, codeSmtpTLSFailed},
		{smtpclient.ErrAuthFailed, codeSmtpAuthFailed},
		{smtpclient.ErrSenderRejected, codeSenderRejected},
		{smtpclient.ErrRecipientRejected, codeRecipientRejected},
		{smtpclient.ErrMessageRejected, codeSmtpMessageRejected},
		{smtpclient.ErrTimeout, codeSmtpTimeout},
		{smtpclient.ErrConnectionFailed, codeSmtpConnectionFailed},
		{smtpclient.ErrInvalidRequest, codeInvalidInput},
	}
	for _, tc := range cases {
		if got := mapError(tc.err); got.Code != tc.code {
			t.Errorf("mapError(%v).Code = %q, want %q", tc.err, got.Code, tc.code)
		}
	}
}

func TestSendMessageRejectsCallerControlledTransportFields(t *testing.T) {
	sender := &recordingSender{}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()
	for _, field := range []string{"from", "headers", "raw_mime", "password", "smtp_command"} {
		t.Run(field, func(t *testing.T) {
			_, err := env.call(t, "send_message", map[string]any{
				"account_id": "sender",
				"to":         []any{"alice@example.test"},
				field:        "attacker-controlled",
			})
			if err == nil {
				t.Fatalf("expected caller-supplied %s to be rejected", field)
			}
		})
	}
	if sender.callCount() != 0 {
		t.Errorf("sender called %d times, want 0", sender.callCount())
	}
}

func TestSendMessageSenderFailureIsReported(t *testing.T) {
	sender := &recordingSender{err: smtpclient.ErrRecipientRejected}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()
	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"to":         []any{"alice@example.test"},
		"subject":    "test",
		"text":       "body",
	})
	requireToolErrorContains(t, err, "rejected a recipient")
	if sender.callCount() != 1 {
		t.Errorf("sender called %d times, want 1", sender.callCount())
	}
}

func TestMapErrorAmbiguousPreservesMessageID(t *testing.T) {
	te := mapError(&smtpclient.AmbiguousError{MessageID: "original@example.test"})
	if te.Code != codeSmtpAmbiguousFailure {
		t.Fatalf("code = %q", te.Code)
	}
	if te.Details["message_id"] != "original@example.test" {
		t.Errorf("details message_id = %v", te.Details["message_id"])
	}
	if te.Details["may_have_been_sent"] != true {
		t.Errorf("details may_have_been_sent = %v", te.Details["may_have_been_sent"])
	}
	if !strings.Contains(te.Message, "original@example.test") || !strings.Contains(te.Message, "may_have_been_sent=true") {
		t.Errorf("message does not carry the ambiguous identity: %q", te.Message)
	}
}

func TestSendMessageDeduplicatesRecipientsAndKeepsBccEnvelopeOnly(t *testing.T) {
	sender := &recordingSender{}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()

	got, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"to":         []any{"alice@example.test"},
		"cc":         []any{"alice@example.test"},
		"bcc":        []any{"bob@example.test", "alice@example.test"},
		"text":       "body",
	})
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if got["recipient_count"] != float64(2) {
		t.Errorf("recipient_count = %v, want 2", got["recipient_count"])
	}
	req := sender.request()
	if len(req.Recipients) != 2 || req.Recipients[0] != "alice@example.test" || req.Recipients[1] != "bob@example.test" {
		t.Errorf("recipients = %v, want unique alice then bob", req.Recipients)
	}
	rawLower := strings.ToLower(string(req.Message))
	if strings.Contains(rawLower, "\r\nbcc:") || strings.HasPrefix(rawLower, "bcc:") {
		t.Error("Bcc header was serialized")
	}
}

func TestSendMessageAmbiguousFailureCarriesMessageID(t *testing.T) {
	sender := &recordingSender{err: &smtpclient.AmbiguousError{MessageID: "ambiguous@example.test"}}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()
	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"to":         []any{"alice@example.test"},
		"subject":    "test",
		"text":       "body",
	})
	requireToolErrorContains(t, err, "SmtpAmbiguousFailure")
	requireToolErrorContains(t, err, "ambiguous@example.test")
	requireToolErrorContains(t, err, "may_have_been_sent=true")
	if sender.callCount() != 1 {
		t.Errorf("sender called %d times, want exactly one attempt", sender.callCount())
	}
}

func TestSendMessageBccOnly(t *testing.T) {
	sender := &recordingSender{}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, nil)
	defer env.cleanup()

	got, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"bcc":        []any{"bob@example.test"},
		"text":       "body",
	})
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if got["recipient_count"] != float64(1) {
		t.Errorf("recipient_count = %v, want 1", got["recipient_count"])
	}
	req := sender.request()
	if len(req.Recipients) != 1 || req.Recipients[0] != "bob@example.test" {
		t.Errorf("recipients = %v, want bob only", req.Recipients)
	}
	rawLower := strings.ToLower(string(req.Message))
	if strings.Contains(rawLower, "\r\nbcc:") || strings.HasPrefix(rawLower, "bcc:") {
		t.Error("Bcc header was serialized")
	}
}

func TestSendMessageAuditLogIsRedacted(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	sender := &recordingSender{}
	env := newSendEnv(t, baseSendYAML(t, 2525), sender, logger)
	defer env.cleanup()

	_, err := env.call(t, "send_message", map[string]any{
		"account_id": "sender",
		"to":         []any{"alice@example.test"},
		"subject":    "private subject",
		"text":       "private body",
	})
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	line := logs.String()
	for _, want := range []string{"send_message", "account_id=sender", "outcome=submitted", "recipient_count=1", "message_id="} {
		if !strings.Contains(line, want) {
			t.Errorf("audit log missing %q: %s", want, line)
		}
	}
	for _, secret := range []string{"private subject", "private body", "s3cret"} {
		if strings.Contains(line, secret) {
			t.Errorf("audit log leaked %q: %s", secret, line)
		}
	}
}

type captureSMTPBackend struct {
	mu       sync.Mutex
	sessions []*captureSMTPSession
}

func (b *captureSMTPBackend) NewSession(_ *gosmtp.Conn) (gosmtp.Session, error) {
	s := &captureSMTPSession{}
	b.mu.Lock()
	b.sessions = append(b.sessions, s)
	b.mu.Unlock()
	return s, nil
}

func (b *captureSMTPBackend) snapshot() []*captureSMTPSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*captureSMTPSession(nil), b.sessions...)
}

type captureSMTPSession struct {
	mu   sync.Mutex
	from string
	rcpt []string
	data []byte
}

func (s *captureSMTPSession) Mail(from string, _ *gosmtp.MailOptions) error {
	s.mu.Lock()
	s.from = from
	s.mu.Unlock()
	return nil
}

func (s *captureSMTPSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.mu.Lock()
	s.rcpt = append(s.rcpt, to)
	s.mu.Unlock()
	return nil
}

func (s *captureSMTPSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.data = data
	s.mu.Unlock()
	return nil
}

func (s *captureSMTPSession) Reset()        {}
func (s *captureSMTPSession) Logout() error { return nil }

func (s *captureSMTPSession) AuthMechanisms() []string { return []string{sasl.Plain} }

func (s *captureSMTPSession) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, gosmtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(_, username, password string) error {
		if username == "user@example.test" && password == "s3cret" {
			return nil
		}
		return gosmtp.ErrAuthFailed
	}), nil
}

func (s *captureSMTPSession) snapshot() (string, []string, []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.from, append([]string(nil), s.rcpt...), append([]byte(nil), s.data...)
}

func smtpSenderWithCert(t *testing.T, cert tls.Certificate) *smtpclient.Client {
	t.Helper()
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	sender := smtpclient.New()
	sender.RootCAs = pool
	return sender
}

func TestSendMessageEndToEndSMTPReal(t *testing.T) {
	cert := generateCert(t)
	backend := &captureSMTPBackend{}
	srv := gosmtp.NewServer(backend)
	srv.Domain = "test"
	srv.MaxMessageBytes = 4 << 20
	srv.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	srv.ErrorLog = log.New(io.Discard, "", 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, portString, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	_, _ = fmt.Sscanf(portString, "%d", &port)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("attachment body"), 0o600); err != nil {
		t.Fatal(err)
	}
	extra := fmt.Sprintf(`
    attachment_send_paths:
      - %q
`, root)
	env := newSendEnv(t, sendYAMLWithPermissions(t, port, "[send, attachment_send]", extra), smtpSenderWithCert(t, cert), nil)
	defer env.cleanup()

	got, err := env.call(t, "send_message", map[string]any{
		"account_id":  "sender",
		"to":          []any{"Alice@Example.TEST"},
		"cc":          []any{"carol@example.test"},
		"bcc":         []any{"bob@example.test"},
		"subject":     "End to end",
		"text":        "Hello via SMTP",
		"html":        "<p>Hello via SMTP</p>",
		"attachments": []any{"note.txt"},
	})
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if got["status"] != "submitted" {
		t.Errorf("status = %v", got["status"])
	}
	sessions := backend.snapshot()
	var submitted []*captureSMTPSession
	for _, session := range sessions {
		from, _, _ := session.snapshot()
		if from != "" {
			submitted = append(submitted, session)
		}
	}
	if len(submitted) != 1 {
		t.Fatalf("submitted sessions = %d, want 1 (total sessions %d)", len(submitted), len(sessions))
	}
	from, rcpts, data := submitted[0].snapshot()
	if from != "Sales@example.test" {
		t.Errorf("MAIL FROM = %q", from)
	}
	wantRcpts := []string{"Alice@example.test", "carol@example.test", "bob@example.test"}
	if len(rcpts) != len(wantRcpts) {
		t.Fatalf("RCPT TO = %v, want %v", rcpts, wantRcpts)
	}
	for i := range wantRcpts {
		if rcpts[i] != wantRcpts[i] {
			t.Errorf("RCPT[%d] = %q, want %q", i, rcpts[i], wantRcpts[i])
		}
	}
	decoded, err := mimeparse.Parse(data, mimeparse.Limits{
		MaxMessageBytes:         4 << 20,
		MaxAttachmentBytes:      1 << 20,
		MaxTotalAttachmentBytes: 2 << 20,
		MaxAttachments:          10,
	})
	if err != nil {
		t.Fatalf("mimeparse.Parse: %v", err)
	}
	if decoded.Text != "Hello via SMTP" {
		t.Errorf("Text = %q", decoded.Text)
	}
	if decoded.HTML != "<p>Hello via SMTP</p>" {
		t.Errorf("HTML = %q", decoded.HTML)
	}
	if len(decoded.Attachments) != 1 || decoded.Attachments[0].Filename != "note.txt" {
		t.Fatalf("attachments = %+v", decoded.Attachments)
	}
	messageID, _ := got["message_id"].(string)
	if decoded.Headers.MessageID != messageID {
		t.Errorf("Message-ID header = %q, want %s", decoded.Headers.MessageID, messageID)
	}
	rawLower := strings.ToLower(string(data))
	if strings.Contains(rawLower, "\nbcc:") || strings.HasPrefix(rawLower, "bcc:") {
		t.Error("Bcc header was serialized")
	}
}
