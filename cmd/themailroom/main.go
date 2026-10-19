// Command themailroom is the MCP server entry point.
//
// Usage:
//
//	themailroom -config /etc/themailroom.yaml
//
// The server reads configuration, builds the account registry, registers
// every MCP tool, and blocks serving requests over stdio. The process
// exits cleanly when the parent closes stdin or sends SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/flowbyte-com/themailroom/internal/accounts"
	"github.com/flowbyte-com/themailroom/internal/config"
	"github.com/flowbyte-com/themailroom/internal/mcpserver"
	"github.com/flowbyte-com/themailroom/internal/version"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			// Recover from a panic deep in the SDK or transport. We
			// do not print the stack to keep the JSON-RPC stream
			// clean; the structured logger is the diagnostic.
			fmt.Fprintf(os.Stderr, "themailroom: internal panic: %v\n", r)
			os.Exit(2)
		}
	}()
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "themailroom: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", defaultConfigPath(), "path to configuration file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		v := version.Get()
		fmt.Printf("themailroom %s (commit %s, built %s)\n", v.Version, v.Commit, v.BuildDate)
		return nil
	}

	if err := loadAndServe(*cfgPath); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return nil
}

// defaultConfigPath looks for a configuration file in conventional
// locations, returning the first match.
func defaultConfigPath() string {
	candidates := []string{
		"themailroom.yaml",
		"themailroom.yml",
		"config/themailroom.yaml",
		"/etc/themailroom/themailroom.yaml",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return "themailroom.yaml"
}

// loadAndServe is the testable inner main.
func loadAndServe(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	logger, err := buildLogger(cfg.Server.LogLevel)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	registry, err := accounts.New(cfg)
	if err != nil {
		return err
	}

	server, err := mcpserver.New(registry, mcpserver.Options{
		Logger: logger,
	})
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := server.Run(ctx); err != nil && !isBenignExit(err) {
		return err
	}
	return nil
}

// buildLogger configures slog from a level string. We never log to a
// file that could contain message content; all output goes to stderr so
// it does not interfere with the stdio JSON-RPC stream.
func buildLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "", "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log_level %q", level)
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h), nil
}

// isBenignExit recognises the "expected" shutdown errors from the MCP
// stdio transport so we do not surface them as failures.
func isBenignExit(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "stdin: EOF") || strings.Contains(msg, "stdin closed")
}
