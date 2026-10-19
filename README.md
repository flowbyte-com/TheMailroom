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

**Phase 1 (Foundation)** is implemented in this branch:

- MCP server over stdio using the official
  [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
- Read-only tool surface: `list_accounts`, `list_mailboxes`,
  `list_messages`, `read_message`, `search_messages`.
- YAML configuration with credential-via-command (`pass`, `secret-tool`).
- Permission and limit enforcement server-side.
- An in-memory backend (loads `.eml` files) for tests and local
  development.

**Phase 2 (Mail functionality)** will add IMAP/SMTP wiring, MIME
composition, replies, attachments, and protected modifications.

**Phase 3 (Hardening)** will add race tests, fuzz tests,
installation docs, and a cross-platform release.

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

No matter which mechanism is used, the server will **never** print a
resolved password. Errors are redacted. Logs go to stderr so they do not
contaminate the stdio JSON-RPC stream.

### Permissions

Every capability is disabled by default. To enable anything beyond read,
add it explicitly:

```yaml
permissions:
  - read
  - send           # compose and send new messages (Phase 2)
  - modify         # flag, move, archive
  - delete         # move to Trash or expunge
  - attachment_send  # required to send messages that include attachments
```

The server refuses any operation whose capability is missing,
independently of what the MCP client requests.

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

The full threat catalogue and mitigations are in
[`docs/THREAT_MODEL.md`](docs/THREAT_MODEL.md).

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