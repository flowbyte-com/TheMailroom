package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// BuildOptions configures a release build.
type BuildOptions struct {
	// Root is the repository root. The build runs there so that the
	// module and the toolchain resolve the same way they do in
	// development.
	Root string
	// Dist is the directory the artifacts are written to.
	Dist string
	// Metadata is the metadata injected into every binary.
	Metadata Metadata
	// Targets are the targets to build. Empty means Targets().
	Targets []Target
	// Go is the go command to use. Empty means "go" from PATH.
	Go string
	// Verbose prints each build command.
	Verbose bool
}

// Build compiles every target into the distribution directory and writes
// the checksum file.
//
// Every target is built with the same toolchain, the same flags and the
// same injected metadata, so the artifacts differ only in the platform
// they target. The environment is pinned:
//
//   - CGO_ENABLED=0 makes the binary static and removes the C toolchain
//     from the build, so a release does not depend on the C library
//     versions of the build machine.
//   - GOFLAGS=-mod=readonly keeps the build from rewriting go.mod, so the
//     build cannot quietly change the dependency set.
//   - GOAMD64=v1 and the Go default for the other architectures pin the
//     instruction set, so the same commit yields the same binary on
//     different microarchitectures.
//   - GOTOOLCHAIN=local stops the toolchain from downloading a different
//     Go version mid-build. A build that silently used another compiler
//     would not be reproducible.
func Build(opts BuildOptions) error {
	if err := opts.Metadata.Validate(); err != nil {
		return err
	}
	targets := opts.Targets
	if len(targets) == 0 {
		targets = Targets()
	}
	goCmd := opts.Go
	if goCmd == "" {
		goCmd = "go"
	}
	// The distribution directory is resolved against the caller's
	// working directory, not against the repository root: the build
	// runs in the repository root, but the two paths are independent.
	dist, err := filepath.Abs(opts.Dist)
	if err != nil {
		return fmt.Errorf("release: resolve %s: %w", opts.Dist, err)
	}
	if err := os.MkdirAll(dist, 0o755); err != nil {
		return fmt.Errorf("release: create %s: %w", dist, err)
	}

	ldflags := LDFlags(opts.Metadata)
	flags := BuildFlags()

	for _, target := range targets {
		out := filepath.Join(dist, ArtifactName(target))
		args := append([]string{"build"}, flags...)
		args = append(args, "-ldflags", ldflags, "-o", out, "./cmd/themailroom")

		cmd := exec.Command(goCmd, args...)
		cmd.Dir = opts.Root
		cmd.Env = buildEnvironment(os.Environ(), target)

		if opts.Verbose {
			fmt.Fprintf(os.Stderr, "release: %s %s\n", goCmd, strings.Join(args, " "))
		}
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("release: build %s: %w\n%s", target, err, output)
		}
		if err := VerifyCommitEmbedded(out, opts.Metadata); err != nil {
			return fmt.Errorf("release: build %s: %w", target, err)
		}
	}

	if err := WriteChecksums(dist, targets); err != nil {
		return err
	}

	// The artifact for the host platform is run, and its reported
	// metadata is compared with the metadata that was injected. This is
	// the conclusive check that the linker flags took effect: a binary
	// that reports the development defaults is not a release artifact.
	for _, target := range targets {
		if target.GOOS != runtime.GOOS || target.GOARCH != runtime.GOARCH {
			continue
		}
		artifact := filepath.Join(dist, ArtifactName(target))
		if err := verifyRuns(artifact, opts.Metadata); err != nil {
			return err
		}
	}
	return nil
}

// buildEnvironment returns the environment for one target build.
//
// The variables that would otherwise change the output are either set
// explicitly or removed. Removing them matters as much as setting them:
// GOEXPERIMENT, GOFIPS140, GO_LDFLAGS and the per-architecture GO386,
// GOMIPS, GOARM and friends all change the binary the toolchain
// produces, so inheriting them from the developer's environment would
// make the build depend on the shell it was run from.
func buildEnvironment(env []string, target Target) []string {
	overrides := map[string]string{
		"CGO_ENABLED": "0",
		"GOFLAGS":     "-mod=readonly",
		"GOTOOLCHAIN": "local",
		"GOOS":        target.GOOS,
		"GOARCH":      target.GOARCH,
	}
	switch target.GOARCH {
	case "amd64":
		overrides["GOAMD64"] = "v1"
	case "arm64":
		overrides["GOARM64"] = "v8.0"
	}

	// Variables that change the toolchain's output. They are removed
	// rather than overridden, because there is no safe default for
	// them: inheriting one would silently change the artifact.
	removed := map[string]bool{
		"GOEXPERIMENT": true,
		"GO_LDFLAGS":   true,
		"GOFIPS140":    true,
		"GO386":        true,
		"GOARM":        true,
		"GOMIPS":       true,
		"GOMIPS64":     true,
		"GOPPC64":      true,
		"GORISCV64":    true,
		"GOWASM":       true,
		"GO_EXTLINKED": true,
	}

	out := make([]string, 0, len(env)+len(overrides))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if _, replaced := overrides[key]; replaced {
			continue
		}
		if removed[key] {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

// WriteChecksums writes the checksum file for the given targets into dir.
//
// The file covers exactly the release artifacts: the checksum file
// itself is not listed, because a file cannot contain its own digest.
func WriteChecksums(dir string, targets []Target) error {
	entries := make([]SHA256Sum, 0, len(targets))
	for _, target := range targets {
		name := ArtifactName(target)
		hash, err := FileSHA256(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("release: checksum %s: %w", name, err)
		}
		entries = append(entries, SHA256Sum{Hash: hash, Filename: name})
	}
	path := filepath.Join(dir, SHA256SumsFile)
	if err := os.WriteFile(path, WriteSHA256Sums(entries), 0o644); err != nil {
		return fmt.Errorf("release: write %s: %w", SHA256SumsFile, err)
	}
	return nil
}

// VerifyCommitEmbedded checks that the artifact at path carries the
// expected commit.
//
// The linker silently ignores an -X that names a symbol which does not
// exist, so a typo in a variable path would produce a binary that
// quietly reports the development defaults. This check reads the commit
// back out of the artifact.
//
// The commit is the value to check, and not the version or the build
// date. The commit is forty hexadecimal characters and cannot occur in a
// binary by accident, so its presence is conclusive. A short version
// string such as "1.0.0" does occur by accident, so a byte scan of it
// would pass even when the linker flag had failed. The version and the
// build date are checked by running the binary instead, which is
// conclusive: see verifyRuns.
func VerifyCommitEmbedded(path string, want Metadata) error {
	if want.Commit == "" {
		return fmt.Errorf("release: %s: no commit to verify", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !containsBytes(data, want.Commit) {
		return fmt.Errorf("release: %s does not carry the build commit %q; "+
			"the linker ignores an -X that names a symbol which does not exist",
			filepath.Base(path), want.Commit)
	}
	return nil
}

// containsBytes reports whether the needle occurs in the haystack.
func containsBytes(haystack []byte, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(string(haystack), needle)
}
