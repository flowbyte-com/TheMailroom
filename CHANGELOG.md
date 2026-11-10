# Changelog

All notable changes to TheMailroom are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The project is licensed under the GNU Affero General Public License,
version 3. See [`LICENSE`](LICENSE).

## [1.0.0-rc.2] - 2026-11-10

The second release candidate of TheMailroom V1. It is the first candidate
that carries the whole onboarding path: the diagnostics command, the setup
command, an optional provider preset, a portable agent skill and an
automated acceptance procedure.

The version is prepared as `1.0.0-rc.2`, and the binaries built from this
tree report it in their metadata. **No RC2 release exists yet.** A future
release will be published under the `v1.0.0-rc.2` tag, and only once that
tag has been pushed with the authorisation to publish it. The release
workflow then publishes it as a prerelease and does not mark it as the
latest release.

### Added

- **`themailroom doctor`**, a diagnostic command that checks a
  configuration without starting the server. The local checks run by
  default: they parse the configuration, report the file mode, and
  describe each account's endpoint, TLS mode and credential source by kind
  only. They open no network connection and execute no credential command.
- **Opt-in network diagnostics.** `themailroom doctor -network` explicitly
  enables the connectivity checks — name resolution, TCP, TLS and
  authentication — and is the only way this diagnostic command opens a
  connection or runs a credential command. A requested network check that
  fails, times out or is cancelled makes the command exit non-zero. This
  constrains the diagnostic command only: the MCP server opens connections
  to the configured IMAP and SMTP servers whenever a request needs them.
- **`themailroom setup`**, an interactive interview that writes a
  configuration file. It records a credential command rather than a
  password, and it validates the configuration with the production loader
  before writing it.
- **Safe configuration publication.** Setup writes the configuration to a
  temporary file in the destination directory and publishes it from there.
  The publish never overwrites, and an interrupted interview writes nothing
  and leaves no temporary file.
  - On POSIX the publish is a hard link. It is atomic, it does not follow
    a symbolic link at the destination, and it fails if the destination
    already exists.
  - On a filesystem without hard links the fallback is an exclusive
    create. It also cannot overwrite, but it is not atomic.
  - An existing destination is refused unless `-force` is given.
  - A symbolic link is refused even with `-force`, because writing through
    it would modify a file the operator did not name.
  - An ordinary setup refuses a destination directory that is
    world-writable and not sticky, because any local user could replace
    the file afterwards.
  - A forced replacement renames over the destination entry, so it is
    permitted only in a directory that group and other users cannot write
    to.
  - The file is created with mode `0600` on POSIX systems, whatever the
    process umask.
  - On Windows setup refuses to write at all, because the Go standard
    library cannot set an access control list.
- **An optional Purelymail preset.** `themailroom setup -preset purelymail`
  fills in that provider's defaults during the interview. The preset is a
  set of defaults and nothing more: every value stays editable, a custom
  domain is used as given, the domain is never inspected to detect a
  provider, and the provider label is informational and does not change
  runtime behaviour.
- **A portable agent skill**, `skills/themailroom/SKILL.md`, in the
  ordinary Agent Skills format. It is framework-neutral, carries no
  dependency on HAL, MPM or any particular agent, and its documentation
  references are pinned to the `v1.0.0-rc.2` tag, so that the file works
  when it is copied out of a checkout on its own.
- **An automated onboarding acceptance**, which builds the command from the
  current commit with the release linker flags, runs the generic and preset
  interviews, validates the result, starts the server over stdio, checks
  the tool surface, exercises a read-only workflow against deterministic
  fixtures, and demonstrates the invalid-configuration, permission-denial
  and interrupted-setup cases. It needs no network and no mail account.

### Changed

- The skill's guidance on reading tool results is corrected. A tool
  execution failure sets `isError` and carries human-readable text in
  `content`; the typed business-error codes are retained in the
  server-side audit log and are not part of the client payload, and a tool
  error carries no `structuredContent`. A malformed request or an unknown
  tool name is a JSON-RPC error rather than a tool result. An agent must
  not assume a machine-readable code is available, and must not scrape the
  error text for one.
- The version inventory under 1.0.0 below now lists `read_attachment`
  among the read tools. It was present in `v1.0.0-rc.1` and was omitted
  from that list by mistake; it was not introduced after RC1.

### Security

- Setup never asks for a password and never stores one. A credential
  command is recorded instead, and it is executed only when a secret is
  actually needed.
- The local doctor opens no network connection and executes no credential
  command. The opt-in network checks do execute the account's credential
  command, because authentication needs the secret.
- Setup on Windows refuses to write, because the Go standard library
  cannot set an access control list and a new file inherits the access
  control list of its directory. It fails closed and explains how to
  create and restrict the file by hand instead of claiming a protection
  that does not exist.

### Known limitations

- **Setup refuses to write on Windows.** Automated configuration writes
  are not supported there. See the note in the 1.0.0 section below.
- **OAuth2-only providers remain unsupported.** Authentication is by
  password or by a credential command.
- **Live acceptance against a second independent IMAP and SMTP provider
  has not occurred.** The automated acceptance uses the memory backend and
  the repository's fixtures, so it proves the onboarding path and the tool
  surface rather than any real provider. A local acceptance is not
  evidence that a provider is compatible.
- **This is a release candidate, not production certification.** No RC2
  release exists yet. When it is published, the release workflow publishes
  it as a prerelease and does not mark it as the latest release.

## [1.0.0-rc.1] - 2026-11-10

The first release candidate of TheMailroom V1. It carries the
functionality described under 1.0.0 below, and it is published as a
prerelease: it is not marked as the latest release, and it is meant for
testing against a real IMAP and SMTP account rather than for production
use.

The binaries report the version `1.0.0-rc.1` in their metadata, and are
published under the `v1.0.0-rc.1` tag.

## [1.0.0] - Unreleased

The first V1 release. TheMailroom is an MCP server that exposes a mail
account to an agent over the Model Context Protocol, with bounded
workloads and an explicit security posture.

### Added

- **MCP server over stdio**, speaking the Model Context Protocol through
  the official Go SDK.
- **Read-only IMAP backend.** IMAP4rev1 over TLS, with `EXAMINE`-based
  read-only selection so a read connection cannot accept `STORE` or
  `APPEND`, and `BODY.PEEK[]` fetches so reading a message never sets
  `\Seen`.
- **Read tools.** `list_accounts`, `list_mailboxes`, `list_messages`,
  `read_message`, `read_attachment` and `search_messages`, all
  permission-checked server-side.
- **Outbound sending.** `send_message`, with an RFC 5322 composer, a
  recipient policy, a confined attachment resolver and authenticated
  SMTP submission. Reply, forward and mailbox mutation are not
  implemented.
- **Strict UIDVALIDITY handling.** A UID is only actionable together with
  the UIDVALIDITY observed in the same selection that produced it, so a
  recycled UID cannot be silently targeted.
- **Protocol-level IMAP literal-size enforcement**, so a server cannot
  make the client allocate an unbounded response.
- **Bounded MCP request framing.** One inbound frame is capped before
  JSON decoding.
- **Bounded IMAP credential commands**, so a hostile password command
  cannot stall the server indefinitely.
- **Bounded list and search workloads.** The per-account
  `limits.max_list_scan_messages` budget bounds how many messages one
  operation may fetch and process, and is reported by `list_accounts`
  so an operator can read the budget the server enforces.
- **Finite IMAP command and operation deadlines.** Every network wait has
  a finite bound; a deadline cannot be disabled.
- **An aggregate MCP response-size limit**, with UTF-8-safe truncation.
- **Release artifacts.** Reproducible cross-platform binaries for Linux,
  macOS and Windows, built with CGO disabled, with version, commit and
  build-date metadata, a `SHA256SUMS` file, and a release workflow that
  publishes only from a version tag on `main`.

### Security

- The release workflow publishes only from a `v*` tag whose commit is on
  `main` and whose version matches the `VERSION` file. There is no
  pull-request, branch-push or manual trigger, so an ordinary branch
  cannot publish a release.
- The Linux release artifacts are statically linked, so they do not
  depend on the shared libraries of the machine they run on.
- Credentials never appear in an MCP-visible structure, a log line or an
  error message.

### Known limitations

- `has_attachments` is omitted from IMAP list and search summaries: those
  paths fetch envelopes only, so attachment presence is unknown rather
  than false. `read_message` returns the parsed attachment list, which is
  authoritative.
- Reply, forward and mailbox mutation are not implemented.
- `themailroom setup` refuses to write on Windows. The Go standard library
  cannot set an access control list, and a new file inherits the access
  control list of its directory, so a configuration written there could be
  readable by other users. Setup fails closed and explains how to create
  and restrict the file by hand instead of claiming a protection that does
  not exist.
- The Purelymail live smoke test completed on 2026-10-21. It is not
  production certification.

[1.0.0-rc.2]: https://github.com/flowbyte-com/TheMailroom/releases/tag/v1.0.0-rc.2
[1.0.0-rc.1]: https://github.com/flowbyte-com/TheMailroom/releases/tag/v1.0.0-rc.1

The `1.0.0-rc.2` link will not resolve until that release is published. It
is recorded here so that the changelog and the tag agree, not to announce a
release that exists.
