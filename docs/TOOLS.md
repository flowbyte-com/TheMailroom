# TheMailroom Tools

> Status: Phase 2.3 implementation and documentation closeout are
> complete on `main`. PR 1 is **MERGED** (PR #10,
> `bf3618f247928ea1f1d2c687aac1cffd96a89070`), PR 2 is **MERGED**
> (PR #11, `ab928c3a33ac5b760ca714c6f915fcbfe4366212`), PR 3 is
> **MERGED** (PR #12,
> `d1ecf774809681b2795a01f91182eec15a4b2f3f`), and PR 4 is **MERGED**
> (PR #13, `a4851e9159af63026e5593f30988c114d7117261`). The read-only
> tools below and `send_message` are implemented. Reply/forward, mailbox
> modification and deletion remain future work. The controlled
> Purelymail smoke-test campaign was executed on 2026-10-21 and passed;
> the authoritative record is
> [`LIVE_SMTP_SMOKE_TEST.md`](LIVE_SMTP_SMOKE_TEST.md). It is
> interoperability/smoke-test evidence, not production certification.
> Production hardening is not complete and TheMailroom is not a
> production-hardened release.

Every tool:

- Validates input against a JSON Schema.
- Returns a structured JSON result.
- Enforces a permission capability server-side for account-scoped operations.
- Returns typed errors without leaking credentials or wire details. Unknown internal failures map to `InternalError`. `BackendError` is reserved for future protocol mapping and is not currently emitted.

All account-scoped tools take `account_id` as a required string field. There is no implicit "current account"; `list_accounts` is the only account metadata tool.

**Error channels.** A tool call can fail in three distinct ways, and
clients must handle all three.

1. **SDK-generated argument errors.** A schema validation failure or an
   argument unmarshal failure is rejected by the MCP SDK before the tool
   handler runs. The SDK returns a normal JSON-RPC *result* whose `isError`
   is `true` and whose `content` holds one human-readable text block
   describing the violation. The SDK's `SetError` does not populate
   `structuredContent`, so a client must not depend on that field for these
   errors; `isError` and `content` are the portable fields.
2. **TheMailroom business errors.** The tool handlers produce the typed
   error model documented under [Common error model](#common-error-model)
   below — a `code`, a `message` and optional `details` — for an unknown
   account, a missing capability, invalid input, and backend or SMTP
   failures. The SDK converts a handler error into the same tool execution
   error shape as case 1: a result with `isError: true` and the error
   message in `content`. The typed `code` is recorded in the server-side
   audit log, for example the `send_message` outcome record; the client
   payload carries the message text.
3. **Protocol errors.** A malformed protocol request, a missing or
   unparseable `params` object, and an unknown tool name are JSON-RPC
   *errors*, not results. An unknown tool name uses code `-32602`; a
   request with no `params` uses `-32600`. A `params` object that cannot be
   parsed into a call-tool request is also a JSON-RPC error. No `isError`
   flag and no `content` are present in any of these.

A tool execution error therefore does not carry `structuredContent` today.
`structuredContent` appears only on a successful call, where it holds the
tool's typed output. The portable way to read any tool execution error is
`isError` plus the text in `content`. This split follows the MCP
specification and is pinned by `TestBoundedServerToolValidationErrorShape`.

## Common error model

```json
{
  "error": {
    "code": "PermissionDenied",
    "message": "account 'personal' lacks capability 'send'",
    "details": { "account_id": "personal", "capability": "send" }
  }
}
```

`code` is one of:

- `InvalidInput` — schema or business validation failed.
- `AccountNotFound` — `account_id` not configured.
- `MailboxNotFound` — the requested mailbox does not exist.
- `MessageNotFound` — the requested UID does not exist in the mailbox.
- `PermissionDenied` — capability not granted.
- `RateLimited` — limit exceeded.
- `UidMismatch` — `uid_validity` does not match the current mailbox.
- `NotImplemented` — capability exists in the schema but is not yet wired.
- `InternalError` — an unexpected internal failure; details are suppressed.
- `MessageTooLarge` — raw From/To/Cc/Bcc input or the serialized message
  exceeds `max_message_bytes`, or an ordinary body part exceeds the
  per-part cap during `read_attachment` validation.
- `NestingTooDeep` — the MIME nesting exceeds `max_mime_nesting`.
- `TooManyParts` — the MIME part count exceeds `max_mime_parts`.
- `TooManyAttachments` — the attachment count exceeds `max_attachments`.
- `AttachmentTooLarge` — a single attachment exceeds `max_attachment_bytes`,
  or the decoded total exceeds `max_total_attachment_bytes`.
- `AttachmentNotFound` — the requested part is not an exposed attachment.
- `Malformed` — the message could not be parsed, or attachment
  extraction could not complete because a part or transfer encoding
  could not be decoded.
- `RecipientNotAllowed` — an outbound recipient is outside the
  account's exact `allowed_recipients` policy.
- `TooManyRecipients` — unique To + Cc + Bcc recipients exceed
  `max_recipients`.
- `SmtpAuthFailed` — SMTP credential resolution failed, the server does
  not advertise AUTH PLAIN, or authentication was rejected.
- `SmtpTLSFailed` — verified TLS could not be established, including a
  missing or refused mandatory STARTTLS.
- `SenderRejected` — the SMTP server rejected `MAIL FROM`.
- `RecipientRejected` — the SMTP server rejected a `RCPT TO`. No
  message data was sent.
- `SmtpMessageRejected` — the server rejected the DATA command or
  returned an explicit final 4xx/5xx response after DATA.
- `SmtpConnectionFailed` — a pre-DATA DNS/TCP/connection or SMTP
  command failure occurred for a reason other than timeout/cancellation.
- `SmtpTimeout` — a submission stage timed out or was cancelled before
  message data began. After DATA begins, an uncertain outcome is
  reported as `SmtpAmbiguousFailure` instead.
- `SmtpAmbiguousFailure` — message-data transmission began, but no
  definitive final response was received. The error carries
  `details.message_id` (the original Message-ID) and
  `details.may_have_been_sent: true`; the same identity and flag appear
  in the error text. The message may have been accepted and must not be
  retried automatically.

## Sending tools

### `send_message`

Submits a new message through one account's authenticated SMTP server.
It does not reply, forward, mutate a mailbox, create a Sent copy, or
retry an uncertain submission.

**Required capabilities:** `send`. If any attachment is present,
`attachment_send` is also required, and the account must have a
configured `attachment_send_paths` root.

**Input.**

```json
{
  "account_id": "work",
  "to": ["alice@example.test"],
  "cc": ["carol@example.test"],
  "bcc": ["audit@example.test"],
  "subject": "Quarterly report",
  "text": "Plain body",
  "html": "<p>HTML body</p>",
  "attachments": ["reports/q3.pdf"]
}
```

`account_id` is required. At least one To/Cc/Bcc recipient is required
after canonicalisation and duplicate collapsing. `attachments` entries
are relative paths understood only by the account's confined resolver;
absolute paths, traversal, symlink escapes, non-regular files and
ambiguous roots are rejected.

There is deliberately no caller-controlled `from`, header map, raw MIME
field, credential field, SMTP command, or filesystem-root field. Unknown
input properties are rejected by the JSON Schema. `From` is always the
account's configured email identity. `Bcc` addresses go into the SMTP
envelope but never appear in serialized MIME headers.

**Output.**

```json
{
  "account_id": "work",
  "message_id": "generated@example.test",
  "status": "submitted",
  "recipient_count": 3,
  "attachment_count": 1,
  "total_attachment_bytes": 123456
}
```

`message_id` is the bare Message-ID embedded in the prepared message.
`recipient_count` is the number of unique envelope recipients across
To, Cc and Bcc. `status: "submitted"` is returned **only after the SMTP
server explicitly accepted the message after DATA**. SMTP acceptance is
not proof of final mailbox delivery to every recipient.

**Preflight and authorization.** The handler performs all of the
following before opening any SMTP connection:

1. Look up `account_id`.
2. Require `send`.
3. Bound the raw From/To/Cc/Bcc entry count and aggregate bytes against
   `max_message_bytes`, before parsing any address.
4. Canonicalise the configured `From`, To, Cc and Bcc.
5. Enforce the exact recipient allow-list, at least one recipient, and
   the unique `max_recipients` limit.
6. Reject any non-ASCII envelope address; SMTPUTF8 is unsupported.
7. Require `attachment_send` when attachments are present.
8. Resolve and freeze attachments inside the account's configured
   roots.
9. Compose the complete RFC 5322/MIME message with the account identity
   as `From`.
10. Enforce `max_message_bytes` on the final serialized message.
11. Retain that Message-ID and those bytes for the sender.

Any preflight error means no SMTP connection or submission attempt.

**Outbound controls (enforced by `send_message`):**

- `max_recipients` — caps unique outbound To + Cc + Bcc recipients
  (default 50). Duplicates across To/Cc/Bcc collapse before the count is
  checked.
- `allowed_recipients` — optional exact allow-list. An empty list permits
  any canonical recipient.
- `max_attachment_bytes` / `max_total_attachment_bytes` /
  `max_attachments` — bound frozen attachments resolved by
  `internal/attachments`.
- `max_message_bytes` — bounds raw From/To/Cc/Bcc entry count and
  aggregate bytes before address parsing, is enforced by
  `internal/mailcompose` on header preparation and the final serialized
  message, and is re-checked by the tool before SMTP.

**SMTP transport.** Exactly one short-lived connection is used per call;
there is no pooling, queue, background worker or retry. The client
supports implicit TLS or mandatory STARTTLS, verifies the configured
hostname and certificate, requires TLS 1.2 or newer, and never falls
back to plaintext or disabled verification. Authentication is AUTH
PLAIN only and happens only after verified TLS. Envelope addresses must
be ASCII; SMTPUTF8 is not requested. Both `password` and
`password_command` are supported for SMTP credentials; command execution
is direct (no shell), bounded and cancellable.

Provider-issued app passwords are accepted as ordinary opaque password
values through `password` or `password_command`; there is no separate
`app_password` field. IMAP and SMTP credentials are independent and may
use different usernames, passwords or commands. IMAP uses `LOGIN` over
verified TLS and SMTP uses AUTH PLAIN after verified TLS. OAuth2-only
providers, IMAP servers that require `AUTHENTICATE` instead of `LOGIN`,
and SMTP servers that do not advertise AUTH PLAIN are unsupported. A
literal `password` remains plaintext at rest; prefer `password_command`
and restrict the config file.

**Transaction and outcomes.** After authentication the client issues
`MAIL FROM`, then one `RCPT TO` for every unique policy-approved
envelope recipient. DATA is issued only if every recipient was
accepted. Any MAIL or RCPT rejection is a definite failure and no
message data is sent; RSET is attempted after a rejected recipient.

- An explicit final 2xx response after DATA is the only success path.
- TLS, authentication, connection, MAIL, RCPT, DATA-command and
  pre-data timeout/cancellation failures are definite failures.
- An explicit final 4xx/5xx response after DATA is a definite
  rejection.
- A lost connection, timeout or cancellation after DATA begins but
  before a definitive response returns `SmtpAmbiguousFailure` with the
  original Message-ID and `may_have_been_sent: true`. No retry is made.
  Because there is no prepared-message replay API, the caller must
  verify server state before composing a new message.

All network stages have finite internal deadlines: DNS/TCP connect, TLS
handshake, greeting/EHLO, STARTTLS, authentication, MAIL, RCPT, DATA
writing and the final response. MCP request cancellation closes the
active connection. The tool does not return raw SMTP transcripts or
server response text.

**Audit logging.** Structured audit logs may contain the account ID,
Message-ID, recipient count, attachment count, total attachment bytes,
outcome category and typed error code. They never contain message
bodies, attachment contents, credentials, raw SMTP transcripts or the
full recipient list.

**Known residual risks (Phase 3 hardening).** `server.max_request_bytes`
caps one inbound MCP JSON-RPC frame before decoding (default 32 MiB,
hard maximum 128 MiB; LF/CRLF framing excluded). It is a per-frame
allocation boundary, not an aggregate process-memory limit: the SDK
handler queue, concurrent in-flight requests, tool-level argument
amplification and response sizes remain separate concerns. Account
`max_message_bytes` still governs email composition and post-decode
preprocessing. The IMAP `password_command` path bounds stdout to 8 KiB,
discards stderr, and bounds the run by a timeout and by the caller's
cancellation; neither its stdout nor its stderr reaches an error, a log or
a tool response. The external command itself remains an operator-supplied
executable and is not made safe by these bounds. Failure-outcome
audit categories are not fully unit-tested. These do not change the
documented Phase 2.3 acceptance results but are recorded in
[`THREAT_MODEL.md`](THREAT_MODEL.md).

The controlled external smoke test for this tool was executed against
Purelymail on 2026-10-21 and passed. The authoritative results and
qualifications are in
[`LIVE_SMTP_SMOKE_TEST.md`](LIVE_SMTP_SMOKE_TEST.md). The campaign is
interoperability/smoke-test evidence, not production certification, and
it does not authorize further live email.

## Read-only tools

### `list_accounts`

Lists the IDs and metadata of configured accounts. **No credentials or
absolute host filesystem paths** are returned.

**Input.**

```json
{ }
```

**Output.**

```json
{
  "accounts": [
    {
      "id": "personal",
      "email": "user@example.test",
      "provider": "generic",
      "permissions": ["read"],
      "limits": {
        "max_message_bytes": 26214400,
        "max_attachment_bytes": 6291456,
        "max_total_attachment_bytes": 26214400,
        "max_attachments": 50,
        "max_recipients": 50,
        "max_search_results": 500,
        "max_batch_operations": 50,
        "max_list_scan_messages": 10000,
        "max_mime_nesting": 16,
        "max_mime_parts": 500
      },
      "attachment_send_enabled": false
    }
  ]
}
```

### `list_mailboxes`

Lists mailboxes/folders for an account.

**Input.**

```json
{ "account_id": "personal" }
```

**Output.**

```json
{
  "account_id": "personal",
  "mailboxes": [
    { "name": "INBOX", "attributes": ["\\HasNoChildren"], "delimiter": "/" },
    { "name": "Sent", "attributes": ["\\Sent"], "delimiter": "/" },
    { "name": "Trash", "attributes": ["\\Trash"], "delimiter": "/" }
  ]
}
```

### `list_messages`

Lists messages in a mailbox.

**Input.**

```json
{
  "account_id": "personal",
  "mailbox": "INBOX",
  "limit": 50,
  "offset": 0,
  "since": "2024-01-01T00:00:00Z",
  "before": "2024-12-31T23:59:59Z"
}
```

`limit` is clamped to the per-account `max_batch_operations` (default 50). `offset` is validated non-negative. `since` / `before` are RFC 3339 timestamps.

**Scan budget.** Results are ordered by internal date, newest first, with
a descending UID tie-break. IMAP UIDs ascend but are unrelated to internal
dates, so an exact page can only be produced by examining every message in
the mailbox. The per-account `max_list_scan_messages` limit (default
10 000, hard maximum 1 000 000) bounds how many messages one call may
examine. The bound is enforced twice: the mailbox size the server reports
in the EXAMINE response is checked before any FETCH is sent, and the
messages the server actually delivers are counted during the fetch, so a
server that under-reports its size is caught while the listing runs. A
mailbox over the limit is refused with the `RateLimited` error code and a
message naming the budget and the recourse; no partial or approximate page
is ever returned. The `since` / `before` filters are applied by the client
after the fetch, so they narrow the result but not the work: a filtered
query over an oversized mailbox is still refused. They are therefore not
offered as a recourse — the recourse is to raise
`max_list_scan_messages`, subject to its hard maximum.

**Output.**

```json
{
  "account_id": "personal",
  "mailbox": "INBOX",
  "uid_validity": 1234567890,
  "messages": [
    {
      "uid": 1024,
      "flags": ["\\Seen"],
      "internal_date": "2024-08-15T10:11:12Z",
      "size": 12345,
      "from": "Sender <sender@example.test>",
      "to": ["user@example.test"],
      "subject": "Quarterly report",
      "has_attachments": true
    }
  ]
}
```

`uid_validity` is the live UIDVALIDITY of the selected mailbox. The
IMAP backend issues a read-only `EXAMINE` for the call, so the value
is authoritative even after reconnects.

### `read_message`

Reads a single message, returning headers, text and HTML bodies, and
a list of attachments. Bodies are truncated by `max_message_bytes`.

**Input.**

```json
{
  "account_id": "personal",
  "mailbox": "INBOX",
  "uid": 1024,
  "uid_validity": 1234567890
}
```

`uid_validity` is **required** in Phase 2.1. Omitting it returns
`InvalidInput`; passing a stale value (e.g. one observed before the
mailbox was re-created) returns `UidMismatch` without reading any
body. This prevents silent targeting of recycled UIDs after a
mailbox UIDVALIDITY rollover.

**Output.**

```json
{
  "account_id": "personal",
  "mailbox": "INBOX",
  "uid": 1024,
  "uid_validity": 1234567890,
  "flags": ["\\Seen"],
  "headers": {
    "message_id": "<abc@example.test>",
    "from": "Sender <sender@example.test>",
    "to": ["user@example.test"],
    "cc": [],
    "subject": "Quarterly report",
    "date": "2024-08-15T10:11:12Z",
    "in_reply_to": null,
    "references": []
  },
  "text": "Plain text body...",
  "html": "<p>HTML body...</p>",
  "attachments": [
    {
      "index": 0,
      "part": "2",
      "filename": "report.pdf",
      "mime_type": "application/pdf",
      "size": 123456,
      "content_id": null,
      "disposition": "attachment"
    }
  ],
  "warnings": [],
  "truncated": false
}
```

Bodies are returned as separate fields. The agent is responsible for
any rendering. The server never concatenates bodies into a
"natural language" string that could be confused with instructions.

**Attachment metadata.** Each attachment entry is metadata only.
`part` is the IMAP-style 1-indexed path (e.g. `"1.2"` for the
second part of a `multipart/related` nested inside the root).
`content_id` is the value of the `Content-ID` header for inline
references. `disposition` is `attachment` or `inline`. Attachment
bodies are not materialised by `read_message`; use
`read_attachment` with the returned `part` to request one
explicitly. Listing or reading a message never triggers an
automatic attachment fetch.

**Read-only IMAP guarantees.**

- The IMAP backend issues `EXAMINE` (read-only `SELECT`) for every
  read. The server-side state cannot accept `STORE` or `APPEND`.
- The body is fetched with `UID FETCH BODY.PEEK[]`, which never
  implicitly sets `\Seen`. A message that the server reports as
  `\Unseen` remains `\Unseen` after `read_message`.
- On connection error the client reconnects; the next call
  re-`EXAMINE`s and re-validates UIDVALIDITY before acting. A
  mutating operation is never silently retried.

**IMAP literal memory: bounded at the protocol layer.**

The `max_message_bytes` cap is enforced by the go-imap fork
(`github.com/flowbyte-com/go-imap` v1.2.1-themailroom.4) at the
protocol reader level. `client.DialOptions.MaxLiteralSize` is set
to `max_message_bytes` at dial time; `ReadLiteral` checks the
server-announced literal size **before** allocating the literal
buffer. An oversized announcement is therefore rejected without an
allocation proportional to the announced size. The application
check in the backend remains as defence in depth.

The cap bounds each literal. It does not bound the number of
literals a server may stream over a long-lived connection; the MIME
part-count, attachment-count and batch limits remain the backstops.
See `docs/PHASE2_DESIGN.md` §8 and §10 for the full analysis.

**MIME safety limits** (per account):

- `max_message_bytes` — caps the raw RFC 5322 size (default 25 MiB).
- `max_attachment_bytes` — caps a single attachment (default 6 MiB).
- `max_total_attachment_bytes` — caps the sum of attachments (default 25 MiB).
- `max_attachments` — caps the number of exposed attachments (default 50).
- `max_mime_nesting` — caps multipart depth (default 16).
- `max_mime_parts` — caps the total number of parts visited (default 500).

Violations return a typed error (`MessageTooLarge`, `AttachmentTooLarge`,
`NestingTooDeep`, `TooManyParts`, `TooManyAttachments`) without reading
the rest of the message.

### `read_attachment`

Reads exactly one attachment selected by the part path returned by
`read_message`. The tool does not list or discover attachments; it
never fetches on its own during listing or message reading.

**Input.**

```json
{
  "account_id": "personal",
  "mailbox": "INBOX",
  "uid": 1024,
  "uid_validity": 1234567890,
  "part": "2"
}
```

`uid_validity` and the live mailbox binding have the same contract as
`read_message`: a stale value returns `UidMismatch` and a zero value
returns `InvalidInput`. `part` is validated for length, depth,
numeric components and the per-account MIME part limit before any
backend call. It is a reference into the parsed message, never a
filesystem path or an IMAP command fragment.

**Output.**

```json
{
  "account_id": "personal",
  "mailbox": "INBOX",
  "uid": 1024,
  "uid_validity": 1234567890,
  "part": "2",
  "filename": "report.pdf",
  "mime_type": "application/pdf",
  "size": 123456,
  "content_base64": "JVBERi0xLjQK...",
  "warnings": []
}
```

`size` is the decoded content length. `content_base64` is standard,
padded RFC 4648 base64. There is no `truncated` field: content that
exceeds a limit is an error, never a partial success. Base64 expands
the payload by roughly one third; a default maximum-size attachment
(6 MiB) can therefore produce an approximately 8 MiB tool response.

**Content and safety.**

- The part must be an exposed attachment from `read_message`:
  attachment disposition, inline part, or the opaque outer
  `message/rfc822` attachment. Multipart containers, ordinary body
  parts, missing parts and descendants hidden inside an opaque
  `message/rfc822` attachment return `AttachmentNotFound`.
- The complete message is parsed and all configured MIME and size
  limits are applied, including siblings of the requested part. If a
  later part fails a limit, no content is returned.
- Extraction is fail-closed. A malformed sibling, a body read or
  transfer-decoding failure, an unknown transfer encoding, or an
  oversized ordinary body part returns `Malformed` or
  `MessageTooLarge`; previously captured content is discarded.
- Transfer encodings are decoded. Text parts are charset-decoded to
  UTF-8 where possible, so text attachment bytes may be normalised
  rather than byte-identical to the sender's original file. Binary
  transfer-encoded content is decoded to its original bytes. A
  charset warning on an unrelated part is non-fatal, but a selected
  text attachment with an unknown charset returns `Malformed` rather
  than undecoded bytes.
- The IMAP backend uses the same read-only `EXAMINE` and
  `UID FETCH BODY.PEEK[]` path as `read_message`; fetching an
  attachment never sets `\Seen`. The memory backend uses a private
  raw-message snapshot and does not re-read the source file.
- Filenames and MIME types are untrusted metadata. The tool never
  opens or executes the content and never writes it to disk.

**Required capability:** `read` only. Outbound `attachment_send` is a
separate capability and is not required or implied.

### `search_messages`

Searches a mailbox.

**Input.**

```json
{
  "account_id": "personal",
  "mailbox": "INBOX",
  "query": {
    "subject": "report",
    "from": "sender@example.test",
    "to": null,
    "body": null,
    "since": "2024-01-01T00:00:00Z",
    "before": null,
    "flagged": false,
    "unseen": true,
    "limit": 50
  }
}
```

Empty query fields are ignored. The `limit` is clamped per-account.

**Criteria semantics.**

- All populated criteria are **ANDed** together. A message must
  satisfy every criterion to be returned.
- `subject`, `from` and `to` are case-insensitive **substring**
  matches against the corresponding header. `to` matches any
  recipient in **To or Cc**.
- `body` is a case-insensitive substring match against the
  decoded plain-text body.
- `flagged` requires the `\Flagged` flag; `unseen` requires the
  **absence** of the `\Seen` flag. A message carrying no flags
  is unseen and not flagged.
- `since` and `before` have **day granularity**: the value is
  truncated to its UTC day before it is applied, because an IMAP
  SEARCH date key carries a date with no time component. A
  `since` of `2024-08-15T23:00:00Z` therefore includes messages
  sent earlier on 2024-08-15. The message's `Date:` header is
  compared by the calendar day named by its own fields,
  ignoring its time and timezone offset, matching the reference
  IMAP server. The IMAP and in-memory backends use the same two
  rules. A server that interprets the bare search date in its
  own local timezone can still differ by up to one day; that is
  inherent to IMAP's date-only search keys.
- An **empty query returns every message** in the mailbox,
  bounded by the per-account `max_search_results` limit.

No requested criterion is silently ignored. If a criterion
cannot be honoured, the tool returns an explicit error.

**Scan budget.** The search criteria are evaluated by the server, so a
query can match an arbitrarily large number of messages — an empty
query matches every message in the mailbox. The per-account
`max_list_scan_messages` limit (default 10 000, hard maximum 1 000 000)
bounds the client's share of that work, the messages it fetches and
processes. It does not bound the work the server does to evaluate the
criteria, which is server-side computation the client cannot observe or
limit. The bound is enforced twice: the UIDs the server
returns are counted, and a search that matches more messages than the
limit allows is refused with the `RateLimited` error code and a message
naming the budget and the recourse — narrow the query or raise
`max_list_scan_messages`; and the messages the server delivers in the
follow-up fetch are counted as well, so a server that sends more than
it was asked for is stopped. No partial or approximate result is ever
returned.

**Response limits.** The scan budget bounds the client's own work. The
IMAP client additionally caps what a single response may cost it: the
number of fields, the size of an atom or a quoted string, the total
number of bytes, and the nesting depth of the lists. The limits are
derived from the same account configuration, so a response that would
exceed them is refused before it is materialised. Such a response is
reported with the `RateLimited` error code and the connection is
closed, because a response that broke its limits cannot be trusted. The
limits apply to one response. An aggregate budget additionally
bounds the inbound protocol bytes one operation may consume, including
the unsolicited responses a server sends while the operation's commands
run, so a server that floods is cut off before the flood is parsed.
Neither limit bounds the work the server does to evaluate a search, nor
the duration of an operation.

**Server-dependent matching.** `subject`, `from` and `to`
matching is delegated to the IMAP server, so substring and
case behaviour and RFC 2047 encoded-header handling can vary by
server; the in-memory backend matches the decoded header text.
`body` search on IMAP is server-side and can match HTML,
attachments or raw MIME content, while the in-memory backend
searches decoded plain text only. These are inherent
differences in a server-delegated search and are not errors.

**Deferred attachment metadata.** The IMAP backend's
`list_messages` and `search_messages` summaries do not populate
`has_attachments`; it is always `false`. The in-memory backend
does populate it. This is a separate outstanding issue, not fixed
by `read_attachment`; populating it for IMAP would require
`BODYSTRUCTURE` parsing and is deliberately not introduced in this
slice.

**Output.**

```json
{
  "account_id": "personal",
  "mailbox": "INBOX",
  "uid_validity": 1234567890,
  "matches": [
    { "uid": 1024, "from": "...", "subject": "...", "internal_date": "...", "flags": ["\\Seen"] }
  ]
}
```

## Future tools (not implemented)

These names and shapes are sketches, not current contracts. `send_message`
is implemented; reply/forward, mailbox mutation and deletion are not.

- `reply_message` — reply, optionally quoting the original. Requires `send`. Honours `reply_all`.
- `forward_message` — forward an existing message including its attachments. Requires `send` and `attachment_send`.
- `move_message` — move to another mailbox. Requires `modify`.
- `archive_message` — move to the configured archive mailbox. Requires `modify`.
- `flag_message` — set/clear flags. Requires `modify`.
- `delete_message` — delete (move to Trash) or expunge. Requires `delete`.

## What will never be exposed

- Raw IMAP or SMTP command pass-through (would defeat the abstraction and break the permission model).
- Credential fields in any output.
- Filesystem write paths outside the configured attachment roots.
- Absolute attachment roots in `list_accounts`; only an
  `attachment_send_enabled` boolean is exposed.
- Provider administration endpoints.