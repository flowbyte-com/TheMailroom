// Package mcpserver assembles the TheMailroom MCP server: it builds the
// official SDK Server, registers every tool, and exposes a stdio Run
// helper.
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/config"
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

	// MaxRequestBytes caps one inbound MCP JSON-RPC frame before JSON
	// decoding. The count excludes LF or CRLF framing. Zero selects the
	// configuration default.
	MaxRequestBytes int
}

// Server is the assembled MCP server.
type Server struct {
	mcp             *mcp.Server
	registry        *accounts.Registry
	logger          *slog.Logger
	maxRequestBytes int
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
	if opts.MaxRequestBytes == 0 {
		opts.MaxRequestBytes = config.DefaultMaxRequestBytes
	}
	if opts.MaxRequestBytes < 0 || opts.MaxRequestBytes > config.MaxRequestBytesLimit {
		return nil, fmt.Errorf(
			"mcpserver: max_request_bytes %d invalid; want between 1 and %d bytes",
			opts.MaxRequestBytes, config.MaxRequestBytesLimit,
		)
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
		mcp:             mcpServer,
		registry:        reg,
		logger:          opts.Logger,
		maxRequestBytes: opts.MaxRequestBytes,
	}, nil
}

// Run starts the server on stdio and blocks until the context is
// cancelled or the transport terminates.
func (s *Server) Run(ctx context.Context) error {
	return s.run(ctx, newBoundedTransport(os.Stdin, os.Stdout, s.maxRequestBytes))
}

func (s *Server) run(ctx context.Context, transport mcp.Transport) error {
	s.logger.Info("starting themailroom",
		"version", version.Version,
		"commit", version.Commit,
		"accounts", len(s.registry.All()),
		"max_request_bytes", s.maxRequestBytes,
	)
	return s.mcp.Run(ctx, transport)
}

// MCPServer exposes the underlying MCP server. It exists so tests can
// drive the server over an in-memory transport without going through stdio.
func (s *Server) MCPServer() *mcp.Server { return s.mcp }

// Registry exposes the account registry. Useful for tests and any
// future tool that needs direct account access.
func (s *Server) Registry() *accounts.Registry { return s.registry }
