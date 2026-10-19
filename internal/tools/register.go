package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/mailbox"
	"github.com/flowbyte-com/themailroom/internal/permissions"
)

// Register wires every tool defined in this package into the MCP server.
//
// All handlers are inlined so the literal return signatures exactly
// match mcp.ToolHandlerFor (which uses named returns). Tool definitions
// and arg/result types live in the per-tool files.

// Register wires every tool defined in this package into the MCP server.
//
// All handlers are inlined so the literal return signatures exactly
// match mcp.ToolHandlerFor (which uses named returns). Tool definitions
// and arg/result types live in the per-tool files.
func Register(server *mcp.Server, registry *accounts.Registry) {

	// list_accounts -- no input, no permission check (the tool itself
	// only returns non-sensitive account metadata).
	mcp.AddTool(server, listAccountsTool,
		func(_ context.Context, _ *mcp.CallToolRequest, _ ListAccountsArgs) (result *mcp.CallToolResult, output *ListAccountsResult, err error) {
			all := registry.All()
			out := make([]accountView, len(all))
			for i, a := range all {
				out[i] = accountViewFrom(a)
			}
			output = &ListAccountsResult{Accounts: out}
			return
		})

	// list_mailboxes
	mcp.AddTool(server, listMailboxesTool,
		func(ctx context.Context, _ *mcp.CallToolRequest, args ListMailboxesArgs) (result *mcp.CallToolResult, output *ListMailboxesResult, err error) {
			if args.AccountID == "" {
				err = invalidInput("account_id is required")
				return
			}
			acc, gerr := registry.Get(args.AccountID)
			if gerr != nil {
				err = mapError(gerr)
				return
			}
			if !acc.Permissions.Has(permissions.CapRead) {
				err = mapError(&permissions.ErrPermissionDenied{Account: acc.ID, Capability: permissions.CapRead})
				return
			}
			mbs, berr := acc.Backend().Mailboxes(ctx, args.AccountID)
			if berr != nil {
				err = mapError(berr)
				return
			}
			views := make([]mailboxView, len(mbs))
			for i, m := range mbs {
				views[i] = mailboxViewFrom(m)
			}
			output = &ListMailboxesResult{AccountID: args.AccountID, Mailboxes: views}
			return
		})

	// list_messages
	mcp.AddTool(server, listMessagesTool,
		func(ctx context.Context, _ *mcp.CallToolRequest, args ListMessagesArgs) (result *mcp.CallToolResult, output *ListMessagesResult, err error) {
			if args.AccountID == "" {
				err = invalidInput("account_id is required")
				return
			}
			if args.Mailbox == "" {
				err = invalidInput("mailbox is required")
				return
			}
			if args.Offset < 0 {
				err = invalidInput("offset must be non-negative")
				return
			}
			acc, gerr := registry.Get(args.AccountID)
			if gerr != nil {
				err = mapError(gerr)
				return
			}
			if !acc.Permissions.Has(permissions.CapRead) {
				err = mapError(&permissions.ErrPermissionDenied{Account: acc.ID, Capability: permissions.CapRead})
				return
			}
			max := acc.Limits.MaxBatchOperations
			limit := args.Limit
			if limit <= 0 || limit > max {
				limit = max
			}
			listResult, berr := acc.Backend().ListMessages(ctx, args.AccountID, args.Mailbox, mailbox.ListOptions{
				Limit:  limit,
				Offset: args.Offset,
				Since:  args.Since,
				Before: args.Before,
			})
			if berr != nil {
				err = mapError(berr)
				return
			}
			summaries := make([]messageSummaryV, len(listResult.Messages))
			for i, m := range listResult.Messages {
				summaries[i] = messageSummaryFrom(m)
			}
			output = &ListMessagesResult{
				AccountID:   args.AccountID,
				Mailbox:     args.Mailbox,
				UIDValidity: listResult.UIDValidity,
				Messages:    summaries,
			}
			return
		})

	// read_message
	mcp.AddTool(server, readMessageTool,
		func(ctx context.Context, _ *mcp.CallToolRequest, args ReadMessageArgs) (result *mcp.CallToolResult, output *ReadMessageResult, err error) {
			if args.AccountID == "" {
				err = invalidInput("account_id is required")
				return
			}
			if args.Mailbox == "" {
				err = invalidInput("mailbox is required")
				return
			}
			if args.UID == 0 {
				err = invalidInput("uid is required")
				return
			}
			if args.UIDValidity == 0 {
				err = invalidInput("uid_validity is required; obtain it from list_messages or search_messages before calling read_message")
				return
			}
			acc, gerr := registry.Get(args.AccountID)
			if gerr != nil {
				err = mapError(gerr)
				return
			}
			if !acc.Permissions.Has(permissions.CapRead) {
				err = mapError(&permissions.ErrPermissionDenied{Account: acc.ID, Capability: permissions.CapRead})
				return
			}
			msg, berr := acc.Backend().FetchMessage(ctx, args.AccountID, args.Mailbox, args.UID, args.UIDValidity)
			if berr != nil {
				err = mapError(berr)
				return
			}
			truncated := msg.Truncated
			if msg.Size() > acc.Limits.MaxMessageBytes {
				msg.Text = truncateRunes(msg.Text, acc.Limits.MaxMessageBytes)
				msg.HTML = truncateRunes(msg.HTML, acc.Limits.MaxMessageBytes)
				truncated = true
			}
			atts := make([]attachmentView, len(msg.Attachments))
			for i, a := range msg.Attachments {
				atts[i] = attachmentFrom(a)
			}
			flags := msg.FlagsSlice()
			if flags == nil {
				flags = []string{}
			}
			output = &ReadMessageResult{
				AccountID:   args.AccountID,
				Mailbox:     args.Mailbox,
				UID:         msg.UID,
				UIDValidity: msg.UIDValidity,
				Flags:       flags,
				Headers:     headersFrom(msg.Headers),
				Text:        msg.Text,
				HTML:        msg.HTML,
				Attachments: atts,
				Warnings:    msg.Warnings,
				Truncated:   truncated,
			}
			return
		})

	// search_messages
	mcp.AddTool(server, searchMessagesTool,
		func(ctx context.Context, _ *mcp.CallToolRequest, args SearchMessagesArgs) (result *mcp.CallToolResult, output *SearchMessagesResult, err error) {
			if args.AccountID == "" {
				err = invalidInput("account_id is required")
				return
			}
			if args.Mailbox == "" {
				err = invalidInput("mailbox is required")
				return
			}
			acc, gerr := registry.Get(args.AccountID)
			if gerr != nil {
				err = mapError(gerr)
				return
			}
			if !acc.Permissions.Has(permissions.CapRead) {
				err = mapError(&permissions.ErrPermissionDenied{Account: acc.ID, Capability: permissions.CapRead})
				return
			}
			q := mailbox.SearchQuery{
				Subject: args.Query.Subject,
				From:    args.Query.From,
				To:      args.Query.To,
				Body:    args.Query.Body,
				Since:   args.Query.Since,
				Before:  args.Query.Before,
				Flagged: args.Query.Flagged,
				Unseen:  args.Query.Unseen,
			}
			max := acc.Limits.MaxSearchResults
			if q.Limit <= 0 || q.Limit > max {
				q.Limit = max
			}
			listResult, berr := acc.Backend().SearchMessages(ctx, args.AccountID, args.Mailbox, q)
			if berr != nil {
				err = mapError(berr)
				return
			}
			summaries := make([]messageSummaryV, len(listResult.Messages))
			for i, m := range listResult.Messages {
				summaries[i] = messageSummaryFrom(m)
			}
			output = &SearchMessagesResult{
				AccountID:   args.AccountID,
				Mailbox:     args.Mailbox,
				UIDValidity: listResult.UIDValidity,
				Matches:     summaries,
			}
			return
		})
}

// truncateRunes is the only remaining helper; the previous
// uidValidityOf helper was removed because the Backend's
// ListMessages / SearchMessages now return a ListResult that
// binds the UIDs and the UIDVALIDITY together at the source.

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	return s[:max] + "…[truncated]"
}
