// Command release builds, verifies and exercises a TheMailroom release
// candidate.
//
// It is the single implementation behind the Makefile targets and the
// release workflow. It never publishes anything: publishing belongs to
// the release workflow, which runs only from a version tag.
//
// Usage:
//
//	release build   [--dist DIR] [--root DIR] [--version V] [--commit SHA] [--build-date T]
//	release verify  [--dist DIR] [--root DIR] [--version V] [--commit SHA] [--build-date T]
//	release smoke   [--dist DIR] [--root DIR] [--version V] [--commit SHA] [--build-date T]
//	release version [--root DIR]
//
// The metadata flags override the values resolved from the repository. A
// release workflow resolves the metadata once, from the tag, and passes
// the same values to every command, so that every artifact in a release
// carries identical metadata.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/flowbyte-com/themailroom/internal/release"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "release: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: release build|verify|smoke|version [flags]")
	}
	command := args[0]
	fs := flag.NewFlagSet("release "+command, flag.ContinueOnError)
	dist := fs.String("dist", "dist", "directory holding the release artifacts")
	root := fs.String("root", ".", "repository root")
	version := fs.String("version", "", "release version override")
	commit := fs.String("commit", "", "commit SHA override")
	buildDate := fs.String("build-date", "", "build date override (RFC 3339) override")
	verbose := fs.Bool("v", false, "print each build command")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	switch command {
	case "build":
		meta, err := resolve(*root, *version, *commit, *buildDate)
		if err != nil {
			return err
		}
		return release.Build(release.BuildOptions{
			Root:     *root,
			Dist:     *dist,
			Metadata: meta,
			Verbose:  *verbose,
		})

	case "verify":
		meta, err := resolve(*root, *version, *commit, *buildDate)
		if err != nil {
			return err
		}
		return release.Verify(release.VerifyOptions{
			Dist:       *dist,
			Metadata:   meta,
			HostGOOS:   runtime.GOOS,
			HostGOARCH: runtime.GOARCH,
		})

	case "smoke":
		meta, err := resolve(*root, *version, *commit, *buildDate)
		if err != nil {
			return err
		}
		mailboxRoot := ""
		if root := *root; root != "" {
			mailboxRoot = filepath.Join(root, "testdata", "messages")
			if _, err := os.Stat(mailboxRoot); err != nil {
				mailboxRoot = ""
			}
		}
		host := release.ArtifactName(release.Target{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH})
		binary := filepath.Join(*dist, host)
		if _, err := os.Stat(binary); err != nil {
			return fmt.Errorf("%s is missing; run \"make release\" first: %w", binary, err)
		}
		return release.SmokeInstall(release.SmokeOptions{
			Binary:      binary,
			Metadata:    meta,
			MailboxRoot: mailboxRoot,
		})

	case "version":
		meta, err := release.ResolveMetadata(*root)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n%s\n%s\n%s\n",
			meta.Version, meta.Commit, meta.BuildDate, release.GoToolchain())
		return nil

	default:
		return fmt.Errorf("unknown command %q; expected build, verify, smoke or version", command)
	}
}

// resolve determines the build metadata, applying the overrides.
func resolve(root, version, commit, buildDate string) (release.Metadata, error) {
	meta, err := release.ResolveMetadata(root)
	if err != nil {
		return release.Metadata{}, err
	}
	if version != "" {
		meta.Version = version
	}
	if commit != "" {
		meta.Commit = commit
	}
	if buildDate != "" {
		meta.BuildDate = buildDate
	}
	if err := meta.Validate(); err != nil {
		return release.Metadata{}, err
	}
	return meta, nil
}
