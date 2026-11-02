package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VerifyOptions configures the verification of a release distribution.
type VerifyOptions struct {
	// Dist is the directory the artifacts are in.
	Dist string
	// Metadata is the metadata the artifacts must carry.
	Metadata Metadata
	// Targets are the targets to verify. Empty means Targets().
	Targets []Target
	// HostGOOS and HostGOARCH identify the machine the verification runs
	// on, so that the artifact for the host can also be executed.
	HostGOOS   string
	HostGOARCH string
}

// Verify checks the artifacts in a release distribution.
//
// The checks are, in order:
//
//  1. every expected artifact is present;
//  2. the checksum file parses and names exactly the expected artifacts,
//     so a missing or an extra artifact is an error rather than a
//     surprise;
//  3. every checksum matches the artifact it names;
//  4. every artifact is the executable format, the machine and the width
//     its target name promises, and is executable;
//  5. every Linux artifact is statically linked, so it does not depend
//     on the shared libraries of the build or the target machine;
//  6. every artifact carries the commit it was built from, so artifacts
//     from different commits cannot be mixed;
//  7. the artifact for the host, when it is part of the matrix, runs and
//     reports exactly the expected version, commit and build date;
//  8. every artifact was built with the same Go toolchain.
func Verify(opts VerifyOptions) error {
	targets := opts.Targets
	if len(targets) == 0 {
		targets = Targets()
	}

	// 1. Every expected artifact is present.
	for _, target := range targets {
		path := filepath.Join(opts.Dist, ArtifactName(target))
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("release: artifact for %s is missing: %w", target, err)
		}
	}

	// 2. The checksum file names exactly the expected artifacts.
	sumsPath := filepath.Join(opts.Dist, SHA256SumsFile)
	data, err := os.ReadFile(sumsPath)
	if err != nil {
		return fmt.Errorf("release: read %s: %w", SHA256SumsFile, err)
	}
	entries, err := ParseSHA256Sums(data)
	if err != nil {
		return err
	}
	expected := make(map[string]bool, len(targets))
	for _, target := range targets {
		expected[ArtifactName(target)] = true
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Filename] = true
	}
	for name := range expected {
		if !got[name] {
			return fmt.Errorf("release: %s does not cover %q", SHA256SumsFile, name)
		}
	}
	for name := range got {
		if !expected[name] {
			return fmt.Errorf("release: %s covers %q, which is not a release artifact", SHA256SumsFile, name)
		}
	}

	// 3. Every checksum matches.
	if err := VerifySHA256Sums(opts.Dist, entries); err != nil {
		return err
	}

	// 4, 5 and 6. Every artifact is what it claims to be, the Linux ones
	// are statically linked, and every one of them carries the commit it
	// was built from.
	for _, target := range targets {
		name := ArtifactName(target)
		path := filepath.Join(opts.Dist, name)

		info, err := InspectArtifact(path)
		if err != nil {
			return fmt.Errorf("release: %s: %w", name, err)
		}
		if want := formatFor(target); info.Format != want {
			return fmt.Errorf("release: %s is a %s artifact, want %s", name, info.Format, want)
		}
		if info.Machine != target.GOARCH {
			return fmt.Errorf("release: %s targets %s, want %s", name, info.Machine, target.GOARCH)
		}
		if want := bitsFor(target); info.Bits != want {
			return fmt.Errorf("release: %s is %d-bit, want %d-bit", name, info.Bits, want)
		}
		if target.GOOS == "linux" && !info.Static {
			return fmt.Errorf("release: %s is not statically linked; "+
				"a Linux release artifact must not depend on the shared libraries of the machine it runs on",
				name)
		}
		if err := VerifyCommitEmbedded(path, opts.Metadata); err != nil {
			return err
		}
		if err := verifyExecutable(path); err != nil {
			return err
		}
	}

	// 7. The host artifact runs and reports the expected metadata.
	if opts.HostGOOS != "" && opts.HostGOARCH != "" {
		for _, target := range targets {
			if target.GOOS != opts.HostGOOS || target.GOARCH != opts.HostGOARCH {
				continue
			}
			if err := verifyRuns(filepath.Join(opts.Dist, ArtifactName(target)), opts.Metadata); err != nil {
				return err
			}
		}
	}

	// 8. Every artifact was built with the same Go toolchain.
	paths := make([]string, 0, len(targets))
	for _, target := range targets {
		paths = append(paths, filepath.Join(opts.Dist, ArtifactName(target)))
	}
	return VerifyToolchainConsistency(paths)
}

// formatFor returns the executable container format a target must have.
func formatFor(target Target) ArtifactFormat {
	switch target.GOOS {
	case "linux":
		return FormatELF
	case "darwin":
		return FormatMachO
	case "windows":
		return FormatPE
	default:
		return ""
	}
}

// bitsFor returns the width of a target in bits.
func bitsFor(target Target) int {
	switch target.GOARCH {
	case "amd64", "arm64":
		return 64
	case "386", "arm":
		return 32
	default:
		return 0
	}
}

// verifyExecutable checks that the artifact has its executable bit set.
//
// An artifact that lost its executable bit cannot be installed, and the
// publish job downloads the artifacts through a zip round trip, so this
// is worth checking rather than discovering at the first run.
func verifyExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("release: %s is not executable", filepath.Base(path))
	}
	return nil
}

// verifyRuns runs the artifact and checks that it reports the expected
// metadata.
//
// This is the check that catches a linker flag which silently failed to
// inject the metadata: the artifact's own output is the authority.
func verifyRuns(path string, want Metadata) error {
	cmd := exec.Command(path, "-version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("release: %s did not run: %w\n%s", filepath.Base(path), err, out)
	}
	gotVersion, gotCommit, gotDate, err := parseVersionOutput(string(out))
	if err != nil {
		return fmt.Errorf("release: %s: %w", filepath.Base(path), err)
	}
	if gotVersion != want.Version {
		return fmt.Errorf("release: %s reports version %q, want %q", filepath.Base(path), gotVersion, want.Version)
	}
	if gotCommit != want.Commit {
		return fmt.Errorf("release: %s reports commit %q, want %q", filepath.Base(path), gotCommit, want.Commit)
	}
	if gotDate != want.BuildDate {
		return fmt.Errorf("release: %s reports build date %q, want %q", filepath.Base(path), gotDate, want.BuildDate)
	}
	return nil
}

// parseVersionOutput parses the output of "themailroom -version".
//
// The format is:
//
//	themailroom <version> (commit <sha>, built <timestamp>)
//
// The values are compared exactly. A substring comparison would be
// wrong: the development default is "0.1.0-dev", which contains
// "0.1.0", so a substring check would accept a binary that still
// reports the development default.
func parseVersionOutput(out string) (version, commit, buildDate string, err error) {
	text := strings.TrimSpace(out)
	open := strings.Index(text, "(")
	close := strings.LastIndex(text, ")")
	if open < 0 || close < open {
		return "", "", "", fmt.Errorf("the version output %q has no version block", out)
	}
	head := strings.Fields(strings.TrimSpace(text[:open]))
	if len(head) != 2 {
		return "", "", "", fmt.Errorf("the version output %q does not name a version", out)
	}
	version = head[1]

	body := text[open+1 : close]
	commit, ok := fieldAfter(body, "commit")
	if !ok {
		return "", "", "", fmt.Errorf("the version output %q does not name a commit", out)
	}
	buildDate, ok = fieldAfter(body, "built")
	if !ok {
		return "", "", "", fmt.Errorf("the version output %q does not name a build date", out)
	}
	return version, commit, buildDate, nil
}

// fieldAfter returns the comma-terminated word that follows the given
// label.
func fieldAfter(body, label string) (string, bool) {
	_, rest, found := strings.Cut(body, label+" ")
	if !found {
		return "", false
	}
	value, _, _ := strings.Cut(rest, ",")
	value, _, _ = strings.Cut(value, ")")
	return strings.TrimSpace(value), true
}
