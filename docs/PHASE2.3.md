# Phase 2.3 Plan — Outbound submission foundations

> **Status:** PR 1 (outbound security foundations) is **MERGED**
> (PR #10, squash commit `bf3618f`). PR 2 (standalone MIME composer) is
> **IN REVIEW**. SMTP submission, `send_message`, reply/forward and
> mailbox mutation remain unimplemented. TheMailroom is a development
> milestone, not a production-hardened release. Phase 2.3 is **not**
> done.

## Scope

Phase 2.3 prepares the outbound path while keeping the server unable to
send until the later PRs land. The full milestone covers:

- RFC 5322 / MIME composition, including text and HTML bodies.
- Multiple outbound attachments with a fixed `From` identity.
- A separate sender abstraction and SMTP submission client.
- Bounded resource use and typed errors at the tool boundary.

Provider/domain/account administration APIs, reply/forward, and
mailbox-mutating operations are out of scope for this milestone.

## PR 1 — Outbound security foundations (MERGED, PR #10)

PR 1 was additive and did not register `send_message`, create an SMTP
client, or compose MIME. It provides:

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

No SMTP client is instantiated. The future sender must use the
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

The future `send_message` handler must call this policy before opening a
connection. The composer in PR 2 only canonicalises addresses; it does
not authorise them.

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
  file change cannot alter a future submission.
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

## PR 2 — Standalone MIME composer (IN REVIEW)

PR 2 adds `internal/mailcompose`, a pure, in-memory RFC 5322 / MIME
message-construction component. It does **not**:

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

`From` is a trusted field: the future sender must populate it from the
account's configured `email`, never from agent input. `To`, `Cc` and
`Bcc` are canonicalised with the same `recipients.Canonical` rules used
by PR 1. `Bcc` is validated but deliberately omitted from every
serialized header; the future sender must use the validated recipient
set to build the SMTP envelope separately.

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
credentials or untrusted raw header input. These errors are suitable
for mapping to MCP typed errors when a sender is added.

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

### Known limitations

- No SMTP client, `send_message`, reply/forward, Sent-folder copy,
  retry, queue or background worker exists.
- The composer does not enforce `allowed_recipients` or
  `max_recipients`; the future sender must run `recipients.Policy`.
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

## PR 3 — SMTP sender and `send_message` (planned)

- Add the selected `github.com/emersion/go-smtp` client after HAL
  approval of the dependency.
- Resolve and freeze path attachments through `internal/attachments`.
- Enforce the recipient policy before opening SMTP.
- Compose with `internal/mailcompose`, send the returned bytes, and
  report the returned Message-ID.
- Use a single `MAIL FROM` / `RCPT TO` / `DATA` cycle; no automatic
  retry and no automatic Sent-folder copy.
- Return typed errors for validation, SMTP, timeout, and ambiguous
  post-DATA failures.

## PR 4 — Documentation and closeout (planned)

- Document the final tool contract and configuration.
- Reconcile the threat model and architecture with the implemented
  controls.
- Run the full verification matrix and record the evidence.
- Mark Phase 2.3 done only after the sender and closeout land.

## Explicit non-goals for PR 2

- No SMTP client or connection.
- No `send_message` registration.
- No reply or forward.
- No mailbox mutation or Sent-folder copy.
- No queues, retries or background workers.
- No provider-specific adapters.
- No arbitrary filesystem paths, separate inline-content channel or
  arbitrary MIME headers; attachment bytes arrive only as frozen
  `attachments.File` values.
- No new dependencies.
