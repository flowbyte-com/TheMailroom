# TheMailroom

> Secure, provider-agnostic IMAP/SMTP MCP server for AI agents.

TheMailroom is an [MCP](https://modelcontextprotocol.org) (Model Context Protocol)
server that lets AI agents work with email accounts over standard `IMAP4rev1`
and `SMTP`. It is designed to be:

- **Secure by default** — read-only unless explicitly enabled; no credentials
  in tool responses, logs, errors or repository files.
- **Provider-independent** — works with any IMAP/SMTP server. Provider
  plugins are not part of the design.
- **Compact and testable** — every protocol backend has a mocked
  counterpart; integration tests run in-process.

## Status

**Phase 1 (Foundation)** is implemented on `main`:

- MCP server over stdio using the official
  [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
- Tool surface: read-only `list_accounts`, `list_mailboxes`,
  `list_messages`, `read_message`, `search_messages`, plus
  authenticated `send_message` (Phase 2.3, merged as PR #12).
- YAML configuration with credential-via-command (`pass`, `secret-tool`).
- Permission and limit enforcement server-side.
- An in-memory backend (loads `.eml` files) for tests and local
  development.

**Phase 2 (Mail functionality)** is implemented through Phase 2.3;
production hardening remains future work. **Phase 2.1**
(read-only IMAP correctness + MIME reading) is merged on `main`
(PR #3), and all three **Phase 2.2** priorities are complete:
protocol-level IMAP literal-size enforcement (PR #5), IMAP search
correctness (PR #6), and secure inbound attachment retrieval (PR #8).
The Phase 2.2 documentation closeout is merged as PR #9.

**Phase 2.3 (Outbound submission)** implementation and documentation
closeout are complete on `main`. PR 1 added conditional SMTP
configuration validation, the recipient policy, the confined outbound
attachment resolver, and safer `list_accounts` output. PR 2 added the
standalone, bounded MIME composer. PR 3 added authenticated SMTP
submission and the `send_message` tool. PR 4 was the documentation-only
closeout.

| PR | Scope | Squash commit |
|---|---|---|
| #10 | Outbound security foundations | `bf3618f247928ea1f1d2c687aac1cffd96a89070` |
| #11 | Bounded MIME composer | `ab928c3a33ac5b760ca714c6f915fcbfe4366212` |
| #12 | Authenticated SMTP submission and `send_message` | `d1ecf774809681b2795a01f91182eec15a4b2f3f` |
| #13 | Documentation closeout | `a4851e9159af63026e5593f30988c114d7117261` |

Reply/forward, mailbox mutation and automatic Sent copies remain
future work. The controlled Purelymail smoke-test campaign was executed
on 2026-10-21 and passed. It exercised authenticated SMTP/TLS, MIME and
attachment delivery, Bcc privacy, Message-ID/IMAP interoperability and
preflight controls; the authoritative record and qualifications are in
[`docs/LIVE_SMTP_SMOKE_TEST.md`](docs/LIVE_SMTP_SMOKE_TEST.md). The
campaign is interoperability/smoke-test evidence, not exhaustive
security testing or production certification. Phase 2.3 is not a
production-hardened outbound system; the plan and explicit limitations
are in [`docs/PHASE2.3.md`](docs/PHASE2.3.md).

TheMailroom is a **development milestone, not a
production-hardened release**. The IMAP literal-size
pre-allocation vulnerability is fixed, but the fix depends on a
fork of go-imap that must still be upstreamed or replaced (see
[`docs/PHASE2_DESIGN.md`](docs/PHASE2_DESIGN.md) §8). Inbound
attachment retrieval is merged (PR #8). MIME composition and
authenticated SMTP submission are implemented for `send_message` and
were exercised against Purelymail; production hardening remains
outstanding.

Phase 2.1 ships:

- IMAP4rev1 read-only backend with TLS, EXAMINE-based read-only
  selection, and `BODY.PEEK[]` body fetches (no implicit `\Seen`).
- Strict UIDVALIDITY handling: `uid_validity` is mandatory on
  `read_message`; a stale or missing value is rejected before any
  body is read.
- MIME parsing via `go-message`: plain text, HTML, multipart,
  nested multipart, character encodings, attachment metadata,
  inline attachments, malformed-input handling.
- Per-account safety limits: max message bytes, max attachment
  bytes, max total attachment bytes, max MIME nesting, max MIME
  parts, max attachments.
- Reconnect safety: a fresh EXAMINE re-validates UIDVALIDITY; no
  mutating operation is silently retried.

**Phase 2.2** is complete. The plan and acceptance criteria are in
[`docs/PHASE2.2.md`](docs/PHASE2.2.md); the design is in
[`docs/PHASE2_DESIGN.md`](docs/PHASE2_DESIGN.md) §11.

| Priority | Scope | Status |
|---|---|---|
| 1 | Protocol-level IMAP literal-size enforcement (the §10 requirement) | **DONE** (PR #5) |
| 2 | IMAP search correctness defects | **DONE** (PR #6) |
| 3 | Secure attachment retrieval | **DONE** (PR #8) |

Outbound `send_message` is implemented in Phase 2.3 PR 3, but
reply/forward and mutating mailbox operations remain unimplemented.
Phase 2.3 PR 1 prepared the configuration, recipient-policy, and
confined attachment-resolution foundations; PR 2 added the bounded MIME
composer; PR 3 added the authenticated SMTP sender.

**Phase 3 (Hardening)** covers the remaining production gates. This
change adds bounded inbound MCP request framing; bounded IMAP list/search
workloads, explicit IMAP command timeouts, `has_attachments` for IMAP
summaries, response-size limits, fuzz tests, installation docs, and a
cross-platform release remain Phase 3 work. The Purelymail smoke test
completed on 2026-10-21; IMAP credential-command buffering and other
hardening items remain Phase 3 work. The smoke test is not production
certification.

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
[`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md) and
[`docs/TOOLS.md`](docs/TOOLS.md) for the design.

## Install

TheMailroom is a single static binary. Build it from source:

```sh
go build -o bin/themailroom ./cmd/themailroom
./bin/themailroom -version
```

Cross-platform builds use the standard Go toolchain. No CGo is required.

## Configure

Create a configuration file (default: `./themailroom.yaml`). The schema is
documented in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#29-configuration-internalconfig)
and validated on startup. The minimal example uses the in-memory backend:

```yaml
# themailroom.yaml
server:
  log_level: info
  max_request_bytes: 33554432             # optional; 32 MiB default, max 128 MiB

accounts:
  - id: personal
    backend: memory                       # for dev / tests
    email: user@example.test
    mailbox_root: ./testdata/messages     # directory of .eml files
    permissions:
      - read
```

For real IMAP/SMTP, see [`examples/themailroom.yaml`](examples/themailroom.yaml).

### Credential handling

TheMailroom supports two credential sources, in order of preference:

1. **`password_command`** — an external command whose stdout is the
   password. This is the recommended path because secrets never live in
   the config file.

   ```yaml
   imap:
     host: imap.example.test
     username: user@example.test
     password_command: ["pass", "email/imap"]   # gpg/pass
   ```

2. **`password`** — a literal string in the config. Use only as a last
   resort; the file should be unreadable to other users
   (`chmod 600 themailroom.yaml`).

No matter which mechanism is used, the server does not print a resolved
password. SMTP credential-command output is bounded and excluded from
errors and logs. The pre-existing IMAP `password_command` path is
pending hardening: it uses unbounded stdout/stderr buffers and includes
truncated stderr in internal errors, although tool errors are
mapped to generic messages. Logs go to stderr so they do not contaminate
the stdio JSON-RPC stream.

### App passwords and 2FA

Provider-issued app passwords are accepted as ordinary opaque password
strings. Use the provider's app password as the value of `password`, or
return it from `password_command`; there is no separate `app_password`
field. Use the exact provider-issued value. `password_command` output has
trailing CR/LF trimmed but preserves internal spaces and punctuation.

IMAP and SMTP credentials are independent. They may use different
usernames, different app passwords, or different credential sources:

```yaml
imap:
  username: user@example.test
  password_command: ["pass", "email/imap"]
smtp:
  username: user@example.test
  password_command: ["pass", "email/smtp"]
```

App passwords are compatible with TheMailroom's supported mechanisms:
IMAP uses `LOGIN` over verified TLS, and SMTP uses AUTH PLAIN after
verified TLS. Providers that require OAuth2/XOAUTH2 or another
unsupported mechanism are not usable with app passwords alone. This
includes, for example, Microsoft 365 tenants where basic authentication
is disabled and Google Workspace domains where app passwords are
disabled by policy. IMAP servers that require `AUTHENTICATE` instead of
`LOGIN`, SMTP servers that do not advertise AUTH PLAIN, and passkey-only
accounts without an app-password feature are likewise unsupported.
OAuth2 remains deferred (see
[`docs/PHASE2_DESIGN.md`](docs/PHASE2_DESIGN.md) §6).

A literal `password` is stored in plaintext in the config file. Prefer
`password_command`, and restrict the config file (`chmod 600
themailroom.yaml`). TheMailroom does not provide a keyring or enforce
file permissions.

### Permissions

Every capability is disabled by default. To enable anything beyond read,
add it explicitly:

```yaml
permissions:
  - read
  - send           # required for send_message outbound submission
  - modify         # flag, move, archive
  - delete         # move to Trash or expunge
  - attachment_send  # required to send messages that include local attachments
```

The server refuses any operation whose capability is missing,
independently of what the MCP client requests.

Accounts with `send` must have a complete, secure SMTP configuration:
host, port, exactly one of `password` / `password_command`, and either
`tls: true` (implicit TLS) or `starttls: true` (mandatory STARTTLS).
Plaintext SMTP is rejected. Read-only accounts are not forced to
configure SMTP.

Outbound recipient and attachment controls are configured per account:

```yaml
allowed_recipients:              # optional exact allow-list; empty = any
  - alice@example.test
  - Sales@example.test           # local-part case is significant
limits:
  max_recipients: 50             # unique To + Cc + Bcc recipients
  max_attachment_bytes: 6291456  # decoded bytes per local attachment
  max_total_attachment_bytes: 26214400
attachment_send_paths:
  - /srv/themailroom/outbound    # absolute, existing directory
```

Recipient matching is exact after RFC 5322 parsing: local-part case is
preserved and significant, while domain case is not. There are no
wildcard or substring rules. Local attachment references are relative
paths resolved only inside `attachment_send_paths`; absolute paths,
traversal, symlink escapes and non-regular files are rejected. The
outbound resolver, recipient policy, MIME composer and SMTP sender are
wired into `send_message`. Preflight runs completely before any SMTP
connection: account lookup, `send`, a raw recipient count/byte budget,
recipient policy and limits, `attachment_send` when needed, confined
attachment resolution, MIME composition and final `max_message_bytes`
enforcement. The composer
bounds header preparation by the same limit, omits Bcc from headers and
returns the Message-ID used by the sender.

`send_message` uses one short-lived SMTP connection per call. It supports
implicit TLS or mandatory STARTTLS, verifies the configured hostname and
certificate, requires TLS 1.2+, and authenticates only with AUTH PLAIN
after TLS. All unique To/Cc/Bcc recipients are sent as envelope `RCPT TO`
before DATA; Bcc never appears in headers. A successful result means the
SMTP server accepted the message after DATA, not that it reached every
mailbox. There is no automatic Sent copy, durable queue, prepared-message
replay API or retry, and SMTPUTF8 envelope submission is unsupported. A
failure after DATA begins is reported as an ambiguous submission with the
original Message-ID and must not be retried automatically.

## Run

```sh
./bin/themailroom -config /etc/themailroom/themailroom.yaml
```

The process serves on stdio and exits when the parent closes stdin or
sends `SIGINT`/`SIGTERM`. All diagnostic output goes to stderr.

## Pointing an MCP client at TheMailroom

Example for Claude Desktop (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "themailroom": {
      "command": "/usr/local/bin/themailroom",
      "args": ["-config", "/etc/themailroom/themailroom.yaml"]
    }
  }
}
```

Other MCP clients use the same `command` + `args` shape.

## Security

TheMailroom treats all email content as untrusted data. Message bodies
are returned as separate `text` and `html` fields; they are never
concatenated into a string that could be confused with instructions.

Outbound submission is authenticated and TLS-only. Credentials are
never placed in tool responses or logs, SMTP server responses are mapped
to typed errors, and ambiguous post-DATA outcomes are never reported as
definite non-delivery or retried. The full threat catalogue and
mitigations are in [`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md).

Inbound stdio MCP frames are capped by `server.max_request_bytes`
(default 32 MiB; hard maximum 128 MiB). The limit counts UTF-8 payload
bytes excluding LF or CRLF framing and is enforced before the SDK's JSON
decoder allocates the message. The 128 MiB upper bound is four times the
default and leaves room for JSON escaping of a default-size 25 MiB
message while keeping a single decoder allocation bounded. Exactly the
limit is accepted; one more byte closes the connection without
dispatching a tool. Each physical line must contain one complete JSON
value; multi-line pretty-printed JSON is rejected. The setting is
independent of an account's `max_message_bytes`: one bounds a single
pre-decode MCP allocation, the other bounds email content after parsing.
It is a per-frame allocation boundary, not an aggregate process-memory
limit. The SDK handler queue, concurrent in-flight requests, tool-level
argument amplification and response sizes remain separate Phase 3
hardening concerns. The IMAP credential-command buffering issue noted
above is also Phase 3 work.

**IMAP client dependency.** TheMailroom depends on
`github.com/flowbyte-com/go-imap` `v1.2.1-themailroom.4`, a
narrow fork of `github.com/emersion/go-imap` v1.2.1 that adds
protocol-level IMAP literal-size enforcement (the cap is applied
before the literal buffer is allocated). The fork is pinned by
version; there is no `replace` directive. **Maintenance
requirement:** the fork must be upstreamed to
`github.com/emersion/go-imap` or replaced with a different IMAP
client that exposes a literal-size cap natively. A long-term
dependency on a personal fork is itself a maintenance risk. See
[`docs/PHASE2_DESIGN.md`](docs/PHASE2_DESIGN.md) §8 and the
fork's `CHANGES.md`.

Reporting security issues: please open a private issue or email the
maintainer before public disclosure.

## Development

```sh
go test ./...           # all unit tests
go vet ./...            # static analysis
go test -race ./...     # with race detector
```

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the layered
design.

## License

[AGPL-3.0](LICENSE).