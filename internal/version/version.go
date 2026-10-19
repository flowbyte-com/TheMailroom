// Package version exposes build-time version metadata.
package version

// These variables are intended to be overridden via -ldflags at release time:
//
//	go build -ldflags "-X github.com/flowbyte-com/themailroom/internal/version.Version=v1.2.3 \
//	                 -X github.com/flowbyte-com/themailroom/internal/version.Commit=$(git rev-parse HEAD)" \
//	         ./cmd/themailroom
var (
	// Version is the semantic version of the binary.
	Version = "0.1.0-dev"

	// Commit is the git commit SHA the binary was built from.
	Commit = "unknown"

	// BuildDate is the RFC 3339 timestamp of the build.
	BuildDate = "unknown"
)

// Info returns a structured representation suitable for log output and the
// MCP server info endpoint.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
}

// Get returns the current version info.
func Get() Info {
	return Info{Version: Version, Commit: Commit, BuildDate: BuildDate}
}
