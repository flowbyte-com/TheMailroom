package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/attachments"
	"github.com/flowbyte-com/themailroom/internal/mailcompose"
	"github.com/flowbyte-com/themailroom/internal/permissions"
	"github.com/flowbyte-com/themailroom/internal/recipients"
	"github.com/flowbyte-com/themailroom/internal/smtpclient"
)

var sendMessageTool = &mcp.Tool{
	Name: "send_message",
	Description: "Submit a new message through the account's authenticated SMTP server. " +
		"Requires the 'send' capability; attachments additionally require 'attachment_send' and relative paths under a configured attachment root. " +
		"Returns 'submitted' only after the SMTP server explicitly accepts the message. SMTP acceptance does not prove final mailbox delivery. " +
		"Ambiguous failures are reported with a Message-ID and must not be retried automatically.",
}

// SendMessageArgs is the send_message input. It deliberately has no From,
// arbitrary headers, raw MIME, credentials or filesystem-root fields.
type SendMessageArgs struct {
	AccountID   string   `json:"account_id" jsonschema:"the configured account to send from"`
	To          []string `json:"to,omitempty" jsonschema:"primary recipients"`
	Cc          []string `json:"cc,omitempty" jsonschema:"carbon-copy recipients"`
	Bcc         []string `json:"bcc,omitempty" jsonschema:"blind carbon-copy recipients; included in the SMTP envelope but never in MIME headers"`
	Subject     string   `json:"subject,omitempty" jsonschema:"message subject"`
	Text        string   `json:"text,omitempty" jsonschema:"plain text body"`
	HTML        string   `json:"html,omitempty" jsonschema:"HTML body"`
	Attachments []string `json:"attachments,omitempty" jsonschema:"relative attachment paths resolved only through the account's confined attachment roots"`
}

// SendMessageResult is returned only after confirmed SMTP acceptance.
type SendMessageResult struct {
	AccountID            string `json:"account_id"`
	MessageID            string `json:"message_id"`
	Status               string `json:"status"`
	RecipientCount       int    `json:"recipient_count"`
	AttachmentCount      int    `json:"attachment_count"`
	TotalAttachmentBytes int64  `json:"total_attachment_bytes"`
}

type registerConfig struct {
	sender smtpclient.Sender
	logger *slog.Logger
}

// RegisterOption customises tool registration. The defaults use the real SMTP
// sender and discard audit logs; the MCP server supplies its configured
// logger.
type RegisterOption func(*registerConfig)

// WithSender replaces the SMTP sender, primarily for tests and embedding.
func WithSender(sender smtpclient.Sender) RegisterOption {
	return func(cfg *registerConfig) {
		if sender != nil {
			cfg.sender = sender
		}
	}
}

// WithLogger sets the audit logger.
func WithLogger(logger *slog.Logger) RegisterOption {
	return func(cfg *registerConfig) {
		if logger != nil {
			cfg.logger = logger
		}
	}
}

func defaultRegisterConfig() registerConfig {
	return registerConfig{
		sender: smtpclient.New(),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func registerSendMessage(server *mcp.Server, registry *accounts.Registry, cfg registerConfig) {
	mcp.AddTool(server, sendMessageTool,
		func(ctx context.Context, _ *mcp.CallToolRequest, args SendMessageArgs) (result *mcp.CallToolResult, output *SendMessageResult, err error) {
			return sendMessage(ctx, registry, cfg.sender, cfg.logger, args)
		})
}

func sendMessage(ctx context.Context, registry *accounts.Registry, sender smtpclient.Sender, logger *slog.Logger, args SendMessageArgs) (result *mcp.CallToolResult, output *SendMessageResult, err error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if sender == nil {
		err = typed(codeInternalError, "internal error", nil)
		return
	}
	var (
		messageID            string
		recipientCount       int
		attachmentCount      int
		totalAttachmentBytes int64
		attempted            bool
	)
	defer func() {
		if err == nil {
			logger.Info("send_message",
				"account_id", args.AccountID,
				"message_id", messageID,
				"recipient_count", recipientCount,
				"attachment_count", attachmentCount,
				"total_attachment_bytes", totalAttachmentBytes,
				"outcome", "submitted",
			)
			return
		}
		typedErr := mapError(err)
		outcome := "preflight_failure"
		if attempted {
			outcome = "definite_failure"
			var ambiguous *smtpclient.AmbiguousError
			if errors.As(err, &ambiguous) {
				outcome = "ambiguous_submission"
			}
		}
		logger.Info("send_message",
			"account_id", args.AccountID,
			"message_id", messageID,
			"recipient_count", recipientCount,
			"attachment_count", attachmentCount,
			"total_attachment_bytes", totalAttachmentBytes,
			"outcome", outcome,
			"error_code", typedErr.Code,
		)
		err = typedErr
	}()

	if args.AccountID == "" {
		err = invalidInput("account_id is required")
		return
	}
	acc, gerr := registry.Get(args.AccountID)
	if gerr != nil {
		err = gerr
		return
	}
	if !acc.Permissions.Has(permissions.CapSend) {
		err = &permissions.ErrPermissionDenied{Account: acc.ID, Capability: permissions.CapSend}
		return
	}
	fromEnvelope, ferr := recipients.Canonical(acc.Email)
	if ferr != nil {
		err = ferr
		return
	}

	to, cerr := canonicalValues(args.To)
	if cerr != nil {
		err = cerr
		return
	}
	cc, cerr := canonicalValues(args.Cc)
	if cerr != nil {
		err = cerr
		return
	}
	bcc, cerr := canonicalValues(args.Bcc)
	if cerr != nil {
		err = cerr
		return
	}
	envelope, perr := acc.RecipientPolicy.Validate(to, cc, bcc)
	if perr != nil {
		err = perr
		return
	}
	recipientCount = len(envelope)

	// SMTPUTF8 is not requested in this implementation. Reject every
	// non-ASCII envelope address before opening a connection.
	if !allASCII(envelope) || !isASCIIString(fromEnvelope) {
		err = invalidInput("SMTPUTF8 envelope addresses are not supported")
		return
	}

	var files []attachments.File
	if len(args.Attachments) > 0 {
		if !acc.Permissions.Has(permissions.CapAttachmentSend) {
			err = &permissions.ErrPermissionDenied{Account: acc.ID, Capability: permissions.CapAttachmentSend}
			return
		}
		resolver := acc.AttachmentResolver()
		if resolver == nil {
			err = invalidInput("attachment sending is not configured for this account")
			return
		}
		files, err = resolver.Resolve(args.Attachments)
		if err != nil {
			return
		}
		attachmentCount = len(files)
		for _, file := range files {
			totalAttachmentBytes += int64(len(file.Data))
		}
	}

	composed, cerr := mailcompose.Compose(mailcompose.Request{
		From:        acc.Email,
		To:          to,
		Cc:          cc,
		Bcc:         bcc,
		Subject:     args.Subject,
		Text:        args.Text,
		HTML:        args.HTML,
		Attachments: files,
	}, acc.Limits)
	if cerr != nil {
		err = cerr
		return
	}
	if acc.Limits.MaxMessageBytes <= 0 || len(composed.Message) > acc.Limits.MaxMessageBytes {
		err = mailcompose.ErrMessageTooLarge
		return
	}
	messageID = composed.MessageID

	// Everything above is preflight. Only now may a connection be opened.
	attempted = true
	smtpResult, serr := sender.Send(ctx, smtpclient.Request{
		AccountID:  acc.ID,
		Config:     acc.SMTPSubmissionConfig(),
		From:       fromEnvelope,
		Recipients: envelope,
		Message:    composed.Message,
		MessageID:  messageID,
	})
	if serr != nil {
		err = serr
		return
	}
	if smtpResult.MessageID != "" && smtpResult.MessageID != messageID {
		// A sender must not substitute or invent a different identity.
		err = invalidInput("sender returned an inconsistent Message-ID")
		return
	}

	output = &SendMessageResult{
		AccountID:            acc.ID,
		MessageID:            messageID,
		Status:               "submitted",
		RecipientCount:       recipientCount,
		AttachmentCount:      attachmentCount,
		TotalAttachmentBytes: totalAttachmentBytes,
	}
	return
}

func canonicalValues(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		addr, err := recipients.Canonical(value)
		if err != nil {
			return nil, err
		}
		out = append(out, addr)
	}
	return out, nil
}

func allASCII(values []string) bool {
	for _, value := range values {
		if !isASCIIString(value) {
			return false
		}
	}
	return true
}

func isASCIIString(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] > 127 {
			return false
		}
	}
	return true
}
