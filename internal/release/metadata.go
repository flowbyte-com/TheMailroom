package release

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VersionFile is the name of the file at the repository root that holds the
// release version.
//
// The version is a reviewable fact in the source tree rather than a
// convention: the release workflow refuses to publish a tag whose version
// does not match this file, and the changelog is checked against it.
const VersionFile = "VERSION"

// ResolveMetadata determines the build metadata from the repository at
// root.
//
// The version comes from the VERSION file. The commit is the full SHA of
// HEAD. The build date is the commit timestamp rather than the wall clock,
// so the metadata is a function of the commit and rebuilding the same
// commit reproduces the same binary. SOURCE_DATE_EPOCH overrides the
// commit timestamp for a build that must match a foreign reference build.
//
// The overrides let the caller pin the metadata, which is what a release
// workflow does: it resolves the metadata once, from the tag, and passes
// the same values to every target.
func ResolveMetadata(root string) (Metadata, error) {
	version, err := ReadVersion(root)
	if err != nil {
		return Metadata{}, err
	}
	commit, err := gitOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return Metadata{}, fmt.Errorf("release: resolve commit: %w", err)
	}
	date, err := resolveBuildDate(root)
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{
		Version:   version,
		Commit:    strings.TrimSpace(commit),
		BuildDate: date,
	}, nil
}

// ReadVersion reads the release version from the VERSION file at root.
//
// The version must be a valid semantic version. A release candidate
// such as "1.0.0-rc.1" is valid and is published as a prerelease; a
// malformed version is refused, because a version the release workflow
// cannot reason about must not reach a release.
func ReadVersion(root string) (string, error) {
	path := filepath.Join(root, VersionFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("release: read %s: %w", VersionFile, err)
	}
	version := strings.TrimSpace(string(data))
	if version == "" {
		return "", fmt.Errorf("release: %s is empty", VersionFile)
	}
	parsed, err := ParseVersion(version)
	if err != nil {
		return "", fmt.Errorf("release: %s: %w", VersionFile, err)
	}
	return parsed.String(), nil
}

// ReadVersionInfo reads the release version from the VERSION file at
// root and returns it parsed.
//
// The caller needs the parsed form when it has to know whether the
// version is a release candidate.
func ReadVersionInfo(root string) (Version, error) {
	version, err := ReadVersion(root)
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(version)
}

// resolveBuildDate determines the build timestamp.
//
// SOURCE_DATE_EPOCH is the reproducible-builds convention and wins when it
// is set. Otherwise the commit timestamp is used.
//
// The timestamp is normalised to UTC and rendered with a "Z" suffix, so
// that the same commit always yields the same string regardless of the
// time zone of the machine that ran the build.
func resolveBuildDate(root string) (string, error) {
	if epoch := strings.TrimSpace(os.Getenv("SOURCE_DATE_EPOCH")); epoch != "" {
		secs, err := strconv.ParseInt(epoch, 10, 64)
		if err != nil {
			return "", fmt.Errorf("release: SOURCE_DATE_EPOCH %q is not an integer: %w", epoch, err)
		}
		return time.Unix(secs, 0).UTC().Format(time.RFC3339), nil
	}
	stamp, err := gitOutput(root, "log", "-1", "--format=%ct")
	if err != nil {
		return "", fmt.Errorf("release: resolve commit timestamp: %w", err)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(stamp), 10, 64)
	if err != nil {
		return "", fmt.Errorf("release: the commit timestamp %q is not an integer: %w", strings.TrimSpace(stamp), err)
	}
	return time.Unix(secs, 0).UTC().Format(time.RFC3339), nil
}

// gitOutput runs a git command in dir and returns its standard output.
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		if stderr != "" {
			return "", fmt.Errorf("%w: %s", err, stderr)
		}
		return "", err
	}
	return string(out), nil
}
