# Live SMTP Smoke-Test Runbook

> **Status: NOT EXECUTED.**
>
> This document is an operator checklist for a future, separately
> authorized test. It is not authorization to send email, and creating it
> does not execute it. Do not run this against production accounts or
> real recipients.

## Purpose

Exercise the merged Phase 2.3 `send_message` path against a controlled
external SMTP submission endpoint and record what actually happened.

The smoke test covers:

- a dedicated account with the `send` capability;
- a restricted `allowed_recipients` policy and permission/recipient
  preflight controls;
- a verified SMTP endpoint, TLS mode and credential source;
- plain text, HTML/plain alternative and multiple-attachment
  submissions from confined roots;
- Bcc envelope privacy and MIME/header encoding;
- Message-ID and SMTP-acceptance verification;
- audit-log inspection for secret or content leakage; and
- recording observations, typed error codes and operational defects.

It does not test reply, forward, mailbox mutation, IMAP append,
automatic Sent-folder copies, retries, a durable queue, a replay API or
SMTPUTF8. Those features do not exist in Phase 2.3.

## Three different outcomes

Do not treat these as interchangeable:

| Outcome | What it means | How to check |
|---|---|---|
| **SMTP acceptance** | The SMTP server returned a final 2xx response after DATA. This is what `status: "submitted"` means. | Tool result and audit log. |
| **Mailbox receipt** | The message is visible in the intended recipient's mailbox. SMTP acceptance does **not** guarantee this. | Sign in to each test recipient and inspect the message source. Check spam/trash if needed. |
| **Sent-folder copy** | The sending account has a copy in its Sent folder. | Inspect the sending account's Sent folder. TheMailroom never creates this copy; a provider may do so on its own. Absence is not a `send_message` failure. |

Record each outcome separately. A successful tool call without mailbox
receipt is still a failed smoke test for delivery.

## Preconditions

- [ ] Explicit approval exists to run a live external SMTP test.
- [ ] The test uses a **dedicated** account, not a personal or production
      account.
- [ ] The account has `send`. Add `attachment_send` only if testing
      attachments. `read` is useful for inspecting the test mailbox.
- [ ] `allowed_recipients` is restricted to test mailboxes controlled by
      the operator.
- [ ] The SMTP host, port, TLS mode and username are confirmed from
      provider documentation.
- [ ] The server advertises **AUTH PLAIN**. TheMailroom does not support
      other SMTP authentication mechanisms.
- [ ] The credential source is verified. Prefer `password_command` over a
      literal password in the file.
- [ ] The SMTP endpoint uses implicit TLS or mandatory STARTTLS, with a
      certificate valid for the configured host. There is no plaintext or
      skip-verification mode.
- [ ] The binary and configuration are built and validated.
- [ ] A place to record observations is ready, excluding credentials and
      unnecessary personal data.

## Safe test configuration

Use placeholder domains. Do not paste real credentials, private
addresses or provider secrets into this runbook or a report.

```yaml
server:
  log_level: info

accounts:
  - id: smtp-smoke
    email: sender@example.test

    imap:
      host: imap.example.test
      port: 993
      tls: true
      username: sender@example.test
      password_command: ["pass", "themailroom/smtp-smoke/imap"]

    smtp:
      host: smtp.example.test
      port: 587                 # 587 STARTTLS; 465 implicit TLS
      starttls: true            # use tls: true for implicit TLS
      username: sender@example.test
      password_command: ["pass", "themailroom/smtp-smoke/smtp"]

    permissions:
      - read
      - send
      - attachment_send

    allowed_recipients:
      - smtp-smoke-recipient@example.test
      - smtp-smoke-bcc@example.test

    attachment_send_paths:
      - /srv/themailroom/smtp-smoke/outbound

    limits:
      max_message_bytes: 1048576
      max_attachment_bytes: 262144
      max_total_attachment_bytes: 524288
      max_attachments: 3
      max_recipients: 3
```

If attachments are not being tested, remove `attachment_send`, the
attachment root and the attachment limits. The dedicated account still
needs valid IMAP settings because it uses the default IMAP backend; the
smoke test does not otherwise use them.

`allowed_recipients` matching is exact. Local-part case is significant;
domain case is not. If an address is rejected, compare the canonical
address, not a wildcard pattern.

## Build and start

The commands below match the actual CLI:

```sh
go build -o bin/themailroom ./cmd/themailroom
./bin/themailroom -version
./bin/themailroom -config /etc/themailroom/smtp-smoke.yaml
```

The last command starts the stdio MCP server and blocks. Start it through
the test MCP client, or run it manually and speak MCP over stdin/stdout.
Diagnostics go to stderr. Capture those logs for the audit check, but do
not disclose them until they have been inspected for sensitive data.

Do not invoke a non-existent command-line "send" subcommand.
`send_message` is an MCP tool; this binary has no email-sending CLI
command.

## Test sequence

Use the test MCP client (for example MCP Inspector) to call
`send_message`. The Mailroom tool input is JSON:

```json
{
  "account_id": "smtp-smoke",
  "to": ["smtp-smoke-recipient@example.test"],
  "subject": "TheMailroom SMTP smoke test",
  "text": "Plain-text smoke-test body."
}
```

A successful result has this shape:

```json
{
  "account_id": "smtp-smoke",
  "message_id": "generated@example.test",
  "status": "submitted",
  "recipient_count": 1,
  "attachment_count": 0,
  "total_attachment_bytes": 0
}
```

The exact Message-ID is generated for the message and is returned
without angle brackets. The serialized header contains it in angle
brackets.

### 1. Plain-text submission

- [ ] Send the request above.
- [ ] Confirm `status: "submitted"`, non-empty `message_id`,
      `recipient_count: 1`, `attachment_count: 0` and
      `total_attachment_bytes: 0`.
- [ ] Confirm the test recipient received the message.
- [ ] Inspect the recipient's raw message source:
  - [ ] `From` is the account's configured `email`;
  - [ ] `To` contains the intended recipient;
  - [ ] `Subject` is correct;
  - [ ] the body is the sent plain text; and
  - [ ] `Message-ID` is `<returned-message-id>`.

### 2. HTML/plain-text alternative and header encoding

Send a request with both `text` and `html`, including a non-ASCII
subject to exercise RFC 2047 header encoding:

```json
{
  "account_id": "smtp-smoke",
  "to": ["smtp-smoke-recipient@example.test"],
  "subject": "TheMailroom alternative smoke test — café",
  "text": "Plain-text alternative.",
  "html": "<p>HTML alternative.</p>"
}
```

- [ ] The result reports `status: "submitted"`.
- [ ] The recipient sees a multipart/alternative message.
- [ ] A client that prefers plain text shows the plain part; a client
      that prefers HTML shows the HTML part.
- [ ] The raw source contains both parts and no Bcc header.
- [ ] The recipient client displays the subject exactly, including
      `café`; the raw header may use RFC 2047 encoded words.

### 3. Multiple attachments from confined roots

Place two known test files under the configured root. Use relative paths
only. Include a non-ASCII filename to exercise encoded MIME parameters:

```json
{
  "account_id": "smtp-smoke",
  "to": ["smtp-smoke-recipient@example.test"],
  "subject": "TheMailroom attachment smoke test",
  "text": "Two attachments follow.",
  "attachments": ["first.txt", "résumé.txt"]
}
```

- [ ] Confirm `attachment_count: 2`.
- [ ] Confirm `total_attachment_bytes` equals the sum of the two source
      file sizes.
- [ ] Confirm both attachments' names, decoded bytes and contents match
      the source files.
- [ ] The recipient client displays `résumé.txt` exactly; the raw source
      may use RFC 2231 encoded parameters.
- [ ] Optionally repeat with a long internationalized filename to
      exercise RFC 2231 continuations.
- [ ] Confirm the filenames are basenames and no host path appears in the
      tool result or the message.
- [ ] Optionally verify an absolute path or an escaping path such as
      `../first.txt` is rejected before any SMTP connection. This is a
      negative control; do not include it in a message.

### 4. Bcc privacy

Use two test recipients: one in `to`, one in `bcc`:

```json
{
  "account_id": "smtp-smoke",
  "to": ["smtp-smoke-recipient@example.test"],
  "bcc": ["smtp-smoke-bcc@example.test"],
  "subject": "TheMailroom Bcc smoke test",
  "text": "Bcc privacy check."
}
```

- [ ] The result reports `recipient_count: 2`.
- [ ] Both test recipients receive the message.
- [ ] The `to` recipient's headers contain `To` but **no** `Bcc` header
      and no Bcc address.
- [ ] The Bcc recipient's headers also omit `Bcc`; the address is
      visible only through the provider's envelope/delivery information.
- [ ] Do not use a personal mailbox for this check.

The tool result does not expose the SMTP transcript or full recipient
list. Privacy is confirmed by recipient delivery plus the absence of
Bcc in the serialized headers.

### 5. Preflight controls (no SMTP)

These controls should fail locally, before any SMTP connection:

- [ ] Send to an address outside `allowed_recipients`, for example
      `not-allowed@example.test`. Confirm `RecipientNotAllowed` and that
      no test mailbox receives the message.
- [ ] Send an attachment reference that escapes the configured root,
      such as `../first.txt`. Confirm `InvalidInput` and no message.
- [ ] Optionally start a second server with a copy of the configuration
      that removes `send`. Call `send_message` and confirm
      `PermissionDenied`.
- [ ] Record each typed error code. None of these controls may create an
      SMTP transaction or a delivered message.

A successful live submission also proves that verified TLS and AUTH
PLAIN completed: the sender cannot reach `MAIL FROM`, let alone DATA,
without both. Record the endpoint's TLS mode and authentication
mechanism in the observations.

### 6. Message-ID and SMTP acceptance

For each successful send:

- [ ] Record the returned bare `message_id`.
- [ ] Find `<message_id>` in the recipient's `Message-ID` header.
- [ ] Confirm the provider shows the message as accepted/delivered
      according to its own UI.
- [ ] Distinguish that from mailbox receipt: only the recipient's inbox
      proves receipt.
- [ ] Confirm no automatic Sent copy was made by TheMailroom. If the
      provider auto-saves to Sent, record that as provider behaviour,
      not tool behaviour.

### 7. Audit-log inspection

Run with `log_level: info` so the structured `send_message` audit record
is emitted to stderr. For each call, confirm the record contains at
most:

- `account_id`
- `message_id`
- `recipient_count`
- `attachment_count`
- `total_attachment_bytes`
- `outcome`
- `error_code` on failure

Then confirm the `send_message` record does **not** contain:

- [ ] SMTP or IMAP passwords, command output or credential-file contents;
- [ ] message bodies or attachment contents;
- [ ] raw SMTP transcripts or server response text;
- [ ] the full recipient list, including Bcc addresses; or
- [ ] host filesystem paths.

The startup config summary redacts credentials but can include
operator-specific endpoints and attachment paths, so inspect the
complete captured log before sharing it. Do not publish logs containing
operator-specific endpoint or mailbox details without review.

### 8. Record observations

Use a table like this, with placeholders rather than personal data:

| Case | Tool result | Message-ID | SMTP accepted? | Mailbox received? | Sent-folder behaviour | Defect/note |
|---|---|---|---|---|---|---|
| Plain text | | | | | | |
| Alternative/header | | | | | | |
| Attachments | | | | | | |
| Bcc | | | | | | |
| Preflight controls | | | | | | |

Also record:

- date/time and environment;
- SMTP host class and TLS mode (not the password);
- whether STARTTLS was required or implicit TLS used;
- recipient test aliases, if they cannot be linked to a person;
- exact typed error codes and Message-IDs for failures; and
- any provider, network, TLS, authentication or delivery anomaly.

Do not record credentials, personal addresses, message bodies or
attachment contents in the report.

## Safe stop after timeout or ambiguity

`SmtpAmbiguousFailure` means message-data transmission began, but no
definitive final SMTP response was received. The server may have
accepted the message. The tool error must contain the original
`message_id` and `may_have_been_sent: true`.

If it occurs:

1. **Stop.** Do not resend, and do not use an external retry wrapper.
2. Record the `message_id`, timestamp, account and test case.
3. Check the recipient mailbox, spam/trash, and provider delivery
   history.
4. Check the sending account's Sent folder only to understand provider
   auto-save behaviour; it is not proof of delivery.
5. Wait long enough for any provider delay before concluding the message
   is absent.
6. Only after confirming absence, and with operator approval, compose a
   **new** message. It will have a new Message-ID. Do not attempt to
   replay the old bytes; there is no replay API.
7. Record the incident and report it separately if it indicates a code
   or provider defect.

`SmtpTimeout` is reported only for a timeout or cancellation before
message data begins. That is a definite pre-DATA failure, but if there is
any uncertainty about the stage, treat the outcome as ambiguous and
follow the safe-stop steps above.

A rejection before DATA (`SmtpTLSFailed`, `SmtpAuthFailed`,
`SenderRejected`, `RecipientRejected`, `SmtpConnectionFailed` or
pre-DATA `SmtpTimeout`) means no message data was sent. Diagnose the
endpoint, TLS, credentials or recipient before a deliberate retry.

## Pass criteria

The smoke test passes only when:

- [ ] all intended live-send cases return `status: "submitted"`;
- [ ] each recipient receives the expected message;
- [ ] the preflight controls return their expected typed errors;
- [ ] Bcc addresses and Bcc headers are absent from recipient headers;
- [ ] attachment counts, byte totals and contents match;
- [ ] returned Message-IDs match the delivered headers;
- [ ] `send_message` audit records contain no secrets, bodies,
      attachment contents, raw SMTP responses, full recipient lists or
      host paths; and
- [ ] Sent-folder behaviour is recorded as provider-specific, with no
      expectation that TheMailroom created a copy.

A failure in any case is an operational defect. Report code defects
separately; do not patch production code as part of the live test.

## Cleanup

- [ ] Stop the test MCP server.
- [ ] Revoke or rotate the dedicated test SMTP credential.
- [ ] Remove the test configuration and any secrets from the test host.
- [ ] Retain only the approved, redacted observations.
- [ ] Remove or lock down the restricted `allowed_recipients` test setup
      if it is no longer needed.

## Phase 2.3 limitations (not tested here)

- SMTP acceptance is not guaranteed final mailbox delivery.
- There is no automatic retry, durable queue, background worker or
  prepared-message replay API.
- There is no automatic Sent-folder copy.
- Reply, forward and mailbox mutations are out of scope.
- SMTPUTF8 envelope submission is unsupported; non-ASCII envelope
  addresses are rejected.
- Production hardening is not complete.
- Controlled external SMTP testing has not yet been performed.
