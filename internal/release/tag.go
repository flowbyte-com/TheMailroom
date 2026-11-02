package release

import (
	"fmt"
	"os/exec"
	"strings"
)

// TagReference describes a remote tag resolved with "git ls-remote".
type TagReference struct {
	// ObjectSHA is the SHA of the tag object for an annotated tag, or of
	// the commit for a lightweight tag.
	ObjectSHA string
	// CommitSHA is the SHA of the commit the tag resolves to.
	CommitSHA string
	// Annotated reports whether the tag is an annotated tag object rather
	// than a lightweight reference to a commit.
	Annotated bool
}

// ParseTagReference parses the output of "git ls-remote" for a tag.
//
// The output has one line per matching ref, each a hexadecimal SHA, a
// tab, and the ref name:
//
//	<sha>	refs/tags/v1.0.0
//	<sha>	refs/tags/v1.0.0^{}
//
// The first line is the tag object for an annotated tag, or the commit
// itself for a lightweight tag. The second line, which git emits only
// for an annotated tag, is the commit the tag object peels to.
//
// Both forms therefore resolve to a commit, and the difference between
// them is recorded rather than assumed away. A tag that is absent from
// the output is an error: a publication step that cannot resolve its
// tag must not proceed.
func ParseTagReference(output, tag string) (TagReference, error) {
	var ref TagReference
	plain := "refs/tags/" + tag
	peeled := plain + "^{}"

	var sawPlain, sawPeeled bool
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sha, name, found := strings.Cut(line, "\t")
		if !found {
			return TagReference{}, fmt.Errorf("release: the remote listing %q is not understood", line)
		}
		sha = strings.TrimSpace(sha)
		if len(sha) != 40 {
			return TagReference{}, fmt.Errorf("release: the remote listing %q does not name a commit", line)
		}
		switch name {
		case plain:
			ref.ObjectSHA = sha
			sawPlain = true
		case peeled:
			ref.CommitSHA = sha
			sawPeeled = true
		}
	}

	if !sawPlain {
		return TagReference{}, fmt.Errorf("release: the remote tag %q does not exist", tag)
	}
	if sawPeeled {
		// An annotated tag points at a tag object, which points at a
		// commit. The peeled line is the commit.
		ref.Annotated = true
		if ref.CommitSHA == "" {
			return TagReference{}, fmt.Errorf("release: the remote tag %q does not resolve to a commit", tag)
		}
		return ref, nil
	}
	// A lightweight tag points directly at its commit.
	ref.CommitSHA = ref.ObjectSHA
	return ref, nil
}

// VerifyRemoteTag resolves the remote tag and checks that it points at
// the given commit.
//
// The remote is consulted rather than the local repository, because the
// question is what the remote tag resolves to at the moment of
// publication. A tag that has been moved, or that has been deleted, is
// refused.
func VerifyRemoteTag(root, remote, tag, commit string) error {
	if remote == "" {
		remote = "origin"
	}
	if commit == "" {
		return fmt.Errorf("release: no commit to check the tag against")
	}
	cmd := exec.Command("git", "ls-remote", remote, "refs/tags/"+tag, "refs/tags/"+tag+"^{}")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(exitErr.Stderr))
		}
		if stderr != "" {
			return fmt.Errorf("release: resolve the remote tag %q: %w: %s", tag, err, stderr)
		}
		return fmt.Errorf("release: resolve the remote tag %q: %w", tag, err)
	}

	ref, err := ParseTagReference(string(out), tag)
	if err != nil {
		return err
	}
	if !strings.EqualFold(ref.CommitSHA, commit) {
		return fmt.Errorf("release: the remote tag %q resolves to %s, but this workflow built %s; "+
			"the tag must not move between the build and the publication",
			tag, ref.CommitSHA, commit)
	}
	return nil
}
