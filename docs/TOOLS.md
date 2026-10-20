# TheMailroom Tools

> Status: Phase 2.3 PR 1 is **IN REVIEW**. The read-only tools below are
> implemented and `read_attachment` is merged. PR 1 adds the recipient
> policy, SMTP configuration validation, and the confined attachment
> resolver; it does **not** add `send_message` or any SMTP/MIME
> submission path. Sending, modification and deletion remain future
> work.

Every tool:

- Validates input against a JSON Schema.
- Returns a structured JSON result.
- Enforces a permission capability server-side for account-scoped operations.
- Returns typed errors without leaking credentials or wire details. Unknown internal failures map to `InternalError`. `BackendError` is reserved for future protocol mapping and is not currently emitted.

All account-scoped tools take `account_id` as a required string field. There is no implicit "current account"; `list_accounts` is the only account metadata tool.

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
- `PermissionDenied` — capability not granted.
- `RateLimited` — limit exceeded.
- `UidMismatch` — `uid_validity` does not match the current mailbox.
- `NotImplemented` — capability exists in the schema but is not yet wired.
- `MessageTooLarge` — the raw message exceeds `max_message_bytes`, or an
  ordinary body part exceeds the per-part cap during `read_attachment`
  validation.
- `NestingTooDeep` — the MIME nesting exceeds `max_mime_nesting`.
- `TooManyParts` — the MIME part count exceeds `max_mime_parts`.
- `TooManyAttachments` — the attachment count exceeds `max_attachments`.
- `AttachmentTooLarge` — a single attachment exceeds `max_attachment_bytes`,
  or the decoded total exceeds `max_total_attachment_bytes`.
- `AttachmentNotFound` — the requested part is not an exposed attachment.
- `Malformed` — the message could not be parsed, or attachment
  extraction could not complete because a part or transfer encoding
  could not be decoded.

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

**Outbound policy limits (Phase 2.3 PR 1, no send tool yet):**

- `max_recipients` — caps unique outbound To + Cc + Bcc recipients (default 50).
- `max_attachment_bytes` / `max_total_attachment_bytes` — bound decoded
  bytes for path attachments resolved by `internal/attachments`.
- `max_message_bytes` — reserved for the future MIME composer's final
  serialized-size check.

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

These names and shapes are sketches, not current contracts.

PR 1 implements the configuration and attachment-resolution foundations
for these tools, but none of them is registered yet. In particular,
there is no SMTP client and no MIME composer.

- `send_message` — compose and send a new message. Requires `send` and `attachment_send` if attachments are included.
- `reply_message` — reply, optionally quoting the original. Requires `send`. Honours `reply_all`.
- `forward_message` — forward an existing message including its attachments. Requires `send` and `attachment_send`.
- `move_message` — move to another mailbox. Requires `modify`.
- `archive_message` — move to the configured archive mailbox. Requires `modify`.
- `flag_message` — set/clear flags. Requires `modify`.
- `delete_message` — delete (move to Trash) or expunge. Requires `delete`.

## What will never be exposed

- Raw IMAP command pass-through (would defeat the abstraction and break the permission model).
- Credential fields in any output.
- Filesystem write paths outside the configured attachment roots.
- Absolute attachment roots in `list_accounts`; only an
  `attachment_send_enabled` boolean is exposed.
- Provider administration endpoints.