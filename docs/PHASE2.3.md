# Phase 2.3 Plan — Outbound submission foundations

> **Status:** Phase 2.3 implementation and documentation closeout are
> complete on `main`.
>
> - PR 1 (outbound security foundations) is **MERGED** (PR #10, squash
>   commit `bf3618f247928ea1f1d2c687aac1cffd96a89070`).
> - PR 2 (standalone MIME composer) is **MERGED** (PR #11, squash commit
>   `ab928c3a33ac5b760ca714c6f915fcbfe4366212`).
> - PR 3 (authenticated SMTP submission and `send_message`) is **MERGED**
>   (PR #12, squash commit
>   `d1ecf774809681b2795a01f91182eec15a4b2f3f`).
> - PR 4 (documentation closeout) is **MERGED** (PR #13, squash commit
>   `a4851e9159af63026e5593f30988c114d7117261`).
>
> The controlled Purelymail smoke-test campaign was executed on
> 2026-10-21 and passed. The authoritative execution record, including
> the distinction between live submissions and offline negative
> controls, is in
> [`LIVE_SMTP_SMOKE_TEST.md`](LIVE_SMTP_SMOKE_TEST.md). It is
> interoperability/smoke-test evidence, not exhaustive security testing
> or production certification. Reply/forward and mailbox mutation remain
> unimplemented. Production hardening is not complete; TheMailroom is a
> development milestone, not a production-hardened release. App-password
> support uses the existing `password` / `password_command` fields; no
> separate `app_password` field exists.

## Scope

Phase 2.3 implemented the outbound path in separately reviewed slices.
The milestone covers:

- RFC 5322 / MIME composition, including text and HTML bodies.
- Multiple outbound attachments with a fixed `From` identity.
- A separate sender abstraction and SMTP submission client.
- Bounded resource use and typed errors at the tool boundary.

Provider/domain/account administration APIs, reply/forward, and
mailbox-mutating operations are out of scope for this milestone.

## PR 1 — Outbound security foundations (MERGED, PR #10)

PR 1 was squash-merged as
`bf3618f247928ea1f1d2c687aac1cffd96a89070`. It was additive and did
not register `send_message`, create an SMTP client, or compose MIME. It
provides:

### Conditional SMTP validation

`config.AccountConfig` accounts with the `send` capability must have a
usable SMTP configuration:

- `smtp.host` is required and must be syntactically valid.
- `smtp.port` is 1–65535; the existing defaults are 587 for STARTTLS
  and 465 for implicit TLS.
- Exactly one of `smtp.tls` (implicit TLS) or `smtp.starttls`
  (mandatory STARTTLS) must be enabled. Plaintext and ambiguous dual
  modes are rejected.
- Authentication uses the existing schema: a username plus exactly one
  of `password` or `password_command`.
- Validation errors never include password or password-command values.
- Accounts without `send` retain their existing configuration
  requirements; SMTP settings are not validated for them.

At PR 1 merge no SMTP client was instantiated. PR 3's sender uses the
account's configured `email` as the fixed sender identity.

### Recipient policy

`allowed_recipients` is an optional per-account allow-list.
`limits.max_recipients` defaults to 50 and counts unique envelope
recipients across To, Cc and Bcc together.

The reusable policy in `internal/recipients`:

- Parses addresses with `net/mail`; display names are accepted but
  discarded for envelope matching.
- Rejects empty, malformed and control-character-containing addresses.
- Preserves the parsed local-part case and lowercases only the domain.
  The whole canonical address is compared exactly: local-part case is
  significant, while domain case is not. There is no substring, prefix,
  suffix or wildcard matching.
- Collapses duplicate canonical addresses before enforcing the limit;
  local-parts that differ only by case remain distinct recipients.
- Rejects a message with no recipients when validation is invoked.

The `send_message` handler calls this policy before opening a connection.
The composer in PR 2 only canonicalises addresses; it does not authorise
them.

### Confined outbound attachment resolver

`internal/attachments` opens every configured `attachment_send_paths`
entry once as an `*os.Root` (Go 1.24+; the module targets Go 1.27.1).

An agent reference:

- Must be a non-empty relative path using slash separators.
- Is rejected if it is absolute, drive-qualified, contains NUL or a
  control character, or contains any `..` segment.
- Is opened through every configured root. A successful open in more
  than one root is rejected as ambiguous rather than silently choosing
  a file.
- Must resolve to a regular file; directories, devices, sockets, FIFOs
  and other non-regular entries are rejected.
- Is read through the root's file descriptor. The resolver never trusts
  a prior size check: it reads at most `max_attachment_bytes + 1` bytes
  and rejects the attachment if the actual byte count exceeds the
  limit. Actual decoded bytes are also counted against
  `max_total_attachment_bytes`.
- Is frozen into bounded in-memory data before returning, so a later
  file change cannot alter a submission.
- Never exposes the configured root or resolved host path to the
  agent. Returned entries contain only a basename and the bounded data.

`Resolve` returns no partial result on error. The resolver supports
multiple attachments and enforces `max_attachments`. Roots are closed by
the account/registry shutdown path; `Close` is idempotent. A missing or
ambiguous path is a typed resolver error.

### Limits

PR 1 uses the existing per-account limits relevant to its work:

- `max_attachment_bytes` — decoded bytes per outbound file.
- `max_total_attachment_bytes` — decoded bytes across all outbound
  files in one resolution.
- `max_attachments` — number of outbound files.
- `max_recipients` — unique envelope recipients.

PR 1 deliberately deferred `max_message_bytes`, because only the
serialized message can be measured accurately. PR 2 now enforces it.

### `list_accounts` compatibility change

PR 1 removes the absolute `attachment_send_paths` array from
`list_accounts` and replaces it with the boolean
`attachment_send_enabled`. The permission list and limits remain. This
intentional wire-format change prevents read-only or unauthorised agents
from learning host filesystem topology; agents only need relative
attachment references. `accounts.View` carries the same boolean and no
root paths.

## PR 2 — Standalone MIME composer (MERGED, PR #11)

PR 2 was squash-merged as
`ab928c3a33ac5b760ca714c6f915fcbfe4366212`. It added
`internal/mailcompose`, a pure, in-memory RFC 5322 / MIME
message-construction component. As merged, that PR did **not**:

- open an SMTP connection;
- register `send_message`;
- resolve attachment paths or perform filesystem I/O;
- mutate mailbox state or create Sent-folder copies;
- add retries, queues or background workers.

### Input and output contract

```go
type Request struct {
    From        string
    To          []string
    Cc          []string
    Bcc         []string
    Subject     string
    Text        string
    HTML        string
    Attachments []attachments.File // frozen resolver output
}

type Result struct {
    Message   []byte // complete serialized RFC 5322 message
    MessageID string // bare Message-ID, without angle brackets
}

func Compose(req Request, limits config.LimitsConfig) (Result, error)
```

`From` is a trusted field: PR 3 populates it from the account's
configured `email`, never from agent input. `To`, `Cc` and `Bcc` are
canonicalised with the same `recipients.Canonical` rules used by PR 1.
`Bcc` is validated but deliberately omitted from every serialized
header; PR 3 uses the validated recipient set to build the SMTP envelope
separately.

The composer accepts no arbitrary header map, raw MIME fragment,
attachment path or separate inline-content channel. Attachment data is
accepted only as already-frozen `attachments.File` values. Tests inject
the clock and Message-ID through an unexported seam; production callers
cannot supply either header.

### Headers

The composer emits:

- `From` — the configured account identity.
- `To` and `Cc` — only when non-empty.
- `Subject` — RFC 2047 encoded when needed.
- `Date` — generated once, UTC.
- `Message-ID` — generated once per composition, from the sender's
  domain plus a 128-bit random nonce.
- `MIME-Version: 1.0`.
- Root and per-part `Content-Type` headers, plus transfer-encoding
  headers on body and attachment parts.

Local-part case is preserved and only the domain is lowercased. CR, LF
and control characters are rejected in user-controlled header fields
and attachment filenames. Bcc never appears in the serialized output,
including on attachment-bearing messages.

### MIME structure

- Plain text only: one `text/plain; charset=utf-8` part, quoted-printable.
- HTML only: one `text/html; charset=utf-8` part, quoted-printable.
- Text and HTML: `multipart/alternative` with `text/plain` first and
  `text/html` second.
- Any attachments: root `multipart/mixed`. The body child is the single
  text part or the `multipart/alternative` container, followed by the
  attachments in request order.

Each attachment carries:

- A `Content-Type` selected from a fixed extension table, falling back
  to `application/octet-stream` for unknown extensions.
- `Content-Disposition: attachment` with RFC 2231 filename encoding.
  Long parameter values use RFC 2231 continuations so header folding
  cannot corrupt the filename.
- `Content-Transfer-Encoding: base64`.
- Exactly the frozen bytes supplied by the caller.

Duplicate basenames remain separate parts. Zero-byte attachments are
valid. Source filesystem paths are never accepted or emitted. The
extension table is in-process rather than `mime.TypeByExtension` so
composition does not read the host MIME database.

### Size limits and errors

The composer consumes `config.LimitsConfig` directly and defends all
limits even when called without the resolver:

- `max_attachment_bytes` — decoded bytes per attachment.
- `max_total_attachment_bytes` — combined decoded attachment bytes.
- `max_attachments` — attachment count.
- `max_message_bytes` — final serialized message size, including all
  headers, MIME boundaries and base64/quoted-printable expansion.

Serialization uses a bounded in-memory writer. Exactly
`max_message_bytes` succeeds; one byte more fails with
`ErrMessageTooLarge`. On any error the zero `Result` is returned, never
partial bytes. No temporary files or disk spooling are used.

`max_message_bytes` is also the budget for header preparation, so a
small serialized limit cannot be bypassed by a huge subject or filename
that expands during RFC 2047/RFC 2231 encoding. Raw subject and filename
bytes, the number of address values and the aggregate raw address input
are checked before encoding; after canonicalisation, aggregate
`From`/`To`/`Cc` header values must fit the same budget. These checks
bound temporary allocations without changing recipient-policy
semantics, and the bounded writer remains authoritative for the exact
final serialized size.

Typed errors distinguish invalid message/header input, invalid
attachment metadata, too many attachments, per-attachment size, total
attachment size, serialized message size, MIME composition failure and
invalid limits. Error text does not echo attachment contents,
credentials or untrusted raw header input. The `send_message` handler
maps these errors to MCP typed errors.

### Tests

The PR 2 test suite parses generated messages independently through
`internal/mimeparse` and `go-message`, and covers plain text, HTML,
alternative, mixed with attachments, MIME nesting/order, base64 binary
round-trips, zero-byte attachments, UTF-8 subjects and filenames,
special-character and long RFC 2231 continuation filenames, duplicate
basenames, header injection, Bcc absence, local-part case,
Date/MIME-Version/Message-ID, exact size boundaries, base64 expansion,
oversized header-preparation inputs under small limits, no partial
results, repeated composition and concurrent composition.

### Known limitations at PR 2 merge

- At PR 2 merge, no SMTP client, `send_message`, reply/forward,
  Sent-folder copy, retry, queue or background worker existed. PR 3 added
  the SMTP client and `send_message`; the remaining limitations still
  apply.
- The composer does not enforce `allowed_recipients` or
  `max_recipients`; the PR 3 sender runs `recipients.Policy`.
- `From` is trusted input. The composer canonicalises it but cannot
  prove that it matches the account configuration.
- The composer does not return an envelope recipient list. The sender
  must assemble `MAIL FROM` / `RCPT TO` from the account identity and
  the policy-validated recipients, including Bcc.
- Attachment content type is inferred from the filename extension using
  a fixed table, not by content sniffing. Unknown extensions use
  `application/octet-stream`.
- Text bodies use quoted-printable, which canonicalises bare line
  endings to CRLF. Attachment bytes are preserved exactly.
- No DKIM, ARC, S/MIME, PGP or other signing exists.
- The Message-ID is generated in memory. A caller that discards the
  prepared bytes and re-composes will receive a new identity.

## PR 3 — SMTP sender and `send_message` (MERGED, PR #12)

PR 3 was squash-merged as
`d1ecf774809681b2795a01f91182eec15a4b2f3f` (reviewed head
`e40bd26fac4e17490652ef0d9712ae24591a5258`) after HAL approval. It added
`internal/smtpclient` and registered the MCP `send_message` tool. It uses
`github.com/emersion/go-smtp` v0.25.0 and `github.com/emersion/go-sasl`
for the SMTP transaction and PLAIN authentication. The sender is a
separate interface; no sending method was added to `mailbox.Backend`.

### Sender architecture

- One short-lived TCP connection per submission. There is no pool,
  queue, background worker, retry loop or provider-specific branch.
- The sender uses go-smtp's low-level `Mail`, `Rcpt`, `Data` and
  `CloseWithResponse` primitives, not the convenience `SendMail` API.
- Requests are validated and credentials are resolved before dialing.
  `Send` then performs exactly one transaction: connect, TLS,
  authenticate, `MAIL FROM`, every approved unique `RCPT TO`, `DATA`,
  write the complete prepared message, finalize `DATA`, read the final
  response, and close.
- `RCPT TO` is issued for every unique envelope recipient, including
  Bcc recipients. DATA is issued only after every recipient is accepted.
  Any rejection aborts before DATA; RSET is attempted as best-effort
  cleanup. There is no partial-recipient send.
- The complete serialized MIME message from PR 2 is written unchanged.
  The tool never reparses or regenerates it after composition.
- No automatic retries and no SMTP pipelining are used.

### TLS and authentication

- Exactly one TLS mode is supported: implicit TLS (`tls: true`) or
  mandatory STARTTLS (`starttls: true`).
- Certificates and hostnames are verified against the configured SMTP
  host using `ServerName`; TLS 1.2 or newer is required. Verification
  cannot be disabled from configuration.
- There is no plaintext fallback. The client requires a completed,
  verified TLS handshake before authentication or any submission
  command.
- Submission requires AUTH PLAIN. Servers that do not advertise PLAIN
  are rejected before credentials are sent.
- Both `password` and `password_command` are supported. Credential
  commands run directly without a shell, with a bounded context and
  bounded stdout; their output is never included in errors or logs.
- SMTP envelope addresses must be ASCII. SMTPUTF8 is not requested and
  non-ASCII envelope addresses are rejected before dialing.

### `send_message` contract

Input fields:

```json
{
  "account_id": "work",
  "to": ["alice@example.test"],
  "cc": [],
  "bcc": ["audit@example.test"],
  "subject": "Report",
  "text": "Plain body",
  "html": "<p>HTML body</p>",
  "attachments": ["reports/q3.pdf"]
}
```

There is no caller-supplied `from`, header map, raw MIME field,
credential field, SMTP command or absolute-path field. Attachment
references are resolved only by the confined `internal/attachments`
resolver; absolute paths, traversal, symlink escapes, ambiguous roots
and non-regular files are rejected.

Successful output contains:

```json
{
  "account_id": "work",
  "message_id": "generated@example.test",
  "status": "submitted",
  "recipient_count": 2,
  "attachment_count": 1,
  "total_attachment_bytes": 123456
}
```

`submitted` means the SMTP server explicitly accepted the message after
DATA. It does **not** prove final mailbox delivery to each recipient.
No Sent-folder copy is made.

### Mandatory preflight order

The handler performs these steps in order, before any SMTP connection:

1. Look up `account_id`.
2. Require the `send` capability.
3. Bound the raw From/To/Cc/Bcc entry count and aggregate bytes against
   `max_message_bytes`, before parsing any address.
4. Canonicalize the configured `From` and all To/Cc/Bcc addresses.
5. Apply the exact recipient allow-list and unique-recipient limit.
6. Reject any non-ASCII envelope address; SMTPUTF8 is not supported.
7. Require `attachment_send` when attachments are present.
8. Resolve and freeze attachments through the confined resolver.
9. Compose the complete serialized RFC 5322/MIME message.
10. Enforce `max_message_bytes` on the final serialized bytes.
11. Retain the generated Message-ID and immutable bytes for submission.

Any preflight error means zero SMTP connections and zero submission
attempts. The sender also validates the final request and resolves
credentials before dialing as defence in depth.

### Outcome classification

- **Confirmed submission:** only after an explicit final 2xx response
  after DATA. The result contains the original Message-ID.
- **Definite failure:** account/permission/validation errors, attachment
  errors, message-size errors, TLS/auth/connection failures before DATA,
  MAIL FROM rejection, any RCPT TO rejection, DATA command rejection,
  or an explicit final 4xx/5xx DATA response. No retry is attempted.
- **Ambiguous failure:** message-data transmission began, but the
  connection failed, timed out, was cancelled or otherwise ended without
  a definitive final response. `SmtpAmbiguousFailure` includes the
  original `message_id` and `may_have_been_sent: true`. The message may
  have been accepted; it is never reported as definite non-delivery and
  is never retried automatically. A new send would create a new
  Message-ID, so callers must verify rather than blindly resubmit.

There is no prepared-message replay API. Because PR 3 has no durable
spool, an ambiguous message cannot be replayed byte-for-byte through the
tool after the call returns.

### Bounds and audit

Every network stage has a finite internal deadline: DNS/TCP connect,
TLS handshake, greeting/EHLO, STARTTLS, authentication, MAIL, RCPT,
message-data writing and the final response. MCP request cancellation is
propagated by closing the active connection.

Audit logging is structured and contains only the account ID,
Message-ID, recipient count, attachment count, total attachment bytes,
outcome category and a typed error code. It never contains bodies,
attachment contents, credentials, raw SMTP transcripts or full
recipient lists.

### Tests

The PR 3 suite includes in-process SMTP servers with controlled fault
injection. Coverage includes:

- successful implicit-TLS and STARTTLS authenticated submissions;
- Bcc present in the SMTP envelope but absent from serialized headers;
- independent MIME reparsing of transmitted text, HTML and attachments;
- no SMTP connection for recipient, permission, attachment, size and
  other preflight failures;
- early oversized recipient-input rejection before address parsing,
  including large repeated lists, aggregate To/Cc/Bcc input, a typed
  `MessageTooLarge` error, zero sender calls and bounded allocation;
- duplicate collapsing, local-part case sensitivity, domain case
  insensitivity and unique-recipient counting;
- invalid certificates, wrong hostnames, missing/refused STARTTLS and
  authentication rejection, with no credentials sent without verified
  TLS;
- MAIL FROM rejection and first/later RCPT rejection with no DATA;
- explicit DATA-command and final DATA rejection;
- disconnect, final-response timeout and cancellation during/after
  DATA, all classified as ambiguous with the original Message-ID;
- exactly one SMTP attempt and no retry;
- connection, TLS-handshake, command, data-write and final-response
  timeouts;
- `password_command` success, failure, output bounding and cancellation;
- SMTPUTF8 rejection before dialing; and
- concurrent submissions for multiple accounts, including `-race`.

## PR 4 — Documentation closeout (MERGED, PR #13)

The Phase 2.3 documentation closeout was documentation-only and was
squash-merged as
`a4851e9159af63026e5593f30988c114d7117261`. It did not change sender
behavior, the configuration schema, Go production code, dependencies or
tests. It:

- records the exact squash commits for PRs #10, #11 and #12;
- reconciles the README, architecture, threat model, tool contract,
  design notes and example configuration with the merged implementation;
- adds the operator runbook in
  [`LIVE_SMTP_SMOKE_TEST.md`](LIVE_SMTP_SMOKE_TEST.md); at PR #13 merge
  time it was a pre-execution checklist.

Controlled external SMTP testing remained a separate operational
activity requiring explicit authorization. It was executed on 2026-10-21
after that authorization; the runbook now records the results. This
follow-up documentation correction changes no code, configuration or
provider state.

### Live smoke-test evidence (2026-10-21)

The Purelymail campaign passed. The authoritative execution record and
the full qualification are in
[`LIVE_SMTP_SMOKE_TEST.md`](LIVE_SMTP_SMOKE_TEST.md); this is a concise
cross-reference, not a second test report.

- **Live submissions and received-message verification:** authenticated
  SMTP over TLS 1.3; plain-text delivery; multipart/alternative MIME;
  attachment delivery with JPEG SHA-256 verification; Bcc delivery
  without visible-header disclosure; returned Message-ID present in the
  received message; IMAP reply-threading interoperability.
- **Offline negative controls:** allowlist rejection and confined
  attachment traversal rejection returned the expected typed errors,
  made zero sender calls and produced no live submission. These were
  preflight security controls, not live SMTP transactions.
- **Non-ASCII qualification:** non-ASCII subject delivery passed live;
  non-ASCII body composition was verified offline only; delivered
  multipart bodies were ASCII-only.
- **Qualification:** the campaign is interoperability/smoke-test
  evidence. It is not exhaustive security testing, provider-wide
  certification or production certification. It does not cover
  OAuth2-only providers.

## Explicit limitations and non-goals

- SMTP acceptance is not guaranteed final mailbox delivery.
- No reply or forward.
- No mailbox mutation, deletion or automatic Sent-folder copy.
- No queues, retries, durable spooling, background sending or
  prepared-message replay API.
- No provider-specific adapters, provider administration or provider
  certification.
- No OAuth2, DKIM, ARC, S/MIME or PGP.
- No arbitrary MIME, caller-supplied headers, caller-selected From,
  raw SMTP commands, credentials or filesystem-root access.
- No plaintext SMTP, disabled certificate verification or SMTPUTF8
  envelope addresses.
- No unrelated IMAP hardening.
- No separate `app_password` field. Provider-issued app passwords are
  supplied through `password` or `password_command`; IMAP and SMTP have
  independent credentials. IMAP uses `LOGIN` over verified TLS and SMTP
  uses AUTH PLAIN after verified TLS. OAuth2-only providers remain
  unsupported.
- The Purelymail smoke test passed on 2026-10-21, but production
  hardening is not complete and TheMailroom is not production-ready.
- **Residual risks:** `max_message_bytes` does not cap the initial
  JSON-RPC allocation of `text`, `html` or attachment arrays; the
  pre-existing IMAP `password_command` path uses unbounded stdout/stderr
  buffers and includes truncated stderr in internal errors; and
  failure-outcome audit categories are not fully unit-tested. These are
  Phase 3 hardening items, not production-code changes in this
  documentation closeout.
