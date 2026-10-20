package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

// listAccountsTool advertises the list_accounts tool.
var listAccountsTool = &mcp.Tool{
	Name:        "list_accounts",
	Description: "List configured email accounts. No credential material or host filesystem paths are returned.",
}

// ListAccountsArgs is intentionally empty; the tool takes no input.
type ListAccountsArgs struct{}

// ListAccountsResult is the structured output.
type ListAccountsResult struct {
	Accounts []accountView `json:"accounts"`
}
