# TheMailroom Architecture

> Status: Phase 2.3 PR 1 is **MERGED** (PR #10) and PR 2 is **MERGED**
> (PR #11). PR 3 (authenticated SMTP submission and `send_message`) is
> **IN REVIEW**. The read-only IMAP path, inbound attachment retrieval,
> recipient policy, conditional SMTP validation, confined outbound
> attachment resolver, bounded MIME composer, SMTP submission client and
> `send_message` are implemented. Reply/forward and mailbox mutation are
> **not** implemented, and Phase 2.3 is **not** done.

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
|  internal/smtpclient      (authenticated SMTP sender)   |
|  internal/mailcompose     (bounded MIME composer)       |
|  internal/mimeparse       (read-only MIME parser)       |
|  internal/attachments     (os.Root outbound confinement) |
|  internal/recipients      (outbound recipient policy)   |
+---------------------------------------------------------+
|  internal/accounts        (Account registry, conns.)    |
|  internal/permissions     (caps, enforcement)           |
|  internal/config          (load + validate config)      |
|  internal/searchspec      (shared search semantics)     |
+---------------------------------------------------------+
|  internal/version         (build metadata)              |
+---------------------------------------------------------+
```

### 2.1 Transport and tool registration (`internal/mcpserver`)

Wraps the official Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`). The server registers tools in one place. Each tool advertises a JSON Schema for its parameters and a typed Go handler. Stdio is the only transport in v1.

The MCP layer never sees credentials. All inputs are validated against the tool schema, then routed through the `permissions` and `accounts` layers.

### 2.2 Tool definitions (`internal/tools`)

Tools are the public surface. They are grouped by capability:

- Read-only:
  - `list_accounts`
  - `list_mailboxes`
  - `list_messages`
  - `read_message`
  - `read_attachment`
  - `search_messages`
- Sending: `send_message` (implemented); `reply_message` and
  `forward_message` remain future
- Modification (future): `move_message`, `flag_message`, `archive_message`, `delete_message`

Tools return structured payloads (JSON) and never include credentials or arbitrary raw protocol output. Errors are typed (`PermissionDenied`, `AccountNotFound`, `InvalidInput`, `RateLimited`, etc.) and serialised without leaking internal details. Unknown internal failures map to `InternalError`; `BackendError` remains a reserved code that is not currently emitted.

### 2.3 Mailbox abstraction (`internal/mailbox`)

A `Backend` interface defines the operations needed by tools:

```go
type Backend interface {
    Mailboxes(ctx context.Context, account AccountRef) ([]Mailbox, error)
    ListMessages(ctx context.Context, account AccountRef, mailbox string, opts ListOptions) (ListResult, error)
    FetchMessage(ctx context.Context, account AccountRef, mailbox string, uid uint32, uidValidity uint32) (*Message, error)
    FetchAttachment(ctx context.Context, account AccountRef, mailbox string, uid uint32, uidValidity uint32, part string) (*AttachmentData, error)
    SearchMessages(ctx context.Context, account AccountRef, mailbox string, q SearchQuery) (ListResult, error)
}
```

Two mailbox backends exist:

1. `internal/mailbox/memory` — loads `.eml` files from a directory. Used
   for development and tests. Rejects symlink entries.
2. `internal/imapclient` — the production IMAP4rev1 implementation.
   **Read-only** in this slice. Modifications and deletions land in
   subsequent slices.

Outbound submission is deliberately separate from `mailbox.Backend`.
`internal/smtpclient` owns the short-lived SMTP transaction, and
`internal/attachments` supplies confined, frozen attachment data. No
sending method was added to the mailbox abstraction.

A new test fixture in `internal/imapclient` runs the production client
against an in-process go-imap server over TLS, exercising the full
real-protocol path. The end-to-end test in `internal/tools` exercises
the same backend through the MCP tool layer.

### 2.4 IMAP (`internal/imapclient`)

**Status (this slice):** `internal/imapclient` ships the read-only
IMAP4rev1 backend. It uses a narrow fork of
`github.com/emersion/go-imap` v1.2.1 —
`github.com/flowbyte-com/go-imap` at module version
`v1.2.1-themailroom.4` — and supports:

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

### 2.4a SMTP submission (`internal/smtpclient`)

Phase 2.3 PR 3 implements authenticated submission in
`internal/smtpclient`. The tool layer supplies a fully prepared message,
the account's configured envelope identity and the policy-approved
envelope recipient list. The sender has no mailbox methods and no
sending method is added to `mailbox.Backend`.

The sender is deliberately minimal:

- One short-lived TCP connection per submission; no pool, queue,
  background worker, retry or pipelining.
- Implicit TLS or mandatory STARTTLS only. Certificates and hostnames
  are verified against the configured SMTP host, TLS 1.2 is the
  minimum, and there is no plaintext or disabled-verification mode.
- AUTH PLAIN after verified TLS. Servers that do not advertise PLAIN
  are rejected before credentials are sent.
- `MAIL FROM` uses only the account's configured `email`. All unique
  policy-approved To/Cc/Bcc addresses become `RCPT TO` envelope
  recipients; Bcc never appears in MIME headers.
- `DATA` is issued only after every recipient is accepted. Any RCPT
  rejection aborts before DATA and attempts RSET. There is no partial
  recipient send.
- The prepared bytes are written unchanged. Only an explicit final 2xx
  response after DATA is success. A final 4xx/5xx is a definite
  rejection; a lost connection, timeout or cancellation after DATA
  begins is reported as an ambiguous submission with the original
  Message-ID and is never retried.
- DNS/TCP, TLS, greeting/EHLO, STARTTLS, authentication, MAIL, RCPT,
  DATA writing and final-response stages have finite deadlines. MCP
  cancellation closes the active connection.

SMTP acceptance is not final mailbox delivery. The tool does not create
an automatic Sent copy. The full semantics are in
`docs/PHASE2_DESIGN.md` §3 and `docs/PHASE2.3.md` PR 3.

### 2.5 MIME (`internal/mimeparse`, `internal/mailcompose`)

Both packages use `github.com/emersion/go-message`.
`internal/mimeparse` is the read-only parser that both mailbox
backends use so memory and IMAP expose the same structured view.
`internal/mailcompose` is the outbound composer added in Phase 2.3
PR 2. It is a pure, in-memory component: it accepts a fixed `From`
identity, To/Cc/Bcc values (canonicalised internally), text/HTML bodies
and frozen attachment data, and returns complete serialized bytes plus the
generated Message-ID. It has no SMTP connection, MCP handler,
filesystem access, path resolution, mailbox state or provider-specific
behaviour. Bcc is validated but omitted from serialized headers.

The composer emits a single `text/plain` or `text/html` body when only
one format is supplied, a `multipart/alternative` container (plain then
HTML) when both are supplied, and a `multipart/mixed` root with the body
child first followed by attachments in request order. Attachments use
base64 and RFC 2231 filenames. A bounded writer enforces
`max_message_bytes` on the final serialized message; per-file, total
attachment and attachment-count limits are re-checked defensively.
The same limit bounds header preparation before RFC 2047/RFC 2231
encoding and address-list joins, so a small serialized limit also bounds
temporary header allocations. Exactly-at-limit succeeds and any overage
returns a typed error with no partial result.

The parser:

- Decodes text/* and html/* bodies, applying the declared
  Content-Transfer-Encoding and charset (RFC 2047) automatically.
- Walks multipart/alternative, multipart/mixed, multipart/related
  and any combination of nested multipart containers.
- Captures attachment metadata (filename, mime type, size, part
  path, Content-ID, disposition) without materialising the
  attachment body in the wire response.
- `ExtractAttachment` shares that traversal to return exactly one
  selected exposed attachment, after the complete message and all
  limits have been validated. It never returns partial content:
  malformed siblings, body read/transfer-decoding failures, unknown
  transfer encodings and oversized ordinary body parts are typed
  errors. `Parse` keeps its best-effort warning behaviour.
- Rejects multipart containers, ordinary body parts, missing parts
  and hidden descendants of opaque `message/rfc822` attachments.
- Enforces per-message, per-attachment, total-attachment, nesting,
  attachment-count and part-count limits before allocating large
  buffers.

### 2.6 Attachments (`internal/attachments`)

The single most security-sensitive path for outbound email. Implemented
in Phase 2.3 PR 1 and wired into `send_message` in PR 3. It uses Go
1.24+ `os.Root` for filesystem confinement:

- Every `attachment_send_paths` entry is opened once as an `*os.Root`.
  Empty paths mean no resolver and therefore no attachment source.
- Agent references are relative paths only. Absolute paths, drive
  qualifiers, NUL/control characters, `..` segments and empty paths are
  rejected before any filesystem access.
- A reference is checked against every root. If it resolves in more
  than one root, the request is rejected as ambiguous instead of
  silently selecting one.
- Only regular files are accepted. Reads use the root's file descriptor,
  are capped by `max_attachment_bytes` on actual bytes read, and are
  counted against `max_total_attachment_bytes`.
- Resolved data is frozen in memory before return; resolved host paths
  are never returned. Roots are closed by the registry shutdown path.
  Each account has its own roots and policy; cross-account access is
  impossible.
- `send_message` resolves and freezes `[]attachments.File` before
  opening SMTP. `internal/mailcompose` consumes that data directly and
  never reopens a path.

### 2.6a Search semantics (`internal/searchspec`)

The IMAP backend delegates matching to the IMAP server; the
in-memory backend matches locally. For the public API contract
to mean anything, the two must agree. `internal/searchspec`
holds the parts that must be identical:

- **Date granularity.** An IMAP SEARCH date key carries a date
  with no time component, so date matching is at day
  granularity. Query values use `TruncateDate`, which converts
  the timestamp to UTC and then takes the UTC day. Message
  `Date:` headers use `CalendarDay`, which keeps the calendar
  day named by the header's own fields and ignores the time and
  timezone offset. The reference server compares the header's
  own calendar day; using `TruncateDate` on the message side
  would shift non-UTC headers near midnight. Both backends apply
  the same two rules, so they agree.
- **Flag predicates.** `MatchesUnseen` and `MatchesFlagged`
  implement the IMAP definitions: unseen means the absence of
  `\Seen`, flagged means the presence of `\Flagged`.
- **Flag derivation for the in-memory backend.** Conventional
  mbox `Status:` (`R` = `\Seen`, `O` = old/no flag) and
  `X-Status:` (`D`/`F`/`A` = `\Deleted`/`\Flagged`/`\Answered`)
  header translation, so an in-memory message can answer the
  flag filters.

The header criteria are ANDed. RFC 3501 section 6.4.4 ANDs
multiple SEARCH keys and provides the `OR` key, so the IMAP
backend sends one SEARCH with the header keys, body, flags and
dates combined, and an explicit `OR To Cc` for the recipient
filter. The in-memory backend ANDs the same criteria natively.
No client-side UID-set intersection is used.

`internal/imapclient/search_consistency_test.go` runs the same
query set against both backends over identical fixture
messages and requires identical results.

### 2.7 Accounts (`internal/accounts`)

A registry of configured accounts. Each entry holds:

- `id` — opaque, stable identifier exposed to tools.
- Provider label and the account's configured email identity.
- Permissions and limits.
- An unexported copy of the account's SMTP submission settings for the
  sender. These settings can contain credentials but are never part of
  `View`, `list_accounts`, logs or any other MCP-visible shape.
- A parsed recipient policy for outbound validation.
- An optional `internal/attachments` resolver for `attachment_send`
  accounts. Absolute roots are not copied into the MCP view.

The IMAP backend uses a single shared connection per account under a
mutex; there is no connection pool yet. SMTP submission creates one
short-lived connection per call. Credentials are never copied into
MCP-visible data structures.

### 2.8 Permissions (`internal/permissions`)

A small, explicit capability set:

- `read` — list/read/search messages and attachments.
- `send` — compose/send/reply/forward.
- `modify` — flag, move, archive (without deletion).
- `delete` — explicit deletion.
- `attachment_send` — sending with a local attachment requires this
  capability *and* a configured `attachment_send_paths` root.
  `send_message` enforces it whenever the attachment list is non-empty.

Each tool handler calls `permissions.Check(account, capability)` first. The check returns `PermissionDenied` if missing, and the tool refuses the operation. The check is server-side; we do not trust the MCP client.

### 2.9 Configuration (`internal/config`)

Loads YAML. The loader is **strict**: unknown fields and duplicate
keys are rejected at parse time, before validation. This prevents
silent typos (e.g. `permisions` instead of `permissions`) and
silent overrides that would otherwise mask a misconfiguration.

Validation covers:

- Account IDs unique.
- IMAP endpoints and credentials are required for `backend: imap`.
- SMTP endpoints, TLS mode and credentials are required only for
  accounts with `send`. Exactly one of `tls` or `starttls` must be
  enabled; plaintext submission is rejected. Read-only accounts may
  leave SMTP unset.
- Limits are non-negative; `max_recipients` defaults to 50.
- Permission names are known capability names.
- `allowed_recipients` entries parse as addresses; matching is exact,
  preserving local-part case while normalizing domain case.
- `attachment_send_paths` are absolute, NUL-free paths. When
  `attachment_send` is granted, they must be existing directories and
  duplicates are rejected; the resolver opens them with `os.Root`.

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
| SMTP (Phase 2.3)     | `github.com/emersion/go-smtp` v0.25.0 + `github.com/emersion/go-sasl` | Implemented in `internal/smtpclient`; low-level `Mail`/`Rcpt`/`Data`/`CloseWithResponse` are used so the client can abort before DATA and classify ambiguity. |
| MIME (Phase 2)       | `github.com/emersion/go-message`                | Parser implemented as `internal/mimeparse`; bounded composer implemented as `internal/mailcompose` in Phase 2.3 PR 2. Charset-aware; handles RFC 2047. |
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

## 4a. Data flow (send example)

```
AI agent
  |
  | MCP tool call: send_message { account_id, to, cc, bcc, subject, text, html, attachments }
  v
internal/tools         -- account lookup, send/attachment_send checks,
  |                       raw recipient budget, recipient policy,
  |                       confined attachment resolve, MIME composition,
  |                       max_message_bytes
  v
internal/smtpclient    -- one connection: TLS -> AUTH PLAIN -> MAIL FROM ->
  |                       all RCPT TO -> DATA -> prepared bytes -> final response
  v
SMTP submission server
```

The sender receives only the prepared message, envelope From and
envelope recipients. It never sees caller-selected headers or a raw
filesystem path. The MCP response contains the account ID, original
Message-ID, submitted status, recipient count and attachment counts; it
never contains raw server responses, credentials or the full recipient
list.

## 5. Build, test, lint

- `go build ./...` — produces `bin/themailroom` via `cmd/themailroom`.
- `go test ./...` — unit tests across all packages.
- `go vet ./...` — required clean.
- `go test -race ./...` — required by CI.
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