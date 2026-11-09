# Changelog

All notable changes to TheMailroom are recorded here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The project is licensed under the GNU Affero General Public License,
version 3. See [`LICENSE`](LICENSE).

## [1.0.0-rc.1] - 2026-11-09

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
  `read_message` and `search_messages`, all permission-checked
  server-side.
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
- The Purelymail live smoke test completed on 2026-10-21. It is not
  production certification.

[1.0.0-rc.1]: https://github.com/flowbyte-com/TheMailroom/releases/tag/v1.0.0-rc.1
