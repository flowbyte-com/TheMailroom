package release

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed semantic version.
//
// The release workflow distinguishes a stable version from a release
// candidate, because a candidate must be published as a GitHub
// prerelease and must not be marked as the latest release.
type Version struct {
	// Major is the major version.
	Major int
	// Minor is the minor version.
	Minor int
	// Patch is the patch version.
	Patch int
	// Prerelease is the prerelease identifier without its leading "-".
	// It is empty for a stable version.
	Prerelease string
	// Build is the build metadata without its leading "+". It is empty
	// when the version carries none.
	Build string
}

// IsPrerelease reports whether the version carries a prerelease
// identifier.
//
// A version such as "1.0.0-rc.1" is a prerelease; "1.0.0" is not.
func (v Version) IsPrerelease() bool { return v.Prerelease != "" }

// String renders the version in its canonical form.
func (v Version) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		b.WriteString("-")
		b.WriteString(v.Prerelease)
	}
	if v.Build != "" {
		b.WriteString("+")
		b.WriteString(v.Build)
	}
	return b.String()
}

// ParseVersion parses a semantic version.
//
// The accepted grammar is the one the Semantic Versioning specification
// defines:
//
//	MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD]
//
// The numeric identifiers must not carry leading zeros, and a prerelease
// identifier that is entirely numeric must not carry one either. The
// build metadata may carry leading zeros, because it is not compared.
//
// A leading "v" is rejected. The tag carries the "v" and the version
// does not, so the two are never confused for one another.
//
// Parsing is strict on purpose: a version that this function rejects
// cannot be published, and a version it accepts is one the release
// workflow can reason about.
func ParseVersion(s string) (Version, error) {
	var v Version
	if s == "" {
		return v, fmt.Errorf("release: the version is empty")
	}
	if strings.HasPrefix(s, "v") {
		return v, fmt.Errorf("release: the version %q must not start with %q", s, "v")
	}

	// The build metadata comes last, so it is split off first.
	rest := s
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		v.Build = rest[i+1:]
		rest = rest[:i]
		if err := validateIdentifiers(v.Build, false, "build metadata"); err != nil {
			return Version{}, fmt.Errorf("release: the version %q is not valid: %w", s, err)
		}
	}

	// Then the prerelease identifier.
	if i := strings.IndexByte(rest, '-'); i >= 0 {
		v.Prerelease = rest[i+1:]
		rest = rest[:i]
		if err := validateIdentifiers(v.Prerelease, true, "prerelease"); err != nil {
			return Version{}, fmt.Errorf("release: the version %q is not valid: %w", s, err)
		}
	}

	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("release: the version %q must have three numeric parts", s)
	}
	numbers := make([]int, 3)
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return Version{}, fmt.Errorf("release: the version %q has the non-numeric part %q", s, part)
		}
		if !isNumericIdentifier(part) {
			return Version{}, fmt.Errorf("release: the version %q has the malformed numeric part %q", s, part)
		}
		numbers[i] = n
	}
	v.Major, v.Minor, v.Patch = numbers[0], numbers[1], numbers[2]
	return v, nil
}

// isNumericIdentifier reports whether the identifier is a decimal number
// without a leading zero.
func isNumericIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	if len(s) > 1 && s[0] == '0' {
		return false
	}
	return true
}

// validateIdentifiers checks a dot-separated list of identifiers.
//
// An identifier is a non-empty run of alphanumeric characters and
// hyphens. When numericNoLeadingZeros is set, an identifier that is
// entirely numeric must not carry a leading zero, which is the rule the
// specification states for prerelease identifiers.
func validateIdentifiers(s string, numericNoLeadingZeros bool, what string) error {
	if s == "" {
		return fmt.Errorf("the %s is empty", what)
	}
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return fmt.Errorf("the %s has an empty identifier", what)
		}
		for _, r := range id {
			switch {
			case r >= '0' && r <= '9':
			case r >= 'a' && r <= 'z':
			case r >= 'A' && r <= 'Z':
			case r == '-':
			default:
				return fmt.Errorf("the %s has the invalid identifier %q", what, id)
			}
		}
		if numericNoLeadingZeros && isAllDigits(id) && len(id) > 1 && id[0] == '0' {
			return fmt.Errorf("the %s has the numeric identifier %q with a leading zero", what, id)
		}
	}
	return nil
}

// isAllDigits reports whether the string is a non-empty run of decimal
// digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
