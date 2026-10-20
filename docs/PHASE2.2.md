# Phase 2.2 Plan

> **Status:** development-only. TheMailroom is **not**
> production-hardened.
> **Prerequisite:** Phase 2.1 (PR #3) merged on `main` at
> `0bfe07b`.

## Progress

| Priority | Scope | Status |
|---|---|---|
| 1 | Protocol-level IMAP literal-size enforcement | **DONE** — merged in PR #5 (`d28dfa3`) |
| 2 | IMAP search correctness defects | **DONE** — merged in PR #6 (`21f859b`) |
| 3 | Secure attachment retrieval | **DONE** — merged in PR #8 (`2690380`) |

## Scope

Phase 2.2 resolves the three pre-production gaps that prevent
Phase 2.1 from being production-hardened:

1. **Protocol-level IMAP literal-size enforcement** (mandatory
   security requirement; PHASE2_DESIGN.md §10). The application-
   level cap does not bound go-imap's pre-allocation; oversized
   IMAP literals can cause unbounded allocation before the
   application-level rejection. **DONE** — see "Priority 1"
   below.
2. **IMAP search correctness defects** (PHASE2_DESIGN.md §7).
   `Flagged` and `Unseen` filters are accepted by the schema but
   silently ignored. `Subject + From + To` are OR'd via the
   IMAP `TEXT` keyword. `SentSince` / `SentBefore` use the RFC
   5322 `Date:` header, not the server's internal date.
   **DONE** — merged in PR #6 (`21f859b`) after HAL approval at
   `38b0bf4`.
3. **Secure attachment retrieval** with stable MIME-part
   references and strict resource limits. The new `read_attachment`
   tool reads a single MIME part by stable part reference
   (mailbox + UIDVALIDITY + UID + part path) with the same
   per-attachment and total-attachment caps as the inline
   path.

## Out of scope (deferred)

- SMTP submission, `send_message`, `reply_message`,
  `forward_message`.
- Mutating mailbox operations (`move_message`,
  `flag_message`, `archive_message`, `delete_message`).
- Outbound attachment sourcing via `os.Root` (the read
  direction is in scope; the write direction is for a later
  slice that introduces SMTP).
- OAuth2 (PHASE2_DESIGN.md §6).

## Priorities

### Priority 1 — Protocol-level IMAP literal-size enforcement

**Status:** **DONE**. Merged in PR #5 as `d28dfa3` on `main`.

**Acceptance criteria:** the twelve criteria in PHASE2_DESIGN.md
§10 are authoritative, with a per-criterion status table and
evidence in that section. All twelve are met.

**Implementation chosen: option 1, a narrow fork of go-imap.**

- Module: `github.com/flowbyte-com/go-imap`
- Version: `v1.2.1-themailroom.4` (immutable tag, commit
  `0fc6600`)
- Upstream: `github.com/emersion/go-imap` v1.2.1
- License: BSD-2-Clause, preserved verbatim
- Pinned in `go.mod` by version, **no `replace` directive**; the
  build resolves from the public module proxy exactly like any
  other dependency.

The fork changes two upstream source files:

1. `read.go` — exports `imap.ErrLiteralSizeExceeded`;
   `ReadLiteral` returns it when the announced size exceeds
   `Reader.MaxLiteralSize`. The check still runs **before** the
   `make([]byte, n)` allocation, so an over-cap announcement is
   rejected without allocating a buffer proportional to the
   announced size. `IsParseError` is extended to match it.

2. `client/client.go` — adds `client.DialOptions{MaxLiteralSize}`
   and `client.NewWithOptions`; closes the connection on a parse
   error (upstream keeps a desynchronised connection alive, which
   can block the executor indefinitely) and records the cause;
   replaces `errClosed` with a typed `*connectionClosedError` that
   `Unwrap`s the cause.

**The constructor applies the cap before the reader is used.**
This was a defect found in the PR #5 review: the first cut set the
cap *after* `New(conn)` had already parsed the greeting and
started the background reader goroutine, which was both a data
race and a pre-configuration window in which the live reader
could accept an over-cap literal with no cap at all. The cap is
now assigned to the `*imap.Reader` at creation time, before the
greeting is parsed and before the reader goroutine exists.
`New(conn)` keeps its upstream signature and behaviour.

TheMailroom sets `DialOptions.MaxLiteralSize` to the per-account
`max_message_bytes` at dial time for both the implicit-TLS and
the STARTTLS path. The cap is set on the IMAP `Reader`, not the
transport, so it observes the *decrypted* stream in both modes;
the `*imap.Reader` survives the STARTTLS upgrade.

**Maintenance and upstreaming requirement (outstanding).**

A long-term dependency on a personal fork is itself a
maintenance risk. The fork must be either:

- **upstreamed** to `github.com/emersion/go-imap` (the change set
  is small and self-contained, and is suitable for a pull
  request — see the fork's `CHANGES.md` for the route), or
- **replaced** with a different IMAP client that exposes a
  literal-size cap natively.

Until one of those happens, TheMailroom depends on a fork
maintained by one person. This is tracked as an open item and is
one of the reasons TheMailroom is not described as
production-hardened.

**Deliverables (all landed in PR #5):**

- The fork, pinned by version, with its own CI workflow.
- `internal/imapclient/literal_cap_test.go`: source-level
  ordering assertions (`TestForkEnforcesCapBeforeAllocation`,
  `TestForkAppliesCapBeforeReaderStarts`) and behavioural tests
  (boundary, unsolicited over-cap literal, binary content,
  malformed announcements, reconnect, no silent success,
  allocation scaling).
- Fork-side `client/constructor_test.go`: the
  constructor-ordering regressions, which report a data race
  against the pre-fix constructor shape.
- Updated package doc, `docs/THREAT_MODEL.md` T4,
  `docs/ARCHITECTURE.md` and `docs/PHASE2_DESIGN.md` §8/§10.

### Priority 2 — IMAP search correctness defects

**Status:** **DONE** — merged in PR #6 as `21f859b` after HAL
approval at `38b0bf4`. The change is narrowly scoped and
preserves the `EXAMINE` / UIDVALIDITY pairing and the
literal-size safety controls from Priority 1.

**Defects to fix (PHASE2_DESIGN.md §7):**

1. **`Subject`, `From` and `To` must use the corresponding IMAP
   SEARCH fields with AND semantics.** Today all three are
   passed in a single `Text` slice, which the IMAP `TEXT` keyword
   ORs together (and which also matches the body). The fix maps
   them to `Subject`, `From` and `To` respectively, and combines
   the criteria with AND. RFC 3501 section 6.4.4 ANDs multiple
   SEARCH keys and provides the `OR` key, so one `UID SEARCH`
   carries `Subject`, `From`, body, flags and dates, with an
   explicit `OR To Cc` for the recipient filter. No client-side
   multi-SEARCH/UID-set intersection is needed.
2. **`q.Flagged` must be honoured** — IMAP `WithFlags =
   []string{"\\Flagged"}`.
3. **`q.Unseen` must be honoured** — IMAP `WithoutFlags =
   []string{"\\Seen"}`.
4. **The tool layer must stop rejecting `Flagged` and
   `Unseen`** once the backends honour them. The typed
   `NotImplemented` rejection added in the PR #3 follow-up is
   removed so callers can use the fields and get the right
   results.
5. **Date filtering must reflect the documented API semantics,
   accounting for IMAP's day-granularity limitations.** IMAP
   `SINCE` / `BEFORE` are **date** criteria: they compare the
   message's `Date:` header at day granularity, so a
   `since` of `2024-08-15T10:11:12Z` behaves as
   "on or after 2024-08-15" and can include messages earlier
   on that day. The API must document this, and the memory
   backend (which has exact timestamps) must behave the same
   way so the two backends agree on the public contract.
6. **Search limits and empty-query behaviour must be
   consistent.** An empty query currently returns every message
   in the mailbox. That is a reasonable and documented
   behaviour, but it must be explicit: the limit is applied,
   and the result is bounded by the per-account
   `max_search_results`.
7. **The IMAP and memory backends must agree wherever their
   public API contracts should.** Where the two backends
   cannot agree (e.g. IMAP's day-granularity dates, or the
   memory backend's exact substring matching versus IMAP's
   server-side semantics), the difference must be documented
   rather than papered over.

**No silently ignored conditions.** If a requested condition
cannot be honoured accurately, the implementation must either
return an explicit error or document the limitation as a
deliberate part of the API contract. Silently dropping a
filter is not acceptable.

**Deliverables for this slice:**

- IMAP search criteria translation in
  `internal/imapclient/imapclient.go::SearchMessages` (or a
  new `internal/imapclient/search.go` helper).
- One IMAP SEARCH combining `Subject` / `From` / body / flags /
  dates, with `OR To Cc` for the recipient filter.
- `WithFlags` / `WithoutFlags` for `Flagged` / `Unseen`.
- Day-granularity date handling, documented, consistent
  between the IMAP and memory backends.
- Removal of the tool-layer rejection of `Flagged` / `Unseen`
  in `internal/tools/register.go` once the backends honour
  them.
- Controlled in-process IMAP-server integration tests
  covering combinations of search criteria, unread / flagged
  states, date boundaries, empty searches, and result limits.
- Tests in `internal/mailbox/memory/` for the same
  contract, and in `internal/tools/tools_test.go` for the wire
  behaviour.

**Review closeout (PR #6).** HAL approved the corrected head
`38b0bf4`; the PR squash-merged as `21f859b`. The review
requested these corrections:

- IMAP search now issues one `UID SEARCH` with the header,
  body, flag and date keys ANDed by the server, plus an
  explicit `OR To Cc` for the recipient filter. The client-side
  multi-SEARCH/UID-set intersection was removed.
- Memory flags now follow the conventional mbox `Status:`
  (`R`/`O`) and `X-Status:` (`D`/`F`/`A`) headers. The previous
  RFC 2156 attribution was incorrect.
- Memory date search reduces the message `Date:` header to the
  calendar day named by its own fields
  (`searchspec.CalendarDay`), including non-UTC headers, instead
  of comparing the UTC instant.
- The memory backend clamps `Limit` to `max_search_results`
  itself, matching the IMAP backend and the tool layer.

Regression tests cover the combined SEARCH structure, both
flag headers, and both date/limit fixes, including a
cross-backend non-UTC date-boundary case. Remaining
server-dependent matching differences and the deferred
`has_attachments` metadata for IMAP summaries are documented in
`docs/PHASE2_DESIGN.md` §7 and `docs/TOOLS.md`.

**Out of scope for this slice:** SMTP, mailbox modifications,
attachment retrieval.

### Priority 3 — Secure attachment retrieval

**Status:** **DONE**. Merged in PR #8 at `2690380` after HAL
approval at `41430936`.

**Requirements:**

- New MCP tool `read_attachment` with input
  `{account_id, mailbox, uid, uid_validity, part}` and output
  `{account_id, mailbox, uid, uid_validity, part, filename,
  mime_type, size, content_base64, warnings}`.
- `part` is the IMAP-style 1-indexed part path returned by
  `read_message` (e.g. `"1.2"`). It must match the existing
  `read_message` attachment reference exactly.
- The reference is `(account, mailbox, UIDVALIDITY, UID, part)`.
  The backend validates UIDVALIDITY against the live mailbox and
  rejects stale values with `ErrUIDMismatch`.
- The backend applies the same per-attachment, total-attachment,
  message-size, attachment-count, nesting and MIME-part caps as
  the inline path. The complete message is validated even after the
  requested part has been captured; a later failure returns no
  content.
- The backend returns typed errors for a missing attachment and
  invalid part input. `ErrTotalAttachmentTooLarge` maps to the
  existing MCP `AttachmentTooLarge` code.
- Attachment body is returned as standard padded base64. There is
  no `truncated` field and no partial success for oversized
  content.
- The tool requires only the existing `read` capability.
- The backend does not write to disk, and no agent-supplied
  filesystem path influences retrieval.

**Approved implementation approach:** full-message
`BODY.PEEK[]` followed by selected-attachment extraction through
the existing `internal/mimeparse` traversal. This deliberately
avoids per-part IMAP retrieval, BODYSTRUCTURE parsing, caching,
temporary files, streaming and new storage subsystems. The
full-message approach gives an exact message-wide total cap and
exact part membership shared with `read_message`.

**Implementation:**

- `mailbox.Backend.FetchAttachment`, `mailbox.AttachmentData`,
  typed `ErrAttachmentNotFound` and `ErrInvalidPart`, and shared
  part-path validation in `internal/mailbox/part.go`.
- `mimeparse.ExtractAttachment`, sharing the existing
  `Parse` traversal, classification and resource limits. It
  captures at most the selected attachment and returns it only
  after the complete traversal succeeds. Extraction is
  fail-closed: malformed siblings, body read or transfer-decoding
  failures, unknown transfer encodings and oversized ordinary body
  parts return typed errors instead of partial content, while
  unrelated non-fatal charset warnings remain warnings. `Parse`
  retains its existing best-effort behaviour.
- The returned backend metadata preserves the selected
  attachment's zero-based ordinal, matching the index exposed by
  `read_message`.
- IMAP `FetchAttachment` reusing the refactored read-only
  `EXAMINE` + `UID FETCH BODY.PEEK[]` path, live UIDVALIDITY and
  exact UID validation, protocol literal cap and MIME limits.
  No part path is interpolated into an IMAP command.
- Memory `FetchAttachment` using a private raw-message snapshot
  retained only for messages containing exposed attachments. The
  source `.eml` is not re-read during retrieval. The additional
  memory cost is bounded by the raw size of those messages and is
  reported in the PR.
- `read_attachment` registration and typed error mapping in
  `internal/tools`.
- Tests for exact content/metadata, nested and inline parts,
  opaque `message/rfc822`, non-attachment and invalid-part
  rejection, stale UIDVALIDITY/missing UID, size limits including
  a failure after the target was captured, malformed/truncated
  siblings after the target, unknown transfer encodings,
  non-fatal charset warnings, selected-text charset failure,
  selected-attachment index preservation, unchanged `\Seen`
  flags, memory retrieval after source deletion, MCP permissions,
  base64 round-trip and existing `read_message` regressions.

**Known content semantics:** text parts are charset-decoded to
UTF-8 where possible, so text attachment content may be normalised
rather than byte-identical to the sender's file. This is the
existing parser behaviour and is documented in `docs/TOOLS.md`;
the implementation does not claim lossless byte preservation for
text parts.

**Deferred:** IMAP `has_attachments` summary population remains
unchanged and is tracked as a separate outstanding issue. It is
not fixed by Priority 3, and BODYSTRUCTURE parsing is not
introduced. Outbound SMTP attachments, forwarding, modification
operations, previews and antivirus integration remain out of
scope.

## Workflow

- Use the normal feature-branch, PR and CI workflow.
- The initial branch is `phase-2.2/imap-literal-cap`. Each
  priority may land in its own branch and PR.
- Each PR must include: unit tests with `-race`, `go vet`,
  `gofmt`, `go mod verify`, and CI green.
- Each PR must update `PHASE2_DESIGN.md` if the design
  changes.
- Each PR must update the threat model if the security
  posture changes.

## MPM continuity

- The Phase 2.1 handoff (`mpm://memory/ab1639591c2aa292`)
  is the baseline; new MPM memories are written for
  decisions, blockers, and PR-3-review findings that apply
  to Phase 2.2.
- A Phase 2.2 kickoff memory is written on the first
  commit of each priority.
- MPM handoffs are written at the end of each priority
  slice.
