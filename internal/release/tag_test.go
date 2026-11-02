package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseTagReference covers the listings "git ls-remote" produces.
//
// The two shapes are an annotated tag, which has a tag object and a
// peeled commit, and a lightweight tag, which points directly at its
// commit.
func TestParseTagReference(t *testing.T) {
	const (
		object  = "fd7cf6b60ca4ee77357a351c49c7e1764ba0f3d1"
		commit  = "bf851288d3f0769fa433377dee4d77e2f032af5a"
		other   = "1111111111111111111111111111111111111111"
		tagName = "v1.0.0"
	)

	t.Run("annotated", func(t *testing.T) {
		out := object + "\trefs/tags/" + tagName + "\n" +
			commit + "\trefs/tags/" + tagName + "^{}\n"
		ref, err := ParseTagReference(out, tagName)
		if err != nil {
			t.Fatalf("ParseTagReference: %v", err)
		}
		if !ref.Annotated {
			t.Error("Annotated = false, want true")
		}
		if ref.ObjectSHA != object {
			t.Errorf("ObjectSHA = %q, want %q", ref.ObjectSHA, object)
		}
		if ref.CommitSHA != commit {
			t.Errorf("CommitSHA = %q, want %q", ref.CommitSHA, commit)
		}
	})

	t.Run("lightweight", func(t *testing.T) {
		out := commit + "\trefs/tags/" + tagName + "\n"
		ref, err := ParseTagReference(out, tagName)
		if err != nil {
			t.Fatalf("ParseTagReference: %v", err)
		}
		if ref.Annotated {
			t.Error("Annotated = true, want false")
		}
		if ref.CommitSHA != commit {
			t.Errorf("CommitSHA = %q, want %q", ref.CommitSHA, commit)
		}
	})

	t.Run("absent", func(t *testing.T) {
		if _, err := ParseTagReference("", tagName); err == nil {
			t.Error("ParseTagReference() with an empty listing = nil, want an error")
		}
		if _, err := ParseTagReference(other+"\trefs/tags/other\n", tagName); err == nil {
			t.Error("ParseTagReference() without the tag = nil, want an error")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		for _, out := range []string{
			"not a listing\n",
			commit + " refs/tags/" + tagName + "\n",
			"abc\trefs/tags/" + tagName + "\n",
		} {
			if _, err := ParseTagReference(out, tagName); err == nil {
				t.Errorf("ParseTagReference(%q) = nil, want an error", out)
			}
		}
	})
}

// TestVerifyRemoteTag checks the publication guard against a real
// repository.
//
// The guard is what binds a publication to the exact tag it was built
// from: a tag that has been moved, that has been deleted, or that never
// existed must be refused.
func TestVerifyRemoteTag(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available")
	}

	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.test",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("commit", "-qm", "first")
	first := run("rev-parse", "HEAD")

	// A lightweight tag points directly at its commit.
	run("tag", "lightweight-v1")
	// An annotated tag points at a tag object, which points at a commit.
	run("tag", "-a", "annotated-v1", "-m", "annotated")
	annotated := run("rev-parse", "annotated-v1^{}")

	// A second commit, so that a moved tag has somewhere to move to.
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("commit", "-qm", "second")
	second := run("rev-parse", "HEAD")

	t.Run("a lightweight tag resolves to its commit", func(t *testing.T) {
		if err := VerifyRemoteTag(repo, ".", "lightweight-v1", first); err != nil {
			t.Errorf("VerifyRemoteTag() = %v, want nil", err)
		}
	})

	t.Run("an annotated tag resolves to its commit", func(t *testing.T) {
		if err := VerifyRemoteTag(repo, ".", "annotated-v1", annotated); err != nil {
			t.Errorf("VerifyRemoteTag() = %v, want nil", err)
		}
	})

	t.Run("an absent tag is refused", func(t *testing.T) {
		err := VerifyRemoteTag(repo, ".", "v-does-not-exist", first)
		if err == nil {
			t.Fatal("VerifyRemoteTag() with an absent tag = nil, want an error")
		}
		if !strings.Contains(err.Error(), "does not exist") {
			t.Errorf("VerifyRemoteTag() = %v, want it to report that the tag does not exist", err)
		}
	})

	t.Run("a tag that resolves to another commit is refused", func(t *testing.T) {
		err := VerifyRemoteTag(repo, ".", "lightweight-v1", second)
		if err == nil {
			t.Fatal("VerifyRemoteTag() with a mismatched commit = nil, want an error")
		}
		if !strings.Contains(err.Error(), "must not move") {
			t.Errorf("VerifyRemoteTag() = %v, want it to report the mismatch", err)
		}
	})

	t.Run("a moved tag is refused", func(t *testing.T) {
		run("tag", "-f", "lightweight-v1", second)
		err := VerifyRemoteTag(repo, ".", "lightweight-v1", first)
		if err == nil {
			t.Fatal("VerifyRemoteTag() with a moved tag = nil, want an error")
		}
	})
}

// TestVerifyRemoteTagRequiresACommit covers the guard's own precondition.
func TestVerifyRemoteTagRequiresACommit(t *testing.T) {
	if err := VerifyRemoteTag(t.TempDir(), ".", "v1.0.0", ""); err == nil {
		t.Error("VerifyRemoteTag() with no commit = nil, want an error")
	}
}
