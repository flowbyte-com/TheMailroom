package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// toolchainPattern matches the Go toolchain version in the output of
// "go version -m".
//
// The output begins with "<path>: go1.27.3", so the version is the last
// word of the first line.
var toolchainPattern = regexp.MustCompile(`go\d+\.\d+(\.\d+)?([a-z0-9.\-+]*)?$`)

// moduleFile is the module definition, relative to the repository root.
const moduleFile = "go.mod"

// GoToolchain returns the version of the Go toolchain that is running.
//
// It is the toolchain the artifacts are built with when the build runs
// here, and the toolchain the artifacts are verified with.
func GoToolchain() string { return runtime.Version() }

// RequiredToolchain returns the Go version the repository requires.
//
// The version comes from the "go" directive of go.mod, so the module
// file remains the single source of truth for the toolchain the project
// supports. The release workflow pins its Go version to this value, and
// the regression tests check that it does.
func RequiredToolchain(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, moduleFile))
	if err != nil {
		return "", fmt.Errorf("release: read %s: %w", moduleFile, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "go ") {
			continue
		}
		version, _, _ := strings.Cut(strings.TrimPrefix(line, "go "), " ")
		version = strings.TrimSpace(version)
		if version == "" {
			break
		}
		return version, nil
	}
	return "", fmt.Errorf("release: %s declares no Go version", moduleFile)
}

// ToolchainOf returns the Go toolchain version recorded in the build
// information of the artifact at path.
//
// The toolchain records its own version in every binary it produces, so
// the version that built an artifact is recoverable from the artifact
// itself. That is what makes the reproducibility precondition checkable
// rather than merely documented.
func ToolchainOf(path string) (string, error) {
	out, err := exec.Command("go", "version", "-m", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("release: read the build information of %s: %w\n%s", path, err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return "", fmt.Errorf("release: %s carries no build information", path)
	}
	first := lines[0]
	// The first line is "<path>: go1.27.3".
	_, version, found := strings.Cut(first, ": ")
	if !found {
		return "", fmt.Errorf("release: the build information of %s is not understood: %q", path, first)
	}
	version = strings.TrimSpace(version)
	if !toolchainPattern.MatchString(version) {
		return "", fmt.Errorf("release: the build information of %s does not name a Go toolchain: %q", path, version)
	}
	return version, nil
}

// VerifyToolchainConsistency checks that every artifact was built with
// the same Go toolchain.
//
// Two builds of the same commit with different Go versions are not
// identical: the compiler and the runtime differ between Go releases,
// and the toolchain records its version in the binary. A release whose
// artifacts were built with different toolchains is therefore not
// reproducible, and the mix would go unnoticed without this check.
func VerifyToolchainConsistency(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	first, err := ToolchainOf(paths[0])
	if err != nil {
		return err
	}
	for _, path := range paths[1:] {
		version, err := ToolchainOf(path)
		if err != nil {
			return err
		}
		if version != first {
			return fmt.Errorf("release: %s was built with %s, but %s was built with %s; "+
				"every artifact of a release must be built with the same Go toolchain",
				filepath.Base(path), version, filepath.Base(paths[0]), first)
		}
	}
	return nil
}
