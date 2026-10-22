# TheMailroom Architecture

> Status: Phase 2.3 implementation and documentation closeout are
> complete on `main`. PR 1 is **MERGED** (PR #10,
> `bf3618f247928ea1f1d2c687aac1cffd96a89070`), PR 2 is **MERGED**
> (PR #11, `ab928c3a33ac5b760ca714c6f915fcbfe4366212`), PR 3 is
> **MERGED** (PR #12,
> `d1ecf774809681b2795a01f91182eec15a4b2f3f`), and PR 4 (documentation
> closeout) is **MERGED** (PR #13,
> `a4851e9159af63026e5593f30988c114d7117261`). The read-only IMAP path,
> inbound attachment retrieval, recipient policy, conditional SMTP
> validation, confined outbound attachment resolver, bounded MIME
> composer, SMTP submission client and `send_message` are implemented.
> The Purelymail smoke-test campaign was executed on 2026-10-21 and
> passed; the authoritative record is
> [`LIVE_SMTP_SMOKE_TEST.md`](LIVE_SMTP_SMOKE_TEST.md). It is
> interoperability/smoke-test evidence, not production certification.
> Reply/forward and mailbox mutation are **not** implemented. Production
> hardening is not complete, and TheMailroom is not a production-hardened
> release.

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
- **Bounded list workload.** `list_messages` is ordered by internal
  date, newest first, with a descending UID tie-break. IMAP UIDs
  ascend but are unrelated to internal dates, so an exact page can
  only be produced by examining every message in the mailbox; a
  bounded UID range would return the newest-by-UID messages, which
  is a different set. Two bounds therefore apply:

  - *Application memory.* The summaries are collected in a bounded
    top-K selector, which retains at most
    `min(offset + limit, max_list_scan_messages)` of them. The
    working set therefore does not grow with the size of the
    mailbox, and the final sort is over that set only.
  - *Server and network work.* The per-account
    `max_list_scan_messages` limit (default 10 000, hard maximum
    1 000 000) bounds how many messages one call may examine. The
    bound is enforced twice:

    1. *Before the fetch.* The server reports the mailbox size in
       the EXAMINE response, which RFC 3501 requires, so an
       oversized mailbox is refused with an explicit typed error
       *before* any FETCH is sent.
    2. *During the fetch.* The client counts every message the
       FETCH handler delivers, independently of the date filters
       and of the selector's retention. When the server delivers
       message *budget + 1*, the client records the overrun and
       aborts the transfer by closing the underlying TCP socket.
       It issues no further IMAP command, so the abort cannot race
       the command writer; it keeps draining the response channel,
       because the go-imap response handler blocks on the channel
       send and a blocked handler stops the reader; and it awaits
       the completion of the in-flight FETCH before returning.

  A refused mailbox produces an error, never a partial or
  approximate page. The `since` / `before` filters are applied
  client-side after the fetch, so they narrow the result but not
  the work. They cannot make an oversized mailbox listable, and
  the refusal therefore does not suggest them: the recourse is to
  raise `limits.max_list_scan_messages`, subject to its hard
  maximum. When the refusal was triggered by the observed
  deliveries rather than by the reported size, the message reports
  the observation, not the server's count.

  The second check is what makes the bound real, because it does not
  depend on the server's own count: a server that under-reports its
  mailbox size is caught while the listing runs, and the listing is
  refused rather than continued.

  The precise guarantee is that the limit bounds the messages
  delivered by the FETCH handler and processed at the application
  level; detection necessarily observes message *budget + 1*. It is
  *not* a strict bound on all protocol work. In particular it does
  not bound the raw responses the server sends, the responses the
  fork discards internally (a FETCH response without a UID is
  dropped by the response handler and is never counted), the total
  bytes transferred or buffered, the allocation for a single
  oversized envelope, or the wall-clock duration of the call. Those
  residuals are tracked in `docs/THREAT_MODEL.md` §3.1. Explicit
  IMAP command timeouts are a separate control and remain
  independent of this budget.

  The in-flight FETCH is always completed before the connection is
  closed, because the go-imap client does not serialise writes to
  the connection and closing it while a command is in flight is a
  data race. The aborted client is discarded together with its
  socket, so the next operation dials a fresh connection.
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
sending method was added to `mailbox.Backend`.

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
`docs/PHASE2_DESIGN.md` §3 and `docs/PHASE2.3.md` PR 3. The sender was
exercised against Purelymail on 2026-10-21 in the live smoke-test
campaign; the authoritative record is
[`LIVE_SMTP_SMOKE_TEST.md`](LIVE_SMTP_SMOKE_TEST.md). That campaign is
interoperability/smoke-test evidence, not production certification.

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
- `send` — outbound submission through `send_message`; the same
  capability is intended for future reply/forward tools.
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
- `server.max_request_bytes` defaults to 32 MiB and is capped at 128 MiB.
  It is a per-frame pre-decode transport boundary and is independent of
  account `max_message_bytes`.
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

A `password_command` is executed directly, never through a shell, so no
argument is ever interpolated into a command line. Both the IMAP and the
SMTP path apply the same constraints:

- stdout is capped at 8 KiB. Output beyond the cap is discarded as it
  arrives, so the retained memory does not grow with the amount of output,
  and the child process is never blocked by a full pipe. Exactly 8 KiB is
  accepted; one byte more is rejected.
- stderr is discarded, not captured. It is arbitrary operator-supplied
  text that may embed secrets, and it is not needed to classify the
  failure.
- The run is bounded by a timeout — 30 seconds for IMAP, the configured
  credential timeout for SMTP — and by the caller's context, so a
  cancelled operation stops the command. A child that outlives
  cancellation while holding the stdout pipe open cannot wedge the
  caller: the wait for inherited pipe handles is bounded separately.
- Neither the command's stdout nor its stderr is ever included in an
  error, a log or an MCP response. The exit status and the timeout or
  cancellation classification are the only failure diagnostics kept.
- Trailing CR and LF characters are trimmed from the captured stdout, so
  a command that prints a trailing newline is handled exactly as before.

The external command is a locally configured executable that runs with
TheMailroom's privileges. Bounding its output bounds TheMailroom's
resource use and output exposure; it does not make the command itself
trustworthy. See [`THREAT_MODEL.md`](THREAT_MODEL.md) T2.

The legacy `insecure_skip_verify` field has been **removed** from
production configuration. There is no way to disable TLS certificate
verification through the configuration file. Tests against a
self-signed server must use the system trust store (e.g. install the
CA) or a `SSL_CERT_FILE` override at the host level.

## 3. Dependency choices

| Concern              | Choice                                          | Rationale                                                       |
|----------------------|-------------------------------------------------|-----------------------------------------------------------------|
| MCP transport        | `github.com/modelcontextprotocol/go-sdk` v1.5.0 | Official Go SDK; stdio transport; schema-validated tools; v1.5.0 carries the upstream `ioConn.protocolVersion` race fix. |
| YAML config          | `gopkg.in/yaml.v3`                              | Mature, widely used; supports strict unknown-key rejection.    |
| IMAP (Phase 2)       | `github.com/flowbyte-com/go-imap` (fork of `github.com/emersion/go-imap` v1.2.1) | De facto standard; protocol-correct; stable API. The fork adds protocol-level IMAP literal-size enforcement; see `docs/PHASE2_DESIGN.md` §8/§10. |
| SMTP (Phase 2.3)     | `github.com/emersion/go-smtp` v0.25.0 + `github.com/emersion/go-sasl` | Implemented in `internal/smtpclient`; low-level `Mail`/`Rcpt`/`Data`/`CloseWithResponse` are used so the client can abort before DATA and classify ambiguity. |
| MIME (Phase 2)       | `github.com/emersion/go-message`                | Parser implemented as `internal/mimeparse`; bounded composer implemented as `internal/mailcompose` in Phase 2.3 PR 2. Charset-aware; handles RFC 2047. |
| Logging              | `log/slog` (stdlib)                             | Go 1.21+; structured logging; no extra dep.                    |
| Filesystem confine   | `os.Root` (stdlib, Go 1.24+)                    | OS-level chroot-style; prevents escape by symlink.             |
| Testing              | stdlib `testing` + emersion server packages     | Stub-friendly; integration via local in-process servers later.  |

The MCP Go SDK is pinned to v1.5.0, the first release that carries the
upstream fix for the `ioConn.protocolVersion` data race (upstream PR #832,
"protect ioConn.protocolVersion with a mutex") and that ships with the
SDK's own race test re-enabled after the related data-race fixes (upstream
PR #851). v1.5.0 also re-enables the SDK's race test across its own suite,
so the upgrade brings fixes beyond the single field that the bounded frame
reader exposed.

The SDK continues to own JSON-RPC decoding, correlation, lifecycle handling
and legacy batch semantics. TheMailroom owns only the inbound byte
boundary: `internal/mcpserver/transport.go` caps and validates one frame
before the SDK decodes it, and hands the SDK a plain `mcp.IOTransport`.

### 3.1 Observable compatibility differences of the v1.5.0 upgrade

The upgrade was assessed by replaying identical raw JSON-RPC frames against
v1.1.0 and v1.5.0 and diffing the emitted bytes. Everything the bounded
framing design depends on is byte-identical: exact frame-size enforcement,
framing-error propagation and transport termination, clean EOF,
cancellation, initialization responses, normal tool invocation, legacy
batch responses and their correlation, the `tools/list` tool set, and
stdout protocol integrity (including JSON escaping of `<`, `>` and `&` in
text content). Three differences are observable:

1. **Protocol version set.** v1.5.0 adds `2025-11-25` and makes it the
   newest supported version. `2024-11-05`, `2025-03-26` and `2025-06-18`
   still negotiate to themselves unchanged; a client that requests an
   unknown version now falls back to `2025-11-25` instead of `2025-06-18`.
   A client that does not send a version is unaffected. Legacy JSON-RPC
   batching remains available only for `2024-11-05` and `2025-03-26`
   clients, because the SDK refuses a batch at the transport boundary from
   `2025-06-18` onwards; this refusal is unchanged by the upgrade.

2. **Tool input validation errors are tool results, not JSON-RPC errors.**
   Upstream PR #863 ("return input validation errors as tool results, not
   JSON-RPC errors") moved schema-validation and argument-unmarshal
   failures from a JSON-RPC `-32602` error to a `CallToolResult` with
   `isError: true`. This matches the MCP specification, which classifies
   input validation errors as tool execution errors so that a model can see
   the message and retry with corrected arguments. It also makes
   TheMailroom internally consistent: its own tool-level failures (for
   example `AccountNotFound` or `PermissionDenied`) were already reported
   as `isError: true` results. Unknown tools remain a JSON-RPC protocol
   error, as the specification requires. A client that inspects only the
   JSON-RPC error and ignores `isError` would misread a rejected call as a
   success; clients are specified to surface tool execution errors to the
   model, and TheMailroom's own documented error model is unaffected.

3. **Tool schema precision.** The transitive `jsonschema-go` upgrade from
   v0.3.0 to v0.4.2 renders Go slices and pointers as nullable
   (`["null","array"]`, `["null","string"]`) and adds the numeric bounds of
   `uint32` fields. `required` lists and `additionalProperties: false` are
   unchanged, the tool set is unchanged, and the change only widens what
   the advertised schemas accept.

No upstream compatibility knob restores the previous validation-error
shape; `MCPGODEBUG` covers JSON escaping and the streamable HTTP
transport's origin checks only.

Avoided-over plugins:

- No plugin framework, no Lua, no dynamic loading.
- No general-purpose email "client" framework; we own the protocol calls.
- No HTTP framework — stdio only.

### 3.2 Why go-imap v1 (not v2)

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