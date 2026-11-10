# Installing TheMailroom

TheMailroom is a single static binary that speaks the Model Context
Protocol over stdio. This document covers installing a released binary,
verifying it, and building one from source.

The configuration itself is documented separately: see
[`README.md`](../README.md#configure) and
[`docs/ARCHITECTURE.md`](ARCHITECTURE.md#29-configuration-internalconfig).

## Licence

TheMailroom is licensed under the **GNU Affero General Public License,
version 3**. The licence text ships with every release as `LICENSE`.

The AGPL requires that the corresponding source code be offered to every
recipient of a binary. Every GitHub release of this project includes the
source code archives that GitHub generates automatically for a tag, so
the source for any released binary is available from the same release
page.

## Install a released binary

Releases are published as plain binaries, one per platform, together with
a `SHA256SUMS` file. There is no installer and no package manager
integration: the binary is copied into place.

| Platform | Artifact |
|---|---|
| Linux x86-64 | `themailroom-linux-amd64` |
| Linux ARM64 | `themailroom-linux-arm64` |
| macOS x86-64 | `themailroom-darwin-amd64` |
| macOS ARM64 | `themailroom-darwin-arm64` |
| Windows x86-64 | `themailroom-windows-amd64.exe` |

The Linux artifacts are statically linked, so they run on any Linux
distribution without a matching C library and without any other shared
dependency.

### 1. Verify the download

Verify the checksum before running the binary. The `SHA256SUMS` file uses
the format produced by `sha256sum(1)` and `shasum(1)`.

On Linux:

```sh
sha256sum --check SHA256SUMS
```

On macOS:

```sh
shasum -a 256 --check SHA256SUMS
```

Each line must report `OK`. A line that reports `FAILED` means the
download is damaged or has been tampered with: do not run the binary.

To check a single file, compare it against the digest in the file:

```sh
grep 'themailroom-linux-amd64$' SHA256SUMS
sha256sum themailroom-linux-amd64
```

The two digests must be identical.

### 2. Install the binary

Copy the binary to a directory on your `PATH`. `/usr/local/bin` is the
conventional choice on Linux and macOS.

```sh
sudo install -m 0755 themailroom-linux-amd64 /usr/local/bin/themailroom
```

On Windows, copy `themailroom-windows-amd64.exe` to a directory on your
`PATH` and rename it `themailroom.exe` if you prefer.

Installing into a directory other than `/usr/local/bin` is fine: the
server has no installation-time state and no runtime dependencies beyond
the standard library.

### 3. Check the installation

```sh
themailroom -version
```

The output names the version, the commit the binary was built from and
the build date:

```
themailroom 1.0.0 (commit 904384a32183ffaad34a1d95432344eeaa920234, built 2026-11-02T12:22:50Z)
```

That output is the authoritative statement of which binary you are
running. Quote it in a bug report.

### 4. Create a configuration

`themailroom setup` writes a configuration through a short interview. It
never asks for a plaintext password; it records a credential command
instead.

```sh
themailroom setup -config /etc/themailroom/themailroom.yaml
```

The account is created read-only with verified TLS. Sending is opt-in, and
attachment sending is never granted. An existing file is not replaced unless
you pass `-force`, a symbolic link is always refused, and the file is
validated with the server's own configuration loader before it is written.

On POSIX systems the file is created with mode `0600`. `-force` is only
allowed in a directory that group and other users cannot write to, because
a forced replacement renames over the destination entry.

On Windows the standard library cannot set an access control list, and a new
file inherits its directory's ACL, so `setup` cannot guarantee that the file
is readable only by its owner. It refuses to write and tells you how to
create the file by hand and restrict it with `icacls`.

Validate the result, then test connectivity as a separate step:

```sh
themailroom doctor -config /etc/themailroom/themailroom.yaml
themailroom doctor -config /etc/themailroom/themailroom.yaml -network
```

A configuration can also be written by hand; see
[`ARCHITECTURE.md`](ARCHITECTURE.md#29-configuration-internalconfig) for the
schema and [`examples/themailroom.yaml`](../examples/themailroom.yaml) for a
complete example.

### 5. Run the server

TheMailroom is an MCP server, not an interactive program. It reads its
configuration, registers its tools and then serves requests on stdin and
stdout.

```sh
themailroom -config /etc/themailroom/themailroom.yaml
```

With no `-config` flag, the server looks for `themailroom.yaml`,
`themailroom.yml`, `config/themailroom.yaml` and
`/etc/themailroom/themailroom.yaml`, in that order, and uses the first
one that exists.

### Uninstalling

Remove the binary. TheMailroom writes no state outside its configuration
file and the directories it is told to read:

```sh
sudo rm /usr/local/bin/themailroom
```

## Build from source

Building from source requires Go 1.27.1 or later. No C compiler is
needed: the build sets `CGO_ENABLED=0`.

```sh
git clone https://github.com/flowbyte-com/TheMailroom.git
cd TheMailroom
make build
./dist/themailroom -version
```

To build every release artifact and verify it:

```sh
make release
make verify
make smoke
```

`make release` writes the binaries and a `SHA256SUMS` file to `dist/`.
`make verify` checks the checksums, the executable format of each
artifact, the machine it targets, the static linkage of the Linux
artifacts, the commit every artifact carries and the Go toolchain they
were all built with; it also runs the artifact for the host platform and
compares the version, the commit and the build date it reports with the
metadata the release was built from. `make smoke` installs the artifact
for the host platform into a clean temporary directory and exercises it:
it checks the reported version, completes an MCP handshake and confirms
the server exits cleanly.

The build is reproducible: the same commit, built with the same Go
toolchain, produces byte-for-byte identical artifacts on any machine. See
[`docs/RELEASE.md`](RELEASE.md) for how that is achieved and how it is
checked.

### Build requirements

| Requirement | Why |
|---|---|
| Go 1.27.1 or later | the version the module declares |
| `git` | the commit and the commit timestamp are part of the build metadata |

The build does not need a C compiler, a container runtime or any other
toolchain.
