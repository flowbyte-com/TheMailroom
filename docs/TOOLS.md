# TheMailroom Tools

> Status: Phase 1 implements the **read-only** tools below. Phase 2 adds sending, modification, deletion, and attachment tools.

Every tool:

- Validates input against a JSON Schema.
- Returns a structured JSON result.
- Enforces a permission capability server-side.
- Returns typed errors (`PermissionDenied`, `AccountNotFound`, `InvalidInput`, `BackendError`, `NotImplemented`, etc.) without leaking credentials or wire details.

All tools take `account_id` as a required string field. There is no implicit "current account".

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
- `BackendError` — IMAP/SMTP operation failed; details are redacted.
- `RateLimited` — limit exceeded.
- `UidMismatch` — `uid_validity` does not match the current mailbox.
- `NotImplemented` — capability exists in the schema but is not yet wired.
- `MessageTooLarge` — the message exceeds `max_message_bytes`.
- `NestingTooDeep` — the MIME nesting exceeds `max_mime_nesting`.
- `TooManyParts` — the MIME part count exceeds `max_mime_parts`.
- `TooManyAttachments` — the attachment count exceeds `max_attachments`.
- `AttachmentTooLarge` — an attachment exceeds `max_attachment_bytes`.
- `Malformed` — the message could not be parsed.

## Read-only tools (Phase 1)

### `list_accounts`

Lists the IDs and metadata of configured accounts. **No credentials** are returned.

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
      "limits": { "max_message_bytes": 26214400, "max_attachments": 50, "max_search_results": 500, "max_batch_operations": 50 },
      "attachment_send_paths": []
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
references. `disposition` is `attachment` or `inline`. Decoded
bodies are never materialised inline; a follow-up `read_attachment`
tool (Phase 2) returns the bytes through a confined stream.

**Read-only IMAP guarantees.**

- The IMAP backend issues `EXAMINE` (read-only `SELECT`) for every
  read. The server-side state cannot accept `STORE` or `APPEND`.
- The body is fetched with `UID FETCH BODY.PEEK[]`, which never
  implicitly sets `\Seen`. A message that the server reports as
  `\Unseen` remains `\Unseen` after `read_message`.
- On connection error the client reconnects; the next call
  re-`EXAMINE`s and re-validates UIDVALIDITY before acting. A
  mutating operation is never silently retried.

**IMAP literal memory: known UNBOUNDED risk (pre-Phase-2.2).**

The `max_message_bytes` cap on the IMAP body is an
application-level backstop, **not** a pre-allocation guard.
The underlying IMAP client (go-imap v1.2.1) allocates a
buffer of the server-announced literal size and reads the
literal body into it *before* the application sees the
data. A server (including a hostile or buggy one) that
announces a literal larger than `max_message_bytes` will
cause that allocation *before* the application rejects the
body. The default 25 MiB cap does not bound this
allocation.

Production deployments that fetch from untrusted IMAP
servers must mitigate this at the network layer
(firewall, reverse proxy with a body-size filter) until
the protocol-level cap is implemented in Phase 2.2. The
tracked security requirement with acceptance criteria is
in `docs/PHASE2_DESIGN.md` §10.

**MIME safety limits** (per account):

- `max_message_bytes` — caps the raw RFC 5322 size (default 25 MiB).
- `max_attachment_bytes` — caps a single attachment (default 6 MiB).
- `max_total_attachment_bytes` — caps the sum of attachments (default 25 MiB).
- `max_mime_nesting` — caps multipart depth (default 16).
- `max_mime_parts` — caps the total number of parts visited (default 500).

Violations return a typed error (`MessageTooLarge`, `AttachmentTooLarge`,
`NestingTooDeep`, `TooManyParts`) without reading the rest of the message.

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
does populate it. Filling it for IMAP requires
`BODYSTRUCTURE` parsing and is deferred to the Phase 2.2
Priority 3 `read_attachment` work.

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

## Phase 2 tools (sketched)

These names and shapes are stable contracts; only the implementation lands later.

- `read_attachment` — fetch an attachment by `(mailbox, uid, attachment_index)`. Requires `read`.
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
- Provider administration endpoints.