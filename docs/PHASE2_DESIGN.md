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

## 7. Pre-existing deficiencies carried into Phase 2.1

The following are pre-Phase-2.1 gaps documented during the PR #3
review. They are reported here for visibility and are tracked
separately from the Phase 2.1 corrections.

- **IMAP SEARCH does not honour `Flagged` or `Unseen`.** The
  `SearchQuery` struct exposes `Flagged` and `Unseen`; the IMAP
  backend never translates them into `WithFlags` /
  `WithoutFlags` on the search criteria. The memory backend
  honours `Unseen` by definition (all memory messages are
  `\\Seen`) and ignores `Flagged`. The wire schema marks both
  fields `omitempty` so callers that do not set them see no
  behaviour change. The fix is mechanical: map `q.Flagged` to
  `crit.WithFlags = []string{"\\Flagged"}` and `q.Unseen` to
  `crit.WithoutFlags = []string{"\\Seen"}` in
  `internal/imapclient/imapclient.go::SearchMessages`. Tracked
  for Phase 2.2.
- **Tool layer: `Flagged` / `Unseen` are rejected, not silently
  ignored.** As of PR #3 follow-up #2, the tool layer
  rejects `Flagged` and `Unseen` with a typed `NotImplemented`
  error rather than silently passing them through. The
  caller must either omit the field or wait for Phase 2.2
  to land the IMAP filter implementation.
- **IMAP SEARCH `Text` field conflates Subject + From + To.**
  The IMAP `TEXT` keyword matches *any* text in headers or
  body; populating it with multiple terms makes them OR, not
  AND. A query for `subject=foo, from=bar` returns messages
  containing "foo" anywhere OR "bar" anywhere. The right fix
  is server-dependent: either issue multiple searches and
  intersect, or use provider extensions (`OR` chaining,
  `X-GM-RAW` on Gmail). Tracked for Phase 2.2.
- **`SentSince` / `SentBefore` use the RFC 5322 `Date:`
  header.** The IMAP criteria maps to the date the message
  was sent, not the internal date the server records. Some
  servers only support the latter; behaviour on those
  servers is unspecified. The right fix is to expose both
  `since_date` and `since_internal` to the caller. Tracked
  for Phase 2.2.
- **`Query.Limit` ignored at the tool layer (fixed in PR #3
  follow-up).** The tool handler for `search_messages`
  previously built a `mailbox.SearchQuery` without copying
  `args.Query.Limit`, so the caller's `limit` was silently
  dropped on the floor and replaced with the per-account
  max. Fixed in the same follow-up; the tool handler now
  propagates the value. The memory and IMAP backends
  continue to honour `q.Limit` as before.

## 8. IMAP literal memory allocation: UNBOUNDED by application cap

The go-imap v1.2.1 `Reader.ReadLiteral` allocates a buffer of
the announced literal size and reads the literal body into
it *before* the application sees the data:

    b := make([]byte, n)
    if _, err := io.ReadFull(r, b); err != nil { ... }

The application-level cap enforced by
`io.LimitReader(r, cap+1)` + length check is applied
*after* this allocation. It is an application-side backstop,
not a pre-allocation guard.

### Consequence

A server (including a malicious or buggy one) that announces
a literal larger than `max_message_bytes` causes the go-imap
client to allocate up to the announced size *before* the
application rejects the body. The default `max_message_bytes`
of 25 MiB does **not** bound this allocation. A single
`FETCH` of a `BODY[]` against a hostile server can therefore
cause unbounded memory growth limited only by the host's
physical RAM and the `net.Conn` read buffer.

### Evaluated mitigations (both rejected for Phase 2.1)

1. **`go-imap.Reader.MaxLiteralSize` via reflection.** The
   field exists on the embedded `*imap.Reader` inside
   `*client.Client`, but `Client` does not expose a setter
   and `conn` is unexported. Setting it via reflection was
   prototyped in commit `bcc3cdc` and abandoned after
   empirical testing showed that when the cap is hit, the
   resulting parse error is silently swallowed by
   `Message.Parse` (message.go:269 — the literal type
   assertion drops the error), the executor returns success
   with the message body missing, and the connection is left
   in a broken state. Reproduced in testing.
2. **A line-aware `net.Conn` filter.** A filter that scans
   the byte stream for `{N}\r\n` and closes the conn on
   oversized literals was prototyped in commit `bcc3cdc` and
   removed in the merge-prep cleanup because its
   whole-lookahead scan produced false positives on the TLS
   handshake (the binary protocol contains byte sequences
   that match `{N}\r\n` by accident). The intended Phase 2.2
   shape is a **line-state** parser that only triggers on
   literal headers preceded by a known IMAP separator (space,
   `(`, etc.). See §10 for the acceptance criteria.

### Current state

- The application cap is the only line of defence.
- A single oversized literal can cause unbounded allocation
  before rejection.
- Callers that fetch from untrusted IMAP servers must
  mitigate at the network layer (firewall / proxy with a
  body-size filter) until Phase 2.2 lands.

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

**Status:** Mandatory before any production deployment that
fetches messages from a server whose literal announcements
are not independently bounded by the network layer (e.g.
firewall, proxy with body-size filter).

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

**Implementation options (in order of preference):**

1. **Fork `github.com/emersion/go-imap` to expose
   `Reader.MaxLiteralSize` as a constructor option on
   `client.Options`, with documented migration path back
   to upstream when the library is fixed.** This is the
   cleanest option: a one-line `client.Options.MaxLiteralSize`
   that flows into the embedded `*imap.Reader`.
2. **Implement a line-state `net.Conn` wrapper that wraps
   the conn after the TLS upgrade, scans the decrypted
   byte stream for IMAP literal headers, and closes the
   conn on oversized literals.** The wrapper must
   understand the IMAP response grammar to avoid false
   positives on the TLS handshake or on binary body bytes.
   This is the prototype evaluated in commit `bcc3cdc` and
   removed in merge-prep because the initial version was
   not line-aware.
3. **Replace `github.com/emersion/go-imap` with a different
   IMAP client library that exposes a literal-size cap
   natively.** This is the largest scope and is only
   justified if options 1 and 2 are not viable.

**Out of scope (deferred, not a blocker):** Outbound IMAP
operations (`APPEND`, `COPY`) are not in scope for Phase 2.1
and do not affect this requirement.