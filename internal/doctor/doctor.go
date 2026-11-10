package doctor

import (
	"context"
	"time"
)

// Options configures one diagnostic run.
type Options struct {
	// ConfigPath is the configuration file to inspect.
	ConfigPath string
	// Network requests the opt-in network checks. When false the run is
	// strictly local and opens no sockets.
	Network bool
	// Timeout bounds the whole network phase. Zero selects
	// DefaultNetworkTimeout.
	Timeout time.Duration
	// Probe is a test seam. Nil selects the real network.
	Probe netProbe
}

// Run performs the requested diagnostics.
//
// Local checks always run. Network checks run only when Options.Network is
// set and the configuration loaded, and are bounded by Options.Timeout.
func Run(ctx context.Context, opts Options) Report {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = "themailroom.yaml"
	}

	report := Report{ConfigPath: opts.ConfigPath, Network: opts.Network}

	local := RunLocal(opts.ConfigPath)
	report.ConfigPath = local.Path
	report.Checks = append(report.Checks, local.Checks...)

	if !opts.Network {
		return report
	}
	if !local.Loaded {
		report.Add(Check{
			Name:   "network",
			Status: StatusSkip,
			Detail: "network checks need a valid configuration; none were run",
		})
		return report
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultNetworkTimeout
	}
	networkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	probe := opts.Probe
	if probe == nil {
		probe = realProbe{dialTimeout: 10 * time.Second, handshakeTimeout: 10 * time.Second}
	}
	report.Checks = append(report.Checks, RunNetwork(networkCtx, local.Config, probe)...)
	return report
}
