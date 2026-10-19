# TheMailroom Architecture

> Status: Phase 1 (Foundation). This document describes the design as implemented in Phase 1 and the planned layering for Phases 2 and 3.

## 1. Goals and non-goals

TheMailroom is an MCP (Model Context Protocol) server that exposes a coherent set of email capabilities to AI agents over a provider-agnostic IMAP/SMTP implementation.

Goals:

- Provider independence through standard protocols (IMAP4rev1, SMTP, MIME).
- Strong security defaults: read-only by default, explicit permissions, no credential leakage, filesystem confinement.
- Testability: every component has a contract, every protocol backend can be mocked.
- Composability with any MCP client (Claude Desktop, MCP Inspector, custom agent harnesses).

Non-goals (explicit):

- Provider administration (no account creation, DNS, alias admin).
- Web/HTTP transport in v1 (stdio only).
- Vendor-specific extensions except where a protocol gap genuinely requires it.
- Holding long-lived outbound message queues.

## 2. Layered design

The module is split into focused internal packages. Each layer depends only on the layers below it.

```
+---------------------------------------------------------+
|  cmd/themailroom          (main: config + run server)   |
+---------------------------------------------------------+
|  internal/mcpserver       (stdio transport, tool reg.)  |
|  internal/tools           (tool definitions + handlers) |
+---------------------------------------------------------+
|  internal/mailbox         (Backend interface)           |
|  internal/mailbox/memory  (in-memory Backend)           |
|  internal/imapclient      (IMAP Backend — read slice)   |
|  internal/smtpclient      (SMTP submission — future)    |
|  internal/mailmime        (MIME parse/compose — future) |
|  internal/attachments     (os.Root confinement)         |
+---------------------------------------------------------+
|  internal/accounts        (Account registry, conns.)    |
|  internal/permissions     (caps, enforcement)           |
|  internal/config          (load + validate config)      |
+---------------------------------------------------------+
|  internal/version         (build metadata)              |
+---------------------------------------------------------+
```

### 2.1 Transport and tool registration (`internal/mcpserver`)

Wraps the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`). The server registers tools in one place. Each tool advertises a JSON Schema for its parameters and a typed Go handler. Stdio is the only transport in v1.

The MCP layer never sees credentials. All inputs are validated against the tool schema, then routed through the `permissions` and `accounts` layers.

### 2.2 Tool definitions (`internal/tools`)

Tools are the public surface. They are grouped by capability:

- Read-only (Phase 1):
  - `list_accounts`
  - `list_mailboxes`
  - `list_messages`
  - `read_message`
  - `search_messages`
- Read attachments (Phase 2): `read_attachment`
- Sending (Phase 2): `send_message`, `reply_message`, `forward_message`
- Modification (Phase 2): `move_message`, `flag_message`, `archive_message`, `delete_message`

Tools return structured payloads (JSON) and never include credentials or arbitrary raw protocol output. Errors are typed (`PermissionDenied`, `AccountNotFound`, `InvalidInput`, `BackendError`, `RateLimited`, etc.) and serialised without leaking internal details.

### 2.3 Mailbox abstraction (`internal/mailbox`)

A `Backend` interface defines the operations needed by tools:

```go
type Backend interface {
    Mailboxes(ctx context.Context, account AccountRef) ([]Mailbox, error)
    ListMessages(ctx context.Context, account AccountRef, mailbox string, opts ListOptions) ([]MessageSummary, error)
    FetchMessage(ctx context.Context, account AccountRef, mailbox string, uid uint32) (*Message, error)
    SearchMessages(ctx context.Context, account AccountRef, mailbox string, q SearchQuery) ([]MessageSummary, error)
}
```

Three implementations exist or are planned:

1. `internal/mailbox/memory` — loads `.eml` files from a directory. Used
   for development and tests. Rejects symlink entries.
2. `internal/imapclient` — the production IMAP4rev1 implementation.
   **Read-only** in this slice. Sends, modifications and deletions land
   in subsequent slices.
3. SMTP and an `os.Root`-confined attachment source — future work.

A new test fixture in `internal/imapclient` runs the production client
against an in-process go-imap server over TLS, exercising the full
real-protocol path. The end-to-end test in `internal/tools` exercises
the same backend through the MCP tool layer.

### 2.4 IMAP and SMTP (`internal/imapclient`, `internal/smtpclient`)

**Status (this slice):** `internal/imapclient` ships the read-only
IMAP4rev1 backend. It uses a narrow fork of
`github.com/emersion/go-imap` v1.2.1 —
`github.com/flowbyte-com/go-imap` at module version
`v1.2.1-themailroom.2` — and supports:

- Implicit TLS on 993 and STARTTLS on 143.
- `LIST *` for mailboxes, with sorted output.
- `UID SEARCH` and `UID FETCH` for listings and searches.
- `UID FETCH BODY.PEEK[]` for full message bodies (never sets
  `\Seen`).
- `EXAMINE` (read-only `SELECT`) for every read; the server-side
  state cannot accept `STORE` or `APPEND` through a read
  connection.
- Live UIDVALIDITY capture on every EXAMINE; the value is returned
  in `list_messages` / `search_messages` results and is mandatory
  on `read_message`. A stale or zero UIDVALIDITY is rejected before
  any body is read.
- **Protocol-level IMAP literal-size enforcement.** The backend
  sets the fork's `client.DialOptions.MaxLiteralSize` to the
  per-account `max_message_bytes`. The fork's `ReadLiteral`
  rejects an over-cap announcement *before* it allocates the
  literal buffer, so a server cannot cause an unbounded
  allocation by announcing a huge literal. The cap is applied to
  the decrypted stream, so it behaves identically under
  implicit TLS and STARTTLS. See `docs/PHASE2_DESIGN.md` §8 and
  §10 for the analysis, the fork's change set, and the route
  back to upstream.
- Single shared connection per account under a mutex.
- Automatic reconnect on connection drop or server error; a
  reconnect re-EXAMINEs and re-validates UIDVALIDITY. Mutating
  operations are never silently retried.
- Refusal to dial without TLS or STARTTLS — plain IMAP is never
  accepted.

SMTP submission and MIME composition are deliberately not in this
slice. They are documented in `docs/PHASE2_DESIGN.md` and will land in
subsequent PRs.

### 2.5 MIME (`internal/mimeparse`)

Wrapping `github.com/emersion/go-message` (parser + composer).
`internal/mimeparse` is the read-only parser that both backends use
so memory and IMAP expose the same structured view. The composer
lands with `send_message` in Phase 2.2. The parser:

- Decodes text/* and html/* bodies, applying the declared
  Content-Transfer-Encoding and charset (RFC 2047) automatically.
- Walks multipart/alternative, multipart/mixed, multipart/related
  and any combination of nested multipart containers.
- Captures attachment metadata (filename, mime type, size, part
  path, Content-ID, disposition) without materialising the
  attachment body in the wire response.
- Enforces per-message, per-attachment, total-attachment, nesting
  and part-count limits before allocating large buffers.

### 2.6 Attachments (`internal/attachments`)

The single most security-sensitive path for outbound email. Uses Go 1.24+ `os.Root` for filesystem confinement:

- `SendDir` per account restricts lookups to a directory tree.
- Symlink resolution is checked before any read.
- Path canonicalisation prevents `..` escape.
- Files larger than the configured limit are rejected.
- Per-account `attachment_send_paths` allow-list is enforced regardless of agent input.

### 2.7 Accounts (`internal/accounts`)

A registry of configured accounts. Each entry holds:

- `id` — opaque, stable identifier exposed to tools.
- Provider, IMAP/SMTP endpoints, authentication method.
- Permissions, limits, attachment paths.

Connections are created lazily and pooled per-request (one IMAP connection per active operation in Phase 2; pooling later). Credentials are never copied into MCP-visible data structures.

### 2.8 Permissions (`internal/permissions`)

A small, explicit capability set:

- `read` — list/read/search messages and attachments.
- `send` — compose/send/reply/forward.
- `modify` — flag, move, archive (without deletion).
- `delete` — explicit deletion.
- `attachment_send` — sending requires this capability *and* a path attachment to `attachment_send_paths`.

Each tool handler calls `permissions.Check(account, capability)` first. The check returns `PermissionDenied` if missing, and the tool refuses the operation. The check is server-side; we do not trust the MCP client.

### 2.9 Configuration (`internal/config`)

Loads YAML. The loader is **strict**: unknown fields and duplicate
keys are rejected at parse time, before validation. This prevents
silent typos (e.g. `permisions` instead of `permissions`) and
silent overrides that would otherwise mask a misconfiguration.

Validation covers:

- Account IDs unique.
- IMAP/SMTP endpoints sane (host required, port in 1..65535, no whitespace).
- IMAP/SMTP credentials: exactly one of `password` or `password_command`
  must be set.
- Limits are non-negative.
- Permission names are known capability names.
- `attachment_send_paths` are absolute paths without NUL bytes.

Password fields in `IMAPConfig` and `SMTPConfig` are tagged `json:"-"`
so that an accidental JSON serialisation never leaks the resolved
credential. The startup log uses `Config.Redacted()` to print a
sanitised view.

Two credential sources are supported:

1. `password` field (warning: file-based secret).
2. `password_command` field — external command whose stdout is the
   password. This is the recommended mechanism using a system keyring
   (e.g. `pass`, `secret-tool`).

The legacy `insecure_skip_verify` field has been **removed** from
production configuration. There is no way to disable TLS certificate
verification through the configuration file. Tests against a
self-signed server must use the system trust store (e.g. install the
CA) or a `SSL_CERT_FILE` override at the host level.

## 3. Dependency choices

| Concern              | Choice                                          | Rationale                                                       |
|----------------------|-------------------------------------------------|-----------------------------------------------------------------|
| MCP transport        | `github.com/modelcontextprotocol/go-sdk`       | Official Go SDK; stdio transport; schema-validated tools.       |
| YAML config          | `gopkg.in/yaml.v3`                              | Mature, widely used; supports strict unknown-key rejection.    |
| IMAP (Phase 2)       | `github.com/flowbyte-com/go-imap` (fork of `github.com/emersion/go-imap` v1.2.1) | De facto standard; protocol-correct; stable API. The fork adds protocol-level IMAP literal-size enforcement; see `docs/PHASE2_DESIGN.md` §8/§10. |
| SMTP (Phase 2)       | `github.com/emersion/go-smtp`                  | Pair with go-imap; covers submission, STARTTLS, AUTH.           |
| MIME (Phase 2)       | `github.com/emersion/go-message`                | Parser + composer; charset-aware; handles RFC 2047.             |
| Logging              | `log/slog` (stdlib)                             | Go 1.21+; structured logging; no extra dep.                    |
| Filesystem confine   | `os.Root` (stdlib, Go 1.24+)                    | OS-level chroot-style; prevents escape by symlink.             |
| Testing              | stdlib `testing` + emersion server packages     | Stub-friendly; integration via local in-process servers later.  |

Avoided-over plugins:

- No plugin framework, no Lua, no dynamic loading.
- No general-purpose email "client" framework; we own the protocol calls.
- No HTTP framework — stdio only.

### 3.1 Why go-imap v1 (not v2)

`github.com/emersion/go-imap` v1 (current `v1.2.1`) is the stable,
production-tracked line; it has been used in many production systems
for years and exposes the full IMAP4rev1 surface required for
mailbox reading. v2 (`github.com/emersion/go-imap/v2`) is a parallel
re-design that is not yet marked stable. TheMailroom will move to v2
only when the upstream maintainer tags a stable release.

## 4. Data flow (read example)

```
AI agent
  |
  | MCP tool call: list_messages { account_id, mailbox, limit }
  v
internal/mcpserver     -- validates schema, decodes args
  |
  v
internal/tools         -- permissions.Check(read), then registry.Resolve(account_id)
  |
  v
internal/accounts      -- returns AccountRef (no credentials)
  |
  v
internal/mailbox.Backend  (IMAP impl in Phase 2)
  |
  v
internal/tools         -- maps *MessageSummary to JSON-friendly struct
  |
  v
internal/mcpserver     -- serialises, returns to agent
```

At no point does the response contain credentials, raw IMAP wire bytes, or filesystem paths beyond what the tool explicitly returns.

## 5. Build, test, lint

- `go build ./...` — produces `bin/themailroom` via `cmd/themailroom`.
- `go test ./...` — unit tests across all packages.
- `go vet ./...` — required clean.
- `go test -race ./...` — Phase 2 onward.
- GitHub Actions CI runs the above on push and PR.
- `golangci-lint` (Phase 3 hardening) with a focused lint set.

## 6. Versioning and releases

- Semantic versioning (`vMAJOR.MINOR.PATCH`).
- Tags cut on a clean `main` only.
- AGPL-3.0 licence preserved on every change.

## 7. Open architecture decisions (deferred)

These are intentionally undecided at Phase 1; they will be resolved when their Phase arrives:

- OAuth2 helper package (Phase 2 if a provider requires it).
- IMAP IDLE / push updates (deferred; v1 is request/response).
- Connection pooling policy (Phase 2 once usage patterns are clearer).
- Rate-limit and back-off strategy (Phase 3).