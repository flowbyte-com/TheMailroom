# Phase 2.2 Plan

> **Status:** development-only. TheMailroom is **not**
> production-hardened.
> **Prerequisite:** Phase 2.1 (PR #3) merged on `main` at
> `0bfe07b`.

## Progress

| Priority | Scope | Status |
|---|---|---|
| 1 | Protocol-level IMAP literal-size enforcement | **DONE** — merged in PR #5 (`d28dfa3`) |
| 2 | IMAP search correctness defects | **NEXT** |
| 3 | Secure attachment retrieval | Not started |

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
   5322 `Date:` header, not the server's internal date. **NEXT.**
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

**Status:** **NEXT**. This is the next development slice after
Priority 1. It must be a separate, narrowly scoped PR, and it
must not weaken the `EXAMINE` / UIDVALIDITY pairing or the
literal-size safety controls from Priority 1.

**Defects to fix (PHASE2_DESIGN.md §7):**

1. **`Subject`, `From` and `To` must use the corresponding IMAP
   SEARCH fields with AND semantics.** Today all three are
   passed in a single `Text` slice, which the IMAP `TEXT` keyword
   ORs together (and which also matches the body). The fix maps
   them to `Subject`, `From` and `To` respectively, and combines
   the criteria with AND. Because a single IMAP SEARCH command
   cannot express "Subject=x AND From=y" for arbitrary servers
   without `OR` chaining, the implementation issues one SEARCH
   per populated field and intersects the resulting UID sets.
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
- Per-field SEARCH + UID-set intersection for the AND
  semantics of `Subject` / `From` / `To`.
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

**Out of scope for this slice:** SMTP, mailbox modifications,
attachment retrieval.

### Priority 3 — Secure attachment retrieval

**Status:** high-priority; the new `read_attachment` tool is
required before any production deployment that needs to
download attachments.

**Requirements:**

- New MCP tool `read_attachment` with input
  `{account_id, mailbox, uid, uid_validity, part}` and
  output `{account_id, mailbox, uid, uid_validity, part,
  filename, mime_type, size, content (base64) | content_ref,
  truncated, warnings}`. The `part` is the IMAP-style
  1-indexed part path (e.g. `"1.2"` for the second part of
  a `multipart/related` nested inside the root).
- The reference is `(account, mailbox, UIDVALIDITY, UID,
  part)`. The backend must validate UIDVALIDITY against
  the live mailbox (re-EXAMINE if necessary) and reject
  stale values with `ErrUIDMismatch`.
- The backend must apply the same per-attachment and
  total-attachment caps as the inline path
  (`max_attachment_bytes`, `max_total_attachment_bytes`).
- The backend must return a stable error if the part is
  not found (`ErrAttachmentNotFound`).
- The backend must not write to disk.
- The backend must surface the same `ErrMessageTooLarge`
  signal if the attachment exceeds the cap (criterion 1 of
  Priority 1; without the protocol-level cap this is the
  application-level backstop, with the cap it is the
  protocol-level enforcement).
- Attachment body is returned as base64 in the wire
  response. For very large attachments, a streaming
  alternative is a future slice (out of scope here).
- Tool-layer rejection of `read_attachment` if the account
  lacks the `read` capability.

**Deliverables for this slice:**

- New `FetchAttachment` method on the `Backend` interface
  in `internal/mailbox/backend.go`.
- IMAP implementation in
  `internal/imapclient/imapclient.go` using
  `imap.FetchItem("BODY.PEEK[part]")` or a typed
  `imap.BodySectionName`.
- Memory implementation in
  `internal/mailbox/memory/memory.go` reading from the
  in-memory message.
- New tool registration in `internal/tools/register.go` with
  the input/output types in `internal/tools/read_attachment.go`.
- New typed error `mailbox.ErrAttachmentNotFound` and
  `codeAttachmentNotFound` MCP code.
- Tests in `internal/imapclient/correctness_test.go` and
  `internal/tools/tools_test.go` covering: in-spec
  attachment, oversized attachment, missing part, stale
  UIDVALIDITY, missing capability, and the boundary at the
  caps.

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
