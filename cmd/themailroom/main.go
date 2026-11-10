// Command themailroom is the MCP server entry point.
//
// Usage:
//
//	themailroom -config /etc/themailroom.yaml
//	themailroom setup [-config PATH] [-preset purelymail] [-force]
//	themailroom doctor [-config PATH] [-network] [-timeout DURATION]
//
// The server reads configuration, builds the account registry, registers
// every MCP tool, and blocks serving requests over stdio. The process
// exits cleanly when the parent closes stdin or sends SIGTERM.
//
// The setup command writes a new configuration file. It never asks for a
// plaintext password, never opens a network connection and never starts the
// server.
//
// The doctor command diagnoses the configuration without serving. It runs
// local checks by default and network checks only when -network is given.
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
	"github.com/flowbyte-com/themailroom/internal/doctor"
	"github.com/flowbyte-com/themailroom/internal/mcpserver"
	"github.com/flowbyte-com/themailroom/internal/setup"
	"github.com/flowbyte-com/themailroom/internal/version"
)

// Exit codes for the doctor and setup commands.
const (
	doctorExitOK     = 0
	doctorExitFailed = 1
	doctorExitUsage  = 2

	setupExitOK     = 0
	setupExitFailed = 1
	setupExitUsage  = 2
)

func main() {
	// setup and doctor are subcommands, not serving modes, so they are
	// dispatched before the server's flags are parsed.
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		os.Exit(runSetup(os.Args[2:], os.Stdin, os.Stdout))
	}
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(runDoctor(os.Args[2:], os.Stdout))
	}
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

// runSetup runs the first-run configuration command and returns its exit
// code.
//
// The connectivity check is offered only after the configuration is
// written, and only as an explicit yes. Declining leaves the process having
// opened no socket and run no credential command.
func runSetup(args []string, stdin io.Reader, stdout io.Writer) int {
	fs := flag.NewFlagSet("themailroom setup", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "where to write the configuration")
	presetName := fs.String("preset", "", "provider preset for the interview defaults (purelymail)")
	force := fs.Bool("force", false, "replace an existing regular file")
	if err := fs.Parse(args); err != nil {
		return setupExitUsage
	}

	// An unknown preset is a usage error: a typo must not silently produce a
	// configuration with the wrong servers.
	preset, presetErr := setup.LookupPreset(*presetName)
	if presetErr != nil {
		fmt.Fprintf(os.Stderr, "themailroom: %v\n\n", presetErr)
		fs.Usage()
		return setupExitUsage
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	err := setup.Run(ctx, setup.Options{
		Path:   *cfgPath,
		Force:  *force,
		Preset: preset,
		In:     stdin,
		Out:    stdout,
		Check:  setupCheck,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "themailroom: %v\n", err)
		return setupExitFailed
	}
	return setupExitOK
}

// errCheckFailed is returned when the operator asked for a connectivity
// check and the diagnostic reported problems. The configuration has already
// been written and is deliberately left in place for troubleshooting.
var errCheckFailed = errors.New(
	"the connectivity check reported problems; the configuration was written and can be inspected with the doctor command")

// renderCheck prints a diagnostic report and reports whether it passed.
//
// Printing and judging are separate on purpose: a report that prints
// successfully can still describe failures, and those failures must reach
// the exit status.
func renderCheck(report doctor.Report, out io.Writer) error {
	if err := report.WriteText(out); err != nil {
		return err
	}
	if report.Failed() {
		return errCheckFailed
	}
	return nil
}

// setupCheck is the connectivity check the setup command offers. It is a
// variable so a test can substitute a deterministic diagnostic.
var setupCheck = func(ctx context.Context, path string, out io.Writer) error {
	return renderCheck(doctor.Run(ctx, doctor.Options{ConfigPath: path, Network: true}), out)
}

// runDoctor runs the diagnostic command and returns its exit code.
//
// The report goes to stdout so it can be captured; usage errors go to
// stderr. A warning does not fail the command: only a failed check does.
func runDoctor(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("themailroom doctor", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", defaultConfigPath(), "path to configuration file")
	network := fs.Bool("network", false, "also test network connectivity and authentication (opt-in)")
	timeout := fs.Duration("timeout", doctor.DefaultNetworkTimeout, "overall bound for the network checks")
	if err := fs.Parse(args); err != nil {
		return doctorExitUsage
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	report := doctor.Run(ctx, doctor.Options{
		ConfigPath: *cfgPath,
		Network:    *network,
		Timeout:    *timeout,
	})
	if err := report.WriteText(stdout); err != nil {
		fmt.Fprintf(os.Stderr, "themailroom: %v\n", err)
		return doctorExitFailed
	}
	if report.Failed() {
		return doctorExitFailed
	}
	return doctorExitOK
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

	// Log a redacted view of the config so operators can confirm what
	// was loaded without leaking credentials.
	logger.Info("config loaded", "path", cfgPath, "summary", cfg.Redacted())

	registry, err := accounts.New(cfg)
	if err != nil {
		return err
	}
	defer registry.Close()

	server, err := mcpserver.New(registry, mcpserver.Options{
		Logger:           logger,
		MaxRequestBytes:  cfg.Server.MaxRequestBytes,
		MaxResponseBytes: cfg.Server.MaxResponseBytes,
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
