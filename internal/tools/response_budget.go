package tools

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// responseEnvelopeAllowance covers the JSON-RPC envelope, the tool-call
// identifier and the small amount of framing around one CallToolResult.
// responseSize already includes the result object itself.
const responseEnvelopeAllowance = 1024

// responseTruncationMarker is appended to a body that had to be shortened.
const responseTruncationMarker = "…[truncated]"

// responseSize returns the approximate serialized size of one successful
// tool result. The SDK emits the structured result twice: once as
// structuredContent and once as its JSON text mirror in content. Both
// copies are counted, together with a fixed envelope allowance.
func responseSize(output any) (int, error) {
	raw, err := json.Marshal(output)
	if err != nil {
		return 0, err
	}
	res := mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(raw)}},
		StructuredContent: json.RawMessage(raw),
	}
	wire, err := json.Marshal(&res)
	if err != nil {
		return 0, err
	}
	return len(wire) + responseEnvelopeAllowance, nil
}

// checkResponseBudget refuses a result that cannot fit the configured
// response limit. Tools that can shorten a body use fitReadMessage
// instead; tools whose content must remain exact return this error with
// no partial result.
func checkResponseBudget(output any, max int) error {
	size, err := responseSize(output)
	if err != nil {
		return fmt.Errorf("tools: encode response: %w", err)
	}
	if size > max {
		return responseTooLargeError(max)
	}
	return nil
}

// responseTooLargeError is the fixed, content-free refusal for a result
// that exceeds server.max_response_bytes.
func responseTooLargeError(max int) *TypedError {
	return typed(codeLimitExceeded,
		fmt.Sprintf("response exceeds server.max_response_bytes (%d bytes); reduce the requested content or raise the server limit", max),
		map[string]any{"max_response_bytes": max})
}

// fitReadMessage shortens the text and HTML bodies, in aggregate, until
// the complete result fits max. It never splits a UTF-8 rune and never
// returns a partial result that claims to be complete: Truncated is set
// when either body was shortened.
func fitReadMessage(output *ReadMessageResult, max int) error {
	if output == nil {
		return nil
	}
	size, err := responseSize(output)
	if err != nil {
		return fmt.Errorf("tools: encode response: %w", err)
	}
	if size <= max {
		return nil
	}

	base := *output
	base.Text = ""
	base.HTML = ""
	baseSize, err := responseSize(&base)
	if err != nil {
		return fmt.Errorf("tools: encode response: %w", err)
	}
	if baseSize > max {
		return responseTooLargeError(max)
	}

	// Start from the full body budget and reduce it until the serialized
	// result, including JSON escaping and the SDK text mirror, fits. This
	// avoids assuming that one source byte equals one response byte.
	bodyBudget := len(output.Text) + len(output.HTML)
	for bodyBudget > 0 {
		bodyBudget = bodyBudget * 3 / 4
		candidate := *output
		truncateMessageBodies(&candidate, bodyBudget)
		size, err := responseSize(&candidate)
		if err != nil {
			return fmt.Errorf("tools: encode response: %w", err)
		}
		if size <= max {
			*output = candidate
			output.Truncated = true
			return nil
		}
	}

	*output = base
	output.Truncated = true
	return nil
}

// truncateMessageBodies applies one aggregate body budget. Plain text is
// retained first; the remaining budget is available to HTML. The total
// retained body bytes never exceed budget.
func truncateMessageBodies(output *ReadMessageResult, budget int) {
	if budget <= 0 {
		output.Text = ""
		output.HTML = ""
		return
	}
	if len(output.Text) >= budget {
		output.Text = truncateUTF8(output.Text, budget)
		output.HTML = ""
		return
	}
	remaining := budget - len(output.Text)
	output.HTML = truncateUTF8(output.HTML, remaining)
}

// truncateUTF8 returns a valid UTF-8 prefix of s of at most max bytes,
// with a truncation marker when s had to be shortened. It never cuts a
// multi-byte rune in half and never exceeds max.
func truncateUTF8(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if len(s) <= max {
		return s
	}
	if max <= len(responseTruncationMarker) {
		return ""
	}
	cut := max - len(responseTruncationMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + responseTruncationMarker
}

// encodeAttachmentContent applies the response budget before allocating
// the base64 copy. base64 has no JSON escapes, so two copies of the
// encoded content plus the envelope allowance are a lower bound on the
// final result size. The caller must still run checkResponseBudget on
// the complete result, which includes the metadata.
func encodeAttachmentContent(content []byte, max int) (string, error) {
	encodedLen := base64.StdEncoding.EncodedLen(len(content))
	if int64(encodedLen) > int64(max) ||
		int64(encodedLen)*2+responseEnvelopeAllowance > int64(max) {
		return "", responseTooLargeError(max)
	}
	return base64.StdEncoding.EncodeToString(content), nil
}
