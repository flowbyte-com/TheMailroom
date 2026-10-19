# Phase 2.2 Plan

> **Status:** development-only.
> **Branch:** `phase-2.2/imap-literal-cap` (initial slice).
> **Prerequisite:** Phase 2.1 (PR #3) merged on `main` at
> `0bfe07b`. The mandatory pre-production security requirement
> for protocol-level IMAP literal-size enforcement (PHASE2_DESIGN.md
> §10) is tracked here as the top priority.

## Scope

This slice resolves the three pre-production gaps that prevent
Phase 2.1 from being production-hardened:

1. **Protocol-level IMAP literal-size enforcement** (mandatory
   security requirement; PHASE2_DESIGN.md §10). The application-
   level cap does not bound go-imap's pre-allocation; oversized
   IMAP literals can cause unbounded allocation before the
   application-level rejection.
2. **IMAP search correctness defects** (PHASE2_DESIGN.md §7).
   `Flagged` and `Unseen` filters are accepted by the schema but
   silently ignored. `Subject + From + To` are OR'd via the
   IMAP `TEXT` keyword. `SentSince` / `SentBefore` use the RFC
   5322 `Date:` header, not the server's internal date.
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

**Status:** mandatory before any production deployment.

**Acceptance criteria** (from PHASE2_DESIGN.md §10; full text
in that file):

1. Pre-allocation enforcement. The cap is enforced before
   go-imap's `Reader.ReadLiteral` `make([]byte, n)`.
2. Correct boundary. cap-1 accepted, cap accepted, cap+1
   rejected.
3. Decrypted-transport compatibility. Cap applies to both
   implicit-TLS and STARTTLS.
4. IMAP framing compatibility. Literal-header detector is
   line-aware: only triggers on `{N}\r\n` or `{N+}\r\n`
   on its own line, preceded by a known IMAP separator.
5. Binary safety. Cap accepts any byte sequence in the body.
6. Disconnection safety. When the cap fires, the underlying
   conn is closed cleanly.
7. Both transport modes. Criteria 3–6 hold for both
   `cfg.IMAP.TLS == true` and `cfg.IMAP.StartTLS == true`.
8. No silent success. Backend must not return a successful
   `*mailbox.Message` with a truncated or omitted body.
9. No panic-disguised-as-size-limit. Panic in go-imap surfaces
   as a generic internal error.
10. Tight error mapping. Only `literal exceeding maximum size`
    maps to `ErrMessageTooLarge`; generic parse errors map to
    `ErrMalformed`.
11. Test coverage. Boundary tests at cap-1, cap, cap+1 for
    both the protocol-level filter and the application-level
    fallback.
12. Documentation. Package doc and `THREAT_MODEL.md` T4
    updated to remove any wording that suggests the application
    cap bounds go-imap's pre-allocation.

**Implementation preference (per PHASE2_DESIGN.md §10):**

1. **Fork `github.com/emersion/go-imap` to expose
   `Reader.MaxLiteralSize` as a constructor option on
   `client.Options`.** This is the cleanest path: a one-line
   constructor option that flows into the embedded
   `*imap.Reader`. The fork is narrowly scoped (a single
   field on `client.Options` and the corresponding wiring in
   `client.New`); the rest of the library is unchanged.
   Document the upstream tracking issue.
2. **Line-state `net.Conn` filter.** If the fork is not
   viable (e.g. upstream rejects the API), implement a filter
   that scans the decrypted byte stream for IMAP literal
   headers and closes the conn on oversized literals. The
   filter must be line-aware (criterion 4).
3. **Replace go-imap with a different library.** Last resort;
   only if options 1 and 2 are not viable.

**Deliverables for this slice:**

- `internal/imapclient/literal_cap.go` (or similar) with
  the chosen implementation, unit-tested at cap-1, cap,
  cap+1.
- Integration tests in `internal/imapclient/correctness_test.go`
  with both TLS modes.
- Update to the package doc and `THREAT_MODEL.md` to remove
  the "unbounded risk" wording now that the cap is enforced.
- New `imap_lit_test.go` (or extension of existing tests)
  using a server that announces a literal larger than the
  cap, verifying the go-imap executor does not allocate
  the announced size.

### Priority 2 — IMAP search correctness defects

**Status:** high-priority; affects user-visible behaviour.

**Defects to fix (PHASE2_DESIGN.md §7):**

1. `q.Flagged` not honoured. Translate to IMAP
   `WithFlags = []string{"\\Flagged"}` in
   `internal/imapclient/imapclient.go::SearchMessages`.
2. `q.Unseen` not honoured. Translate to IMAP
   `WithoutFlags = []string{"\\Seen"}`.
3. The tool layer currently rejects `Flagged` and `Unseen`
   with a typed `NotImplemented` error (PR #3 follow-up).
   The tool-layer rejection is removed once the backends
   honour the filters; callers can then set the fields and
   get the right results.
4. `Subject + From + To` are OR'd via the IMAP `TEXT`
   keyword. The right fix is server-dependent: issue multiple
   searches (one per field) and intersect, or use provider
   extensions (`OR` chaining, `X-GM-RAW` on Gmail). For
   Phase 2.2, implement the multi-search intersection
   approach as the default; document provider-specific
   variants for a later slice.
5. `SentSince` / `SentBefore` use the RFC 5322 `Date:`
   header. Expose both `since_date` and `since_internal` to
   the caller (the memory backend already has the data; the
   IMAP backend needs the IMAP `SINCE` / `BEFORE` search
   criteria with the right key). The default is `Date:` to
   preserve existing behaviour; the new fields are opt-in.

**Deliverables for this slice:**

- IMAP search criteria translation in
  `internal/imapclient/imapclient.go::SearchMessages`.
- Multi-search intersection logic in the IMAP backend (or a
  helper in `internal/imapclient/search.go`).
- Memory backend search support for `Flagged` and `Unseen`
  (currently `Unseen` returns no matches; `Flagged` is
  ignored).
- Removal of the tool-layer rejection of `Flagged` /
  `Unseen` in `internal/tools/register.go` once the backends
  honour them.
- New `SearchMessagesQuery` fields: `since_date`,
  `since_internal`, `before_date`, `before_internal`.
- Tests in `internal/imapclient/correctness_test.go` and
  `internal/tools/tools_test.go`.

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
