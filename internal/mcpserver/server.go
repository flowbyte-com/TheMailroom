// Package mcpserver assembles the TheMailroom MCP server: it builds the
// official SDK Server, registers every tool, and exposes a stdio Run
// helper.
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/tools"
	"github.com/flowbyte-com/themailroom/internal/version"
)

// Options configures server construction.
type Options struct {
	// Implementation is the name advertised in MCP server info.
	Implementation string

	// Instructions is the human-readable description returned by
	// initialize. Keep it under 4 KiB.
	Instructions string

	// Logger is used for diagnostic output. If nil, slog.Default() is
	// used.
	Logger *slog.Logger
}

// Server is the assembled MCP server.
type Server struct {
	mcp      *mcp.Server
	registry *accounts.Registry
	logger   *slog.Logger
}

// New constructs a Server bound to the given account registry.
func New(reg *accounts.Registry, opts Options) (*Server, error) {
	if reg == nil {
		return nil, fmt.Errorf("mcpserver: registry is required")
	}
	if opts.Implementation == "" {
		opts.Implementation = "themailroom"
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	mcpImpl := &mcp.Implementation{
		Name:    opts.Implementation,
		Version: version.Version,
	}
	serverOpts := &mcp.ServerOptions{
		Instructions: opts.Instructions,
	}
	mcpServer := mcp.NewServer(mcpImpl, serverOpts)

	tools.Register(mcpServer, reg, tools.WithLogger(opts.Logger))

	return &Server{
		mcp:      mcpServer,
		registry: reg,
		logger:   opts.Logger,
	}, nil
}

// Run starts the server on stdio and blocks until the context is
// cancelled or the transport terminates.
func (s *Server) Run(ctx context.Context) error {
	s.logger.Info("starting themailroom",
		"version", version.Version,
		"commit", version.Commit,
		"accounts", len(s.registry.All()),
	)
	return s.mcp.Run(ctx, &mcp.StdioTransport{})
}

// MCPServer exposes the underlying MCP server. It exists so tests can
// drive the server over an in-memory transport without going through stdio.
func (s *Server) MCPServer() *mcp.Server { return s.mcp }

// Registry exposes the account registry. Useful for tests and any
// future tool that needs direct account access.
func (s *Server) Registry() *accounts.Registry { return s.registry }
