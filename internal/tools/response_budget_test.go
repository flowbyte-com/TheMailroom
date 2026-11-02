package tools

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestResponseSizeIncludesBothSDKMirrors(t *testing.T) {
	output := &ListMessagesResult{
		AccountID:   "a",
		Mailbox:     "INBOX",
		UIDValidity: 1,
		Messages: []messageSummaryV{{
			UID:     1,
			From:    "sender@example.test",
			Subject: strings.Repeat("s", 64),
		}},
	}
	size, err := responseSize(output)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	raw, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if size <= len(raw)+responseEnvelopeAllowance {
		t.Errorf("responseSize = %d, want more than one JSON copy (%d) plus the envelope", size, len(raw))
	}
}

func TestCheckResponseBudgetExactBoundary(t *testing.T) {
	output := &ListAccountsResult{Accounts: []accountView{{
		ID:          "a",
		Email:       "a@example.test",
		Permissions: []string{"read"},
	}}}
	size, err := responseSize(output)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	if err := checkResponseBudget(output, size); err != nil {
		t.Errorf("exact boundary rejected: %v", err)
	}
	err = checkResponseBudget(output, size-1)
	if err == nil {
		t.Fatal("one byte over the boundary was accepted")
	}
	var typedErr *TypedError
	if !errors.As(err, &typedErr) || typedErr.Code != codeLimitExceeded {
		t.Errorf("err = %v, want a typed %s error", err, codeLimitExceeded)
	}
	if strings.Contains(err.Error(), "a@example.test") {
		t.Errorf("refusal leaked response content: %v", err)
	}
}

func TestTruncateUTF8(t *testing.T) {
	marker := responseTruncationMarker
	if got := truncateUTF8("abc", 10); got != "abc" {
		t.Errorf("short string = %q, want unchanged", got)
	}
	if got := truncateUTF8("abc", 3); got != "abc" {
		t.Errorf("exact boundary = %q, want unchanged", got)
	}
	if got := truncateUTF8("abc", 2); got != "" {
		t.Errorf("tiny budget = %q, want empty", got)
	}

	// The two-byte é straddles the nominal cut. The result must stay
	// within the cap and must not contain a broken rune.
	max := len(marker) + 2
	got := truncateUTF8("aé"+strings.Repeat("é", 10), max)
	if len(got) > max {
		t.Errorf("len(%q) = %d, want at most %d", got, len(got), max)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncated text is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, marker) {
		t.Errorf("truncated text = %q, want the truncation marker", got)
	}
	if strings.HasPrefix(got, "a\xc3") {
		t.Errorf("truncation split the é rune: %q", got)
	}

	invalid := truncateUTF8(string(bytes.Repeat([]byte{0xff}, 40)), 16)
	if !utf8.ValidString(invalid) {
		t.Errorf("invalid input produced invalid output: %q", invalid)
	}
	shortInvalid := truncateUTF8(string([]byte{0xff, 'a'}), 64)
	if !utf8.ValidString(shortInvalid) {
		t.Errorf("short invalid input produced invalid output: %q", shortInvalid)
	}
}

func TestFitReadMessageAggregateTruncation(t *testing.T) {
	text := strings.Repeat("a", 4000)
	html := strings.Repeat("é", 4000)
	output := &ReadMessageResult{
		AccountID:   "a",
		Mailbox:     "INBOX",
		UID:         1,
		UIDValidity: 1,
		Text:        text,
		HTML:        html,
	}

	const max = 2048
	if err := fitReadMessage(output, max); err != nil {
		t.Fatalf("fitReadMessage: %v", err)
	}
	if !output.Truncated {
		t.Error("fitReadMessage did not mark the result truncated")
	}
	if len(output.Text)+len(output.HTML) >= len(text)+len(html) {
		t.Error("fitReadMessage did not reduce the aggregate body")
	}
	if !utf8.ValidString(output.Text) || !utf8.ValidString(output.HTML) {
		t.Error("fitReadMessage produced invalid UTF-8")
	}
	size, err := responseSize(output)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	if size > max {
		t.Errorf("response size = %d, want at most %d", size, max)
	}
}

func TestFitReadMessageZeroBodyExactBoundary(t *testing.T) {
	output := &ReadMessageResult{AccountID: "a", Mailbox: "INBOX"}
	size, err := responseSize(output)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	if err := fitReadMessage(output, size); err != nil {
		t.Fatalf("fitReadMessage: %v", err)
	}
	if output.Truncated {
		t.Error("zero-body exact boundary was marked truncated")
	}
	final, err := responseSize(output)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	if final > size {
		t.Errorf("final size = %d, want at most %d", final, size)
	}
}

func TestFitReadMessageTruncatedCandidateNearLimit(t *testing.T) {
	output := &ReadMessageResult{
		AccountID: "a",
		Text:      strings.Repeat("a", 2000),
	}

	// The first fit iteration uses a body budget of 1500. Measure that
	// exact candidate both before and after the flag is set, then choose
	// a limit where the un-flagged copy fits but the final flagged copy
	// would not. The final result must still respect the limit.
	candidate := *output
	truncateMessageBodies(&candidate, 1500)
	candidate.Truncated = false
	withoutFlag, err := responseSize(&candidate)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	candidate.Truncated = true
	withFlag, err := responseSize(&candidate)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	if withFlag <= withoutFlag {
		t.Fatalf("truncation flag did not add bytes: without=%d with=%d", withoutFlag, withFlag)
	}
	limit := withFlag - 1
	if withoutFlag > limit {
		t.Skipf("candidate boundary is not separated: without=%d limit=%d", withoutFlag, limit)
	}

	if err := fitReadMessage(output, limit); err != nil {
		t.Fatalf("fitReadMessage: %v", err)
	}
	if !output.Truncated {
		t.Error("fitReadMessage did not mark the result truncated")
	}
	final, err := responseSize(output)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	if final > limit {
		t.Errorf("final size = %d, exceeds limit %d", final, limit)
	}
}

func TestFitReadMessageExactBoundaryIsUnchanged(t *testing.T) {
	output := &ReadMessageResult{
		AccountID:   "a",
		Mailbox:     "INBOX",
		UID:         1,
		UIDValidity: 1,
		Text:        "hello",
		HTML:        "<p>hello</p>",
	}
	size, err := responseSize(output)
	if err != nil {
		t.Fatalf("responseSize: %v", err)
	}
	if err := fitReadMessage(output, size); err != nil {
		t.Fatalf("fitReadMessage: %v", err)
	}
	if output.Truncated || output.Text != "hello" || output.HTML != "<p>hello</p>" {
		t.Errorf("exact boundary changed the result: %+v", output)
	}
}

func TestReadMessageResponseBudgetTruncatesUTF8(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("é", 4096)
	raw := "From: sender@example.test\r\n" +
		"Subject: Big body\r\n" +
		"Date: Fri, 19 Jan 2024 12:00:00 +0000\r\n" +
		"Message-Id: <big@example.test>\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + body
	if err := os.WriteFile(filepath.Join(dir, "big.eml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}

	env := setupEnvWithMailbox(t, dir, WithMaxResponseBytes(4096))
	defer env.cleanup()
	uid, uv := findTestMessage(t, env, "Big body")
	got, err := env.call(t, "read_message", map[string]any{
		"account_id":   "full",
		"mailbox":      "INBOX",
		"uid":          float64(uid),
		"uid_validity": float64(uv),
	})
	if err != nil {
		t.Fatalf("read_message: %v", err)
	}
	text, ok := got["text"].(string)
	if !ok {
		t.Fatalf("text missing or wrong type: %T", got["text"])
	}
	if truncated, _ := got["truncated"].(bool); !truncated {
		t.Error("read_message did not mark the response truncated")
	}
	if len(text) >= len(body) {
		t.Errorf("text length = %d, want less than the original %d", len(text), len(body))
	}
	if !utf8.ValidString(text) {
		t.Error("read_message returned invalid UTF-8 after truncation")
	}
}

func TestFitReadMessageMetadataOverBudget(t *testing.T) {
	output := &ReadMessageResult{
		AccountID: "a",
		Text:      "hello",
	}
	err := fitReadMessage(output, 1)
	if err == nil {
		t.Fatal("fitReadMessage accepted a response whose metadata alone exceeds the cap")
	}
	var typedErr *TypedError
	if !errors.As(err, &typedErr) || typedErr.Code != codeLimitExceeded {
		t.Errorf("err = %v, want a typed %s error", err, codeLimitExceeded)
	}
}

func TestCheckResponseBudgetListAndSearch(t *testing.T) {
	messages := make([]messageSummaryV, 50)
	for i := range messages {
		messages[i] = messageSummaryV{
			UID:     uint32(i + 1),
			Subject: strings.Repeat("s", 4096),
		}
	}
	cases := []struct {
		name   string
		output any
	}{
		{"list", &ListMessagesResult{AccountID: "a", Mailbox: "INBOX", Messages: messages}},
		{"search", &SearchMessagesResult{AccountID: "a", Mailbox: "INBOX", Matches: messages}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkResponseBudget(tc.output, 1024); err == nil {
				t.Fatal("large result accepted under a small cap")
			}
		})
	}
}

func TestEncodeAttachmentContentBudget(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 32000)
	encoded, err := encodeAttachmentContent(content, 100000)
	if err != nil {
		t.Fatalf("encodeAttachmentContent: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if !bytes.Equal(decoded, content) {
		t.Error("encoded content did not round-trip")
	}

	if _, err := encodeAttachmentContent(content, 50000); err == nil {
		t.Fatal("content that cannot fit the duplicate response was encoded")
	}
}

func TestReadAttachmentResponseBudget(t *testing.T) {
	// Discover the stable part reference with an unrestricted server;
	// the capped server has the same fixture and therefore the same
	// UIDs and part paths.
	base := setupEnv(t, "full")
	defer base.cleanup()
	uid, uv := findTestMessage(t, base, "Multi-part: alternative + related + mixed")
	part := attachmentPart(t, base, uid, uv, "logo.png")

	env := setupEnvWithOptions(t, WithMaxResponseBytes(256))
	defer env.cleanup()
	_, err := env.call(t, "read_attachment", map[string]any{
		"account_id":   "full",
		"mailbox":      "INBOX",
		"uid":          float64(uid),
		"uid_validity": float64(uv),
		"part":         part,
	})
	if err == nil {
		t.Fatal("read_attachment accepted a response over the configured cap")
	}
	if !strings.Contains(err.Error(), "max_response_bytes") {
		t.Errorf("err = %v, want the response-budget refusal", err)
	}
}

func TestListAccountsResponseBudget(t *testing.T) {
	env := setupEnvWithOptions(t, WithMaxResponseBytes(64))
	defer env.cleanup()

	_, err := env.call(t, "list_accounts", map[string]any{})
	if err == nil {
		t.Fatal("list_accounts accepted a response over the configured cap")
	}
	if !strings.Contains(err.Error(), "max_response_bytes") {
		t.Errorf("err = %v, want the response-budget refusal", err)
	}
}
