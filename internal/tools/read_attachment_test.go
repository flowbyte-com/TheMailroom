package tools

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/flowbyte-com/themailroom/internal/mailbox"
)

func findTestMessage(t *testing.T, env *testEnv, subject string) (uint32, uint32) {
	t.Helper()
	got, err := env.call(t, "list_messages", map[string]any{
		"account_id": "full",
		"mailbox":    "INBOX",
		"limit":      float64(50),
	})
	if err != nil {
		t.Fatalf("list_messages: %v", err)
	}
	uv := uint32(got["uid_validity"].(float64))
	for _, item := range got["messages"].([]any) {
		m := item.(map[string]any)
		if m["subject"] == subject {
			return uint32(m["uid"].(float64)), uv
		}
	}
	t.Fatalf("message %q not found", subject)
	return 0, 0
}

func attachmentPart(t *testing.T, env *testEnv, uid, uv uint32, filename string) string {
	t.Helper()
	msg, err := env.call(t, "read_message", map[string]any{
		"account_id":   "full",
		"mailbox":      "INBOX",
		"uid":          float64(uid),
		"uid_validity": float64(uv),
	})
	if err != nil {
		t.Fatalf("read_message: %v", err)
	}
	for _, item := range msg["attachments"].([]any) {
		a := item.(map[string]any)
		if a["filename"] == filename {
			return a["part"].(string)
		}
	}
	t.Fatalf("attachment %q not found", filename)
	return ""
}

func TestReadAttachment(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()
	uid, uv := findTestMessage(t, env, "Multi-part: alternative + related + mixed")

	// Nested inline part: the tool must return the exact bytes and the
	// same part reference that read_message advertised.
	part := attachmentPart(t, env, uid, uv, "logo.png")
	got, err := env.call(t, "read_attachment", map[string]any{
		"account_id":   "full",
		"mailbox":      "INBOX",
		"uid":          float64(uid),
		"uid_validity": float64(uv),
		"part":         part,
	})
	if err != nil {
		t.Fatalf("read_attachment: %v", err)
	}
	if got["account_id"] != "full" || got["mailbox"] != "INBOX" ||
		uint32(got["uid"].(float64)) != uid || uint32(got["uid_validity"].(float64)) != uv ||
		got["part"] != part || got["filename"] != "logo.png" || got["mime_type"] != "image/png" {
		t.Errorf("metadata mismatch: %v", got)
	}
	encoded, ok := got["content_base64"].(string)
	if !ok {
		t.Fatalf("content_base64 missing or wrong type: %T", got["content_base64"])
	}
	content, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if len(content) != 99 || !strings.HasPrefix(string(content), "\x89PNG\r\n\x1a\n") {
		t.Errorf("logo content len=%d prefix=%q", len(content), content[:8])
	}
	if int64(got["size"].(float64)) != int64(len(content)) {
		t.Errorf("size = %v, decoded len = %d", got["size"], len(content))
	}
	if _, present := got["truncated"]; present {
		t.Error("read_attachment must not return a truncated field")
	}
}

func TestReadAttachmentPDFBase64RoundTrip(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()
	uid, uv := findTestMessage(t, env, "Multi-part: alternative + related + mixed")
	part := attachmentPart(t, env, uid, uv, "report.pdf")
	got, err := env.call(t, "read_attachment", map[string]any{
		"account_id":   "full",
		"mailbox":      "INBOX",
		"uid":          float64(uid),
		"uid_validity": float64(uv),
		"part":         part,
	})
	if err != nil {
		t.Fatalf("read_attachment: %v", err)
	}
	encoded := got["content_base64"].(string)
	content, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if !strings.HasPrefix(string(content), "%PDF-1.4") {
		t.Errorf("PDF prefix = %q", content[:8])
	}
	if int64(len(content)) != int64(got["size"].(float64)) {
		t.Errorf("decoded len = %d, size = %v", len(content), got["size"])
	}
	if got["filename"] != "report.pdf" || got["mime_type"] != "application/pdf" {
		t.Errorf("metadata = %v", got)
	}
}

func TestReadAttachmentErrors(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()
	uid, uv := findTestMessage(t, env, "Multi-part: alternative + related + mixed")
	base := map[string]any{
		"account_id":   "full",
		"mailbox":      "INBOX",
		"uid":          float64(uid),
		"uid_validity": float64(uv),
	}

	call := func(part string) error {
		args := map[string]any{}
		for k, v := range base {
			args[k] = v
		}
		args["part"] = part
		_, err := env.call(t, "read_attachment", args)
		return err
	}

	if err := call("1"); !hasToolMessage(err, "attachment not found") {
		t.Errorf("container part err = %v, want AttachmentNotFound", err)
	}
	if err := call("99"); !hasToolMessage(err, "attachment not found") {
		t.Errorf("missing part err = %v, want AttachmentNotFound", err)
	}
	if err := call("0"); !hasToolMessage(err, "invalid attachment part") {
		t.Errorf("zero part err = %v, want InvalidInput", err)
	}
	if err := call("1..2"); !hasToolMessage(err, "invalid attachment part") {
		t.Errorf("empty component err = %v, want InvalidInput", err)
	}
	if err := call("../../etc/passwd"); !hasToolMessage(err, "invalid attachment part") {
		t.Errorf("path traversal err = %v, want InvalidInput", err)
	}
	if err := call("501"); !hasToolMessage(err, "invalid attachment part") {
		t.Errorf("part-count overflow err = %v, want InvalidInput", err)
	}
	if err := call(strings.Repeat("1", 129)); !hasToolMessage(err, "invalid attachment part") {
		t.Errorf("oversize part path err = %v, want InvalidInput", err)
	}
	if err := call("1.2.3.4.5.6.7.8.9.10.11.12.13.14.15.16.17"); !hasToolMessage(err, "invalid attachment part") {
		t.Errorf("depth overflow err = %v, want InvalidInput", err)
	}

	wrongUV := map[string]any{}
	for k, v := range base {
		wrongUV[k] = v
	}
	wrongUV["uid_validity"] = float64(uv + 1)
	wrongUV["part"] = "2"
	if _, err := env.call(t, "read_attachment", wrongUV); !hasToolMessage(err, "uid/uid_validity mismatch") {
		t.Errorf("stale UIDVALIDITY err = %v, want UidMismatch", err)
	}

	missing := map[string]any{}
	for k, v := range base {
		missing[k] = v
	}
	missing["uid"] = float64(999999)
	missing["part"] = "2"
	if _, err := env.call(t, "read_attachment", missing); !hasToolMessage(err, "message not found") {
		t.Errorf("missing UID err = %v, want MessageNotFound", err)
	}
}

func TestReadAttachmentPermissionAndInputErrors(t *testing.T) {
	env := setupEnv(t, "full")
	defer env.cleanup()

	args := map[string]any{
		"account_id":   "norights",
		"mailbox":      "INBOX",
		"uid":          float64(1),
		"uid_validity": float64(1),
		"part":         "1",
	}
	if _, err := env.call(t, "read_attachment", args); !hasToolMessage(err, "lacks capability") {
		t.Errorf("no-read account err = %v, want PermissionDenied", err)
	}

	delete(args, "part")
	args["account_id"] = "full"
	args["mailbox"] = "INBOX"
	if _, err := env.call(t, "read_attachment", args); !hasToolMessage(err, "missing properties") {
		t.Errorf("missing part err = %v, want schema required-property error", err)
	}

	args["part"] = ""
	if _, err := env.call(t, "read_attachment", args); !hasToolMessage(err, "part is required") {
		t.Errorf("empty part err = %v, want InvalidInput", err)
	}

	args["part"] = "1"
	args["uid_validity"] = float64(0)
	if _, err := env.call(t, "read_attachment", args); !hasToolMessage(err, "uid_validity is required") {
		t.Errorf("missing UIDVALIDITY err = %v, want InvalidInput", err)
	}

	args["account_id"] = "missing"
	args["uid_validity"] = float64(1)
	if _, err := env.call(t, "read_attachment", args); !hasToolMessage(err, "account not found") {
		t.Errorf("missing account err = %v, want AccountNotFound", err)
	}
}

func TestMapErrorAttachmentCodes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"total attachment", mailbox.ErrTotalAttachmentTooLarge, codeAttachmentTooLarge},
		{"attachment not found", mailbox.ErrAttachmentNotFound, codeAttachmentNotFound},
		{"invalid part", mailbox.ErrInvalidPart, codeInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapError(tc.err)
			if got.Code != tc.code {
				t.Errorf("code = %q, want %q", got.Code, tc.code)
			}
		})
	}
}

// TestMapErrorListTooLarge checks that a mailbox that is too large to
// list exactly maps onto the documented limit-exceeded code and keeps
// the actionable detail, so the agent can narrow the query or raise the
// limit instead of retrying unchanged.
func TestMapErrorListTooLarge(t *testing.T) {
	err := fmt.Errorf("%w: mailbox %q holds %d messages, more than the list scan budget of %d",
		mailbox.ErrListTooLarge, "INBOX", 50000, 10000)
	got := mapError(err)
	if got.Code != codeLimitExceeded {
		t.Errorf("code = %q, want %q", got.Code, codeLimitExceeded)
	}
	if !strings.Contains(got.Message, "50000") || !strings.Contains(got.Message, "10000") {
		t.Errorf("message = %q, want it to carry the message count and the budget", got.Message)
	}
}

func hasToolMessage(err error, want string) bool {
	return err != nil && strings.Contains(err.Error(), want)
}
