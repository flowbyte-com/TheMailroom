# Phase 2 Design Notes

> Status: in progress. Phase 2.1 (read-only IMAP correctness + MIME
> reading) is implemented in `internal/imapclient` and
> `internal/mimeparse`. This document captures the design decisions
> for the remaining slices: SMTP submission, MIME composition, reply /
> forward, attachments, and protected mailbox modifications.

## 1. Library selection (post-research)

| Concern              | Library                              | Decision |
|----------------------|--------------------------------------|----------|
| IMAP client          | `github.com/emersion/go-imap` v1.2.1 | **Selected.** v1 is the stable, production-tracked line. v2 is parallel development, not yet marked stable. TheMailroom will move to v2 only when upstream tags a stable release. |
| IMAP server (tests)  | `github.com/emersion/go-imap/server` + `backend/memory` | **Selected.** Spin up a real IMAP4rev1 server in-process for tests. |
| SMTP client          | `github.com/emersion/go-smtp` v0.25.0 | **Selected.** Mature submission client; STARTTLS, AUTH (PLAIN, LOGIN, CRAM-MD5). |
| MIME parser/composer | `github.com/emersion/go-message` v0.18.2 | **Selected.** Charset-aware, multipart-aware, RFC 2047 handling. **Implemented in Phase 2.1** as `internal/mimeparse`. |
| Filesystem confine   | `os.Root` (stdlib, Go 1.24+) | **Selected.** OS-level confinement; prevents symlink escape. |
| Self-signed test CA  | `crypto/rsa`, `crypto/x509`, `crypto/tls` (stdlib) | **Selected.** Generate certs in tests; trust via `InsecureSkipVerify` only in the test-only `Options.TLSConfig` knob. |

## 2. Attachment reference model

The brief in P2 mandates outbound attachments including multiple
files, replies and forwards. The security boundary is between
arbitrary filesystem content (untrusted) and message data (also
untrusted but constrained to per-account roots).

### 2.1 Reference types

The MCP tool surface accepts two attachment references:

1. **Path reference** — a string. The path is resolved against the
   account's `attachment_send_paths` roots. Allowed only when the
   account has the `attachment_send` capability. Refused outright if
   the resolved path escapes any root.

2. **Inline reference** (Phase 2.5) — a base64 blob supplied by the
   tool. Refused for any account without the `attachment_send`
   capability and rate-limited per `LimitsConfig`.

The path reference is the primary mechanism because the agent's
working files are the typical attachment source.

### 2.2 Filesystem confinement

Each `attachment_send_path` becomes an `*os.Root` at startup. The
resolver opens any path through one of the roots:

```go
type Resolver struct {
    roots []*os.Root
}

func (r *Resolver) Open(name string) (*os.File, error) {
    for _, root := range r.roots {
        f, err := root.Open(name)
        if err == nil {
            return f, nil
        }
    }
    return nil, ErrAttachmentNotFound
}
```

`os.Root.Open` rejects:
- Absolute paths.
- Paths with `..` segments.
- Symlinks that escape the root.
- Anything not under the root.

`os.Root.Stat` and `os.Root.Lstat` provide additional introspection
without exposing the kernel-level errors. Any escape attempt is
returned as a generic `ErrPathEscape`; we do not leak the resolved
path back to the agent.

### 2.3 Limits

Each account has:
- `max_attachments` (default 50): hard cap on attachments per send.
- Per-attachment size cap: `max_message_bytes / 4` (default 6.25 MiB).
- Total size cap: `max_message_bytes` (default 25 MiB).
- MIME nesting depth cap: 16 (rejects `message/rfc822` bombs).

These are enforced in the SMTP submission path before any DATA is
written.

## 3. SMTP submission semantics

### 3.1 Single submit cycle

TheMailroom performs one `MAIL FROM` / `RCPT TO` / `DATA` cycle per
send. There is no internal queue, no retry, no pipelining.

This is intentional:

- We do not have a notion of a "send spool" — a crashed send is a
  failed send.
- The agent (MCP caller) is responsible for retrying with the
  **same** Message-ID if needed; TheMailroom never reuses a
  Message-ID.
- Duplicate sends are the agent's responsibility. TheMailroom
  surfaces the `Message-ID` and the SMTP server's response so the
  agent can detect duplicates on its end.

### 3.2 Ambiguous failure handling

Network errors after the DATA payload was sent but before the
server's `250` confirmation are **not retried** automatically. They
are returned as `SmtpAmbiguousFailure` with the `Message-ID` of the
attempted message. The agent is told "the message may or may not
have been accepted; verify on the server before retrying".

This is the strictest of three reasonable choices; it is what
RFC 5321 Section 6.1 recommends for client implementers and what
limits accidental duplicate sends.

### 3.3 Sent-folder strategy

TheMailroom does **not** automatically append sent messages to a
Sent folder. Reasons:

- Providers that auto-Sent (Gmail, Purelymail, Fastmail, etc.) will
  save a server-side copy regardless of what the client does.
- Providers that do not auto-Sent usually have a separate
  IMAP APPEND path; mixing SMTP submission and IMAP APPEND in a
  single tool adds complexity for no real benefit.

A future option, `sent_copy: true` per account, will use IMAP
APPEND to the configured Sent mailbox after a successful send.
This is opt-in because it changes the on-server state.

### 3.4 Recipient restrictions

Optional `allowed_recipients` per account:

```yaml
accounts:
  - id: work
    email: alice@work.test
    allowed_recipients:
      - alice@work.test
      - team@work.test
    ...
```

If set, the SMTP `RCPT TO` must match. Otherwise the SMTP backend
returns `RecipientNotAllowed` and the message is not sent. If the
list is empty, any recipient is allowed.

### 3.5 Sender identity enforcement

The SMTP `MAIL FROM` and the RFC 5322 `From:` header must match the
account's configured `email`. A mismatched `From:` is rejected
client-side before the connection is opened. A mismatched SMTP
`MAIL FROM` is closed by the server; TheMailroom reports it as
`SenderRejected` without retrying.

This prevents the agent from using one configured account to send
as another. It is a hard architectural rule; configuration cannot
disable it.

## 4. Reply and forward semantics

### 4.1 Reply

- `reply_message` takes `(account_id, mailbox, uid, all?, body,
  attachments?)`.
- The original message is FETCHed via IMAP, the In-Reply-To and
  References headers are computed, and the body is composed with
  `multipart/alternative` for the new text (and HTML if supplied).
- Default quoting prefix is `> ` and quoted text is included in a
  `text/plain` part; the original is not re-attached unless
  `attach_original: true`.
- Reply-All expands the To/Cc lists from the original envelope,
  minus the account's own address.

### 4.2 Forward

- `forward_message` takes `(account_id, mailbox, uid, to, body?,
  attachments?)`.
- The original message is included as `message/rfc822` in a
  `multipart/mixed` wrapper, with the original attachments
  preserved as nested parts. Forwarded attachments do **not**
  require `attachment_send` because they are re-encoded, not
  re-read from the filesystem.

## 5. Mailbox modifications

### 5.1 Move and copy

- `move_message(account, mailbox, uid, dest, uid_validity)`: UID
  COPY to `dest` followed by UID STORE +FLAGS \Deleted and EXPUNGE.
- `uid_validity` is required; a mismatch returns `UidMismatch`
  without acting.
- The original mailbox is re-SELECTed after the move; UIDVALIDITY
  is re-read and returned in the result.

### 5.2 Flag

- `flag_message(account, mailbox, uid, op, flags, uid_validity)`:
  UID STORE with `+FLAGS` or `-FLAGS`. op ∈ {`add`, `remove`, `set`}.
- Returns the new flag set.

### 5.3 Archive

- `archive_message(account, mailbox, uid, uid_validity)`: configured
  `archive_mailbox` per account (default `Archive`). Equivalent to
  `move_message` to that mailbox.

### 5.4 Delete

- `delete_message(account, mailbox, uid, uid_validity, expunge?)`:
  move to the configured `trash_mailbox` (default `Trash`); if
  `expunge=true`, EXPUNGE the source mailbox.
- Requires the `delete` capability. Trash is the only supported
  form for v1; no `delete_immediately` option.

## 6. OAuth2 (deferred)

If a target provider requires OAuth2 (none in the initial target
list), TheMailroom will add an `auth` block with `method: oauth2`
and an `oauth2` sub-block containing `token_command` (analogous to
`password_command`). The token is refreshed on 401 responses. The
implementation is intentionally deferred to a separate slice.

## 7. Search correctness: deficiencies and their resolution

The following were pre-Phase-2.2 gaps, documented during the
PR #3 review. They are recorded here for visibility; each one
now states its resolution.

- **IMAP SEARCH did not honour `Flagged` or `Unseen`.**
  *Resolved in Phase 2.2 Priority 2.* The IMAP backend now maps
  `q.Flagged` to `WithFlags = []string{"\\Flagged"}` and
  `q.Unseen` to `WithoutFlags = []string{"\\Seen"}`. The memory
  backend honours the same predicates; the flags of an
  in-memory message are derived from the RFC 2156 `Status:`
  header, so a message with no `Status:` header is unseen and
  not flagged. The shared predicates live in
  `internal/searchspec`.
- **The tool layer rejected `Flagged` / `Unseen` with a typed
  `NotImplemented` error** rather than silently passing them
  through to backends that ignored them. *Resolved in Phase 2.2
  Priority 2.* Now that both backends honour the filters, the
  rejection has been removed and the fields are passed
  through.
- **IMAP SEARCH used the generic `Text` keyword for
  Subject + From + To,** which matches any text in the headers
  or the body and ORs the terms together. A query for
  `subject=foo, from=bar` returned messages containing "foo"
  anywhere OR "bar" anywhere. *Resolved in Phase 2.2 Priority
  2.* The backend now uses the corresponding IMAP header
  search keys (`Subject`, `From`, `To`, `Cc`) and computes the
  conjunction in the client: one SEARCH per header criterion
  (two for `to`, which matches To or Cc) and an intersection
  of the resulting UID sets. This is server-independent — no
  provider extensions such as `X-GM-RAW` are used — and gives
  the caller true AND semantics.
- **`SentSince` / `SentBefore` use the RFC 5322 `Date:`
  header,** not the internal date the server records, and the
  IMAP date key carries no time component, so the comparison is
  at day granularity. *Resolved in Phase 2.2 Priority 2.* The
  day granularity is now explicit in the public contract: the
  search value is truncated to its UTC day before it is
  applied. The message side is reduced to the calendar day
  named by the `Date:` header's own fields, ignoring its time
  and timezone offset, which is what the reference server
  compares. The memory backend uses the same two
  normalisations (`TruncateDate` for the query, `CalendarDay`
  for the message), so the two backends agree for `Date:`
  headers in any timezone. A server that interprets the bare
  search date in its own local timezone can still differ by up
  to one day; that is inherent to IMAP's date-only search keys
  and is documented rather than papered over.
- **`Query.Limit` was ignored at the tool layer.** *Fixed in the
  PR #3 follow-up.* The tool handler now copies
  `args.Query.Limit` into the backend query, and both backends
  honour and clamp it to the per-account `max_search_results`.
  An empty query returns every message in the mailbox, bounded
  by the per-account limit; this is now documented in
  `docs/TOOLS.md`.
- **Search limits and empty-query behaviour** are now
  specified and covered by tests in both backends.
- **Server-dependent search matching remains.** Header
  substring/case matching and RFC 2047 encoded-header handling
  are delegated to the IMAP server; the memory backend matches
  decoded headers. IMAP `BODY` is server-side and may match
  HTML, attachments or raw MIME content, while the memory
  backend searches decoded plain text only. These differences
  are inherent to delegating search to the server and are
  documented in `docs/TOOLS.md`.
- **Attachment metadata in list/search summaries is deferred.**
  The IMAP backend's list and search summaries do not populate
  `has_attachments` (it is always false); the memory backend
  does. Populating it requires `BODYSTRUCTURE` parsing and is
  deferred to Phase 2.2 Priority 3 (`read_attachment`).

Cross-backend agreement on the tested criteria is asserted
directly: `internal/imapclient/search_consistency_test.go` runs
the same seventeen query combinations against the IMAP backend
and the memory backend over the same fixture messages and
requires identical results. It also has a dedicated non-UTC
date-boundary case that exercises a `+0200` and a `-0500` `Date:`
header.

## 8. IMAP literal memory allocation: enforced at the protocol layer

### The upstream problem

The go-imap v1.2.1 `Reader.ReadLiteral` allocates a buffer of
the announced literal size and reads the literal body into it
*before* the application sees the data:

    b := make([]byte, n)
    if _, err := io.ReadFull(r, b); err != nil { ... }

An application-level cap enforced by
`io.LimitReader(r, cap+1)` + length check is therefore only an
application-side backstop: it runs AFTER the protocol layer has
allocated the buffer, so it does not bound the allocation. A
server (including a malicious or buggy one) that announces a
literal larger than `max_message_bytes` causes the go-imap
client to allocate up to the announced size before the
application rejects the body. The default 25 MiB cap did not
bound this allocation.

### The implementation (Phase 2.2, Priority 1)

TheMailroom now uses a **narrow fork** of go-imap:

- Module: `github.com/flowbyte-com/go-imap`
- Version: `v1.2.1-themailroom.4`
- Upstream: `github.com/emersion/go-imap` v1.2.1
- License: BSD-2-Clause, preserved verbatim
- Fork changelog: `CHANGES.md` in the fork repository

The fork's change set is deliberately small (two upstream
source files):

1. **`read.go`**
   - Exports `imap.ErrLiteralSizeExceeded`.
   - `ReadLiteral` returns it when the announced size exceeds
     `Reader.MaxLiteralSize`. **The check runs before the
     `make([]byte, n)` call**, so an over-cap announcement is
     rejected without allocating a buffer proportional to the
     announced size.
   - `IsParseError` also matches the new error, so the
     executor's existing parse-failure detection keeps working.

2. **`client/client.go`**
   - Adds `client.DialOptions{MaxLiteralSize}` and
     `client.NewWithOptions`.
   - `readOnce` closes the connection on a parse error. A parse
     error desynchronises the response stream; upstream keeps
     the connection alive, which lets the executor block
     forever waiting for a tagged response that will never
     arrive.
   - `errClosed` becomes a typed `*connectionClosedError` that
     wraps the underlying cause, so the executor's
     `loggedOut` case can return the specific error
     (`imap.ErrLiteralSizeExceeded`) instead of a generic
     transport failure.

TheMailroom sets `DialOptions.MaxLiteralSize` to the
per-account `max_message_bytes` at dial time, for both the
implicit-TLS and the STARTTLS path. The cap is set on the IMAP
*Reader*, not on the transport, so it applies identically to
both: in both cases the Reader observes the *decrypted* IMAP byte
stream.

### Constructor ordering (corrected in PR #5 follow-up)

The first cut of the fork set the cap *after* construction:

```go
func NewWithOptions(conn net.Conn, opts DialOptions) (*Client, error) {
    c, err := New(conn)          // <- parses the greeting AND
    ...                          //    starts the reader goroutine
    c.conn.Reader.MaxLiteralSize = uint32(opts.MaxLiteralSize)  // <- too late
    return c, nil
}
```

`New(conn)` constructs the reader, parses the server greeting
and starts the background reader goroutine. The assignment
therefore happened while the reader goroutine was already live,
which was both:

- **a data race** — the goroutine reads `Reader.MaxLiteralSize`
  in `ReadLiteral` (read.go) while the caller writes it; and
- **a pre-configuration allocation window** — during that
  window the live reader goroutine could accept an over-cap
  literal with *no cap at all*, i.e. an unbounded allocation of
  the announced size.

The constructor was refactored so the cap is assigned to the
`*imap.Reader` **at creation time**, before the reader is
installed in the `Conn`, before the greeting is parsed, and
before the reader goroutine exists. `New(conn)` keeps its
upstream signature and behaviour and is equivalent to
`NewWithOptions(conn, DialOptions{})`.

`Reader.MaxLiteralSize` is now written in exactly one place (the
constructor) and only before any goroutine that could read it
exists. The only other write in the tree is
`server/conn.go`, which sets the limit on the *server's* reader
at connection construction — before the server's `send`
goroutine and before its command loop start. It is likewise not
a post-start mutation.

### Evidence that the allocation is prevented

1. **Source-level.** `TestForkEnforcesCapBeforeAllocation` reads
   the fork's `read.go` from the module cache and asserts that
   the `MaxLiteralSize` check appears *before* the
   `b := make([]byte, n)` allocation.
2. **Behavioural.** `TestLiteralSizeCapEnforcedBeforeAllocation`
   uses a raw IMAP server that announces a 64 MiB literal and
   **never sends the body**. The client rejects the
   announcement immediately (bounded latency, not a timeout),
   closes the connection, and returns `ErrMessageTooLarge`. If
   the client had allocated the announced size, the test would
   have had to wait for the body.
3. **Memory.** `TestAllocatedMemoryDoesNotScaleWithAnnouncedSize`
   measures the process heap around a fetch that announces a
   512 MiB literal and asserts the allocation stays under 8 MiB.
4. **Boundary.** `TestLiteralSizeCapBoundary` exercises a real
   message at cap-1, cap and cap+1. The IMAP `BODY[]` literal
   contains the whole RFC 5322 message, so a message whose raw
   size is exactly the cap is a literal exactly at the cap: it
   is accepted; one byte over is rejected.
5. **Constructor ordering.** `TestForkAppliesCapBeforeReaderStarts`
   (TheMailroom) asserts structurally that the fork assigns the
   cap to the reader *before* the greeting is parsed, and never
   mutates the reader after the constructor returns. The fork's
   own `client/constructor_test.go` adds the behavioural and
   race-detector coverage: an over-cap literal in an unsolicited
   response that arrives with the greeting is rejected, the
   connection is terminated, and no operation reports a missing
   body as a success. Against the pre-fix constructor shape those
   tests report a data race between `Reader.ReadLiteral` and
   `NewWithOptions`.

### Remaining risks

- The fork is a temporary measure. It must be upstreamed or
  replaced with a maintained library. The route back to
  upstream is documented in the fork's `CHANGES.md`.
- The protocol-level cap bounds the *literal* size. It does not
  bound the number of literals a server may stream in one
  response, nor the total bytes across a long-lived connection.
  The application-level cap and the per-account batch limits
  remain the backstops.
- A server that announces a valid but enormous literal is
  rejected; a server that announces many *small* literals is
  bounded by the MIME part-count and attachment-count limits.

## 9. Test strategy

For each new tool and protocol path:

1. Unit tests against the `internal/mailbox/memory` backend (no
   network, fast).
2. Integration tests against the in-process go-imap / go-smtp
   servers from emersion (real protocol, controlled environment,
   no real network).
3. End-to-end tests through the MCP tool layer (in-memory
   transport; covers schema, capability enforcement, error mapping).
4. Manual tests against a real provider are **never** in CI; they
   are run by a human with a dedicated test account and live in a
   separate `manual-tests/` directory not committed to the repo.

## 10. Pre-production security requirement: protocol-level IMAP literal-size enforcement

**Status:** **SATISFIED** by the Phase 2.2 Priority 1
implementation (see §8). The requirement remains open for the
*fork* itself: the fork must be upstreamed or replaced before
the project can claim a production-hardened posture, because a
long-term dependency on a personal fork is itself a
maintenance risk. The remaining production-readiness work is
tracked in `docs/PHASE2.2.md`.

**Verification:** all twelve acceptance criteria below are
implemented and covered by the tests named against them. The
per-criterion status is recorded in the "Status" column of the
table that follows the list.

**Why this is mandatory:** §8 documents that the
application-level cap does not bound the go-imap client's
pre-allocation. A production deployment without a
protocol-level cap and without network-layer mitigation
is vulnerable to unbounded memory allocation by a hostile
or buggy IMAP server. This is unacceptable for a tool that
runs as a long-lived process handling untrusted mail.

**Acceptance criteria.** The implementation must satisfy
*all* of the following before the requirement is closed:

1. **Pre-allocation enforcement.** The cap must be enforced
   *before* the go-imap `Reader.ReadLiteral` `make([]byte, n)`
   call, not after. An oversized literal must not cause
   the go-imap library to allocate the announced size.
2. **Correct boundary.** A literal whose announced size is
   exactly the cap must be accepted; a literal one byte
   over must be rejected. The check must distinguish
   "exactly at the cap" from "above the cap" with
   confidence, not by re-reading or buffering the body.
3. **Decrypted transport compatibility.** The cap must
   apply to *both* the implicit-TLS (`tls.Dial`) and
   STARTTLS paths. A literal received over the encrypted
   channel must be subject to the same cap as one received
   before the TLS upgrade. A line-state filter must
   therefore be inserted *after* the TLS upgrade, not
   before (the TLS handshake itself is binary and must not
   be filtered).
4. **IMAP framing compatibility.** The literal-header
   detector must understand the IMAP response grammar: a
   `{N}` or `{N+}` token that is on its own line, preceded
   by a known IMAP separator (space, `(`, list boundary).
   It must not match `{N}\r\n`-shaped byte sequences that
   appear in binary content (TLS records, attachment bodies,
   `message/rfc822` parts, etc.). The default-safe heuristic
   is "only fire when the line begins with whitespace or
   `(` immediately before the `{`".
5. **Binary safety.** The cap must accept any byte
   sequence in the literal body (the body is opaque).
   The cap must not attempt to parse, classify, or
   interpret the body bytes.
6. **Disconnection safety.** When the cap fires, the
   underlying conn must be closed cleanly (so the server
   is not left waiting for a body that will not arrive).
   The go-imap client must observe the close and surface a
   typed error (`errClosed` or a parse error) to the
   executor, not a panic or a successful read with a
   missing body.
7. **Both transport modes.** Acceptance criteria 3–6
   must hold for both `cfg.IMAP.TLS == true` and
   `cfg.IMAP.StartTLS == true`. A deployment that supports
   one mode must not silently fall back to the other.
8. **No silent success.** The backend must not return a
   successful `*mailbox.Message` whose body was truncated
   or omitted. The current `FetchMessage` implementation
   includes an explicit `int64(len(rawBytes)) > cap` check
   and returns `mailbox.ErrMessageTooLarge`; this check
   must remain and any future implementation must preserve
   it.
9. **No panic-disguised-as-size-limit.** A panic in
   `go-imap`'s executor (which can happen when the conn
   is in a desynced state) must be surfaced as a generic
   internal error, not as `ErrMessageTooLarge`. The current
   `FetchMessage` implementation recovers panics and
   returns a generic `fmt.Errorf("imap: fetch panic: %v", r)`;
   this must remain.
10. **Tight error mapping.** `internal/imapclient/helpers.go::mapErr`
    must only translate a literal-cap violation to
    `mailbox.ErrMessageTooLarge` when there is concrete
    evidence in the error: specifically, the string
    `literal exceeding maximum size` (the only string
    go-imap produces from its own `MaxLiteralSize` check).
    Generic parse errors (`"expected a space"`,
    `"error reading response"`, etc.) must be mapped to
    `mailbox.ErrMalformed`, not `ErrMessageTooLarge`.
11. **Test coverage.** Unit tests must exercise the boundary
    at cap-1, cap, and cap+1 for both the protocol-level
    filter (if implemented as a net.Conn wrapper) and the
    application-level fallback. Integration tests must
    verify that a server announcing `{cap+1}\r\n` is
    rejected without `make([]byte, cap+1)` being reached
    in go-imap (verified by code inspection or
    instrumentation in the chosen implementation).
12. **Documentation.** The package doc of
    `internal/imapclient` and `docs/THREAT_MODEL.md` T4 must
    be updated to remove any wording that suggests the
    application cap bounds go-imap's pre-allocation. The
    replacement wording must explicitly state that the
    default 25 MiB cap does **not** bound go-imap's
    pre-allocation memory.

**Acceptance criteria status.**

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Pre-allocation enforcement | Met | `TestForkEnforcesCapBeforeAllocation` (source-level ordering assertion); the fork's `ReadLiteral` checks the size before `make([]byte, n)`. `TestForkAppliesCapBeforeReaderStarts` additionally asserts the cap is assigned to the reader before the greeting is parsed and before the reader goroutine starts, so there is no pre-configuration window. |
| 2 | Correct boundary (cap accepted, cap+1 rejected) | Met | `TestLiteralSizeCapBoundary` (cap-1 / cap / cap+1 against a real message); fork test `TestReader_MaxLiteralSize_Boundary`. |
| 3 | Decrypted-transport compatibility (both TLS modes) | Met | The cap is set on the IMAP `Reader`, which observes the decrypted stream in both modes. `TestOversizedLiteralUnderImplicitTLS`, `TestOversizedLiteralUnderSTARTTLS`. |
| 4 | IMAP framing compatibility | Met | The cap is enforced by the IMAP parser, not by a byte-pattern scan, so it cannot misfire on binary content. `TestBinaryContentWithLiteralHeaderBytes`. |
| 5 | Binary safety (body is opaque) | Met | The cap inspects only the announced size; the body is never parsed or classified. `TestBinaryContentWithLiteralHeaderBytes`. |
| 6 | Disconnection safety (conn closed, error typed, not a panic) | Met | The fork closes the conn on a parse error and returns a typed `*connectionClosedError`. `TestLiteralSizeCapEnforcedBeforeAllocation` (server observes the close), `TestNoSilentSuccessOnLiteralFailure`, `TestOversizedUnsolicitedLiteralRejected` (over-cap literal in an unsolicited response that arrives with the greeting). |
| 7 | Both transport modes | Met | `TestOversizedLiteralUnderImplicitTLS`, `TestOversizedLiteralUnderSTARTTLS`; the STARTTLS path sets the cap before the upgrade and the `*imap.Reader` survives the upgrade. |
| 8 | No silent success | Met | The application-level `int64(len(rawBytes)) > cap` check is retained and returns `ErrMessageTooLarge`. `TestNoSilentSuccessOnLiteralFailure`. |
| 9 | No panic disguised as a size limit | Met | The `recover()` in `FetchMessage` returns a generic `fmt.Errorf("imap: fetch panic: %v", r)`, which maps to `codeInternalError`, not `codeMessageTooLarge`. |
| 10 | Tight error mapping | Met | `mapErr` translates only `imap.ErrLiteralSizeExceeded` to `ErrMessageTooLarge`; generic parse errors map to `ErrMalformed`. `TestMapErrLiteralSizeEvidence`. |
| 11 | Test coverage | Met | Boundary tests for the protocol-level cap and the application-level fallback; integration tests for a server announcing a very large literal without sending its body. `TestLiteralSizeCapEnforcedBeforeAllocation`, `TestAllocatedMemoryDoesNotScaleWithAnnouncedSize`, `TestMalformedLiteralAnnouncement`. |
| 12 | Documentation | Met | This section, `docs/THREAT_MODEL.md` T4, `docs/ARCHITECTURE.md` and the `internal/imapclient` package doc. The wording that implied the application cap bounded the protocol-layer allocation is removed. |

**Implementation chosen: option 1 (the fork).**

Option 1 was selected and implemented. The fork is
`github.com/flowbyte-com/go-imap` at module version
`v1.2.1-themailroom.4`, pinned to the immutable commit
`0fc6600`. Its full change set is documented in the fork's
`CHANGES.md`.

Why option 1 over option 2 (the `net.Conn` filter):

- The filter has to re-implement enough of the IMAP response
  grammar to avoid false positives. The prototype evaluated in
  commit `bcc3cdc` scanned the whole lookahead and matched
  `{N}\r\n` byte sequences inside TLS records; a correct
  version needs a line-state machine that only fires on a
  literal header in a legal position. That is more fragile
  than letting the IMAP parser do the framing.
- The fork approach puts the check exactly where the
  allocation happens, so there is no possibility of a
  mismatch between the filter's view of the stream and the
  parser's.
- The fork also fixes the executor hang that the upstream
  library has on any parse error (upstream keeps a
  desynchronised connection alive, which can block the
  executor indefinitely). That fix is a prerequisite for
  option 2 as well.

Option 3 (replacing the library) remains the fallback if the
fork cannot be upstreamed.

### Dependency management

- The dependency is pinned to an immutable tag
  (`v1.2.1-themailroom.4`) in `go.mod`. No `replace` directive
  is used, so the build is reproducible from the public
  module proxy.
- `go mod verify` verifies the module contents against
  `go.sum`.
- The fork preserves the upstream BSD-2-Clause license
  verbatim and the upstream copyright notice.
- The precise differences from upstream are documented in the
  fork's `CHANGES.md`, which also describes the route back to
  upstream.
- Verification procedure:

  ```sh
  go mod verify                 # module contents match go.sum
  go list -m github.com/flowbyte-com/go-imap   # pinned version
  go test -race -count=1 ./...  # full suite, including the fork's
                                # own upstream tests
  ```

  The fork's own test suite is preserved and runs as part of
  the module's `go test ./...`; TheMailroom additionally
  asserts the fork's source-level ordering
  (`TestForkEnforcesCapBeforeAllocation`) so an upstream
  upgrade that reintroduces the allocation would be caught
  immediately.

**Out of scope (deferred, not a blocker):** Outbound IMAP
operations (`APPEND`, `COPY`) are not in scope for Phase 2.1
and do not affect this requirement.
## 11. Phase 2.2 plan

The full plan, including priorities, acceptance criteria,
deliverables, and out-of-scope items, is in
`docs/PHASE2.2.md`. The three priorities are, in order:

1. **Protocol-level IMAP literal-size enforcement** —
   resolves the mandatory pre-production security requirement
   (§10). **DONE**: merged in PR #5 as `d28dfa3`, using a
   narrowly scoped fork of `github.com/emersion/go-imap`
   (`github.com/flowbyte-com/go-imap` v1.2.1-themailroom.4)
   that exposes `Reader.MaxLiteralSize` as a
   `client.DialOptions` field. See §8 for the implementation
   and §10 for the per-criterion status.
2. **IMAP search correctness defects** — `Flagged`, `Unseen`,
   `Subject + From + To` OR semantics, `SentSince` /
   `SentBefore` semantics (§7). **IN REVIEW**: implemented in
   PR #6, with the date-semantics and limit closeout applied;
   awaiting review before merge.
3. **Secure attachment retrieval** — new `read_attachment` tool
   with stable MIME-part references and strict resource
   limits.

SMTP, outbound attachments, and mutating mailbox operations
remain out of scope.

TheMailroom is **not** production-hardened. The literal-size
requirement (§10) is satisfied, but a long-term dependency on a
personal fork of go-imap is itself a maintenance risk that must
be resolved (upstream the change, or replace the library) — see
§8 and the fork's `CHANGES.md`. The search defects (§7) and the
missing attachment retrieval tool are also still open. The
remaining production-readiness work is tracked in
`docs/PHASE2.2.md`.
