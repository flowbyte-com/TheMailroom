// Command release builds, verifies and exercises a TheMailroom release
// candidate.
//
// It is the single implementation behind the Makefile targets and the
// release workflow. It never publishes anything: publishing belongs to
// the release workflow, which runs only from a version tag.
//
// Usage:
//
//	release build      [--dist DIR] [--root DIR] [--version V] [--commit SHA] [--build-date T]
//	release verify     [--dist DIR] [--root DIR] [--version V] [--commit SHA] [--build-date T]
//	release smoke      [--dist DIR] [--root DIR] [--version V] [--commit SHA] [--build-date T]
//	release version    [--root DIR] [--field FIELD]
//	release verify-tag --tag TAG --commit SHA [--root DIR] [--remote NAME]
//
// The metadata flags override the values resolved from the repository. A
// release workflow resolves the metadata once, from the tag, and passes
// the same values to every command, so that every artifact in a release
// carries identical metadata.
//
// The "version" command prints the version metadata, one field per
// line, or only the requested field. The "prerelease" field reports
// whether the version is a release candidate, which is what the release
// workflow uses to publish a candidate as a GitHub prerelease.
//
// The "verify-tag" command resolves a remote tag and checks that it
// points at the given commit. It is what binds a publication to the
// exact tag it was built from.
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
	field := fs.String("field", "", "print only this field of the version metadata")
	tag := fs.String("tag", "", "tag to resolve")
	remote := fs.String("remote", "origin", "remote to resolve the tag in")
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
		version, err := release.ReadVersionInfo(*root)
		if err != nil {
			return err
		}
		if *field != "" {
			switch *field {
			case "version":
				fmt.Println(meta.Version)
			case "commit":
				fmt.Println(meta.Commit)
			case "build-date":
				fmt.Println(meta.BuildDate)
			case "toolchain":
				fmt.Println(release.GoToolchain())
			case "prerelease":
				fmt.Println(version.IsPrerelease())
			default:
				return fmt.Errorf("unknown field %q; expected version, commit, build-date, toolchain or prerelease", *field)
			}
			return nil
		}
		fmt.Printf("%s\n%s\n%s\n%s\n",
			meta.Version, meta.Commit, meta.BuildDate, release.GoToolchain())
		return nil

	case "verify-tag":
		if *tag == "" {
			return fmt.Errorf("verify-tag needs --tag")
		}
		if *commit == "" {
			return fmt.Errorf("verify-tag needs --commit")
		}
		return release.VerifyRemoteTag(*root, *remote, *tag, *commit)

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
