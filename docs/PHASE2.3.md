# Phase 2.3 Plan — Outbound submission foundations

> **Status:** PR 1 is **IN REVIEW**. TheMailroom is a development
> milestone, not a production-hardened release. This document records
> the approved Phase 2.3 scope and the controls landed by PR 1. It
> does **not** claim that SMTP sending or MIME composition is
> implemented.

## Scope

Phase 2.3 prepares the outbound path while keeping the server unable to
send until the later PRs land. The full milestone covers:

- A separate sender abstraction and SMTP submission client.
- RFC 5322 / MIME composition, including text and HTML bodies.
- Multiple outbound attachments with a fixed `From` identity.
- Bounded resource use and typed errors at the tool boundary.

Provider/domain/account administration APIs, reply/forward, and
mailbox-mutating operations are out of scope for this milestone.

## PR 1 — Outbound security foundations (IN REVIEW)

PR 1 is deliberately additive and does not register `send_message`,
create an SMTP client, or compose MIME. It provides:

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

No SMTP client is instantiated in PR 1. The future sender must use the
account's configured `email` as the fixed sender identity.

### Recipient policy

`allowed_recipients` is an optional per-account allow-list.
`limits.max_recipients` defaults to 50 and counts unique envelope
recipients across To, Cc and Bcc together.

The reusable policy in `internal/recipients`:

- Parses addresses with `net/mail`; display names are accepted but
  discarded for envelope matching.
- Rejects empty, malformed and control-character-containing addresses.
- Canonicalises to a lowercase addr-spec and compares the whole address
  exactly. Matching is case-insensitive; there is no substring,
  prefix, suffix or wildcard matching.
- Collapses duplicate canonical addresses before enforcing the limit.
- Rejects a message with no recipients when validation is invoked.

The policy is fully testable without SMTP. The future `send_message`
handler must call it before opening a connection.

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

`max_message_bytes` remains the eventual serialized RFC 5322 limit. The
future MIME composer, not this PR, must enforce it because only the
composer knows the final transfer-encoded size.

### `list_accounts` compatibility change

PR 1 removes the absolute `attachment_send_paths` array from
`list_accounts` and replaces it with the boolean
`attachment_send_enabled`. The permission list and limits remain. This
intentional wire-format change prevents read-only or unauthorised agents
from learning host filesystem topology; agents only need relative
attachment references. `accounts.View` carries the same boolean and no
root paths.

## PR 2 — SMTP sender and `send_message` (planned)

- Add the selected `github.com/emersion/go-smtp` client (after HAL
  approval of the dependency).
- Compose text/plain and text/html messages with `go-message`.
- Resolve and freeze path attachments through `internal/attachments`.
- Enforce `max_message_bytes` on the serialized message.
- Use a single `MAIL FROM` / `RCPT TO` / `DATA` cycle; no automatic
  retry and no automatic Sent-folder copy.
- Return typed errors for validation, SMTP, timeout, and ambiguous
  post-DATA failures.

## PR 3 — Documentation and closeout (planned)

- Document the final tool contract and configuration.
- Reconcile the threat model and architecture with the implemented
  controls.
- Run the full verification matrix and record the evidence.

## Explicit non-goals for PR 1

- No SMTP connection or message submission.
- No MIME composer.
- No `send_message` registration.
- No reply/forward path.
- No mailbox mutation.
- No generic filesystem service or directory enumeration.
