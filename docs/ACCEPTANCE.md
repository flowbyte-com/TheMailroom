# Onboarding acceptance

This document defines how a fresh environment is accepted for TheMailroom,
and what still has to be checked by hand against a real mail provider.

There are two levels:

- **Local acceptance** is automated, deterministic and required. It uses the
  repository's fixtures and needs no network and no mail account.
- **Live-provider acceptance** is manual, explicitly authorised and not part
  of the automated gate.

## Version scope

The published `v1.0.0-rc.1` predates the `setup`, `doctor` and `-preset`
work. **Do not run this acceptance against that binary**: the commands it
exercises are not in it. Build from the current `main` commit, or use a later
release that contains all FW-12 changes.

Confirm before anything else:

```sh
themailroom -version
themailroom setup -h
themailroom doctor -h
```

The first command must report the commit you intended to test.

## Local acceptance (required)

```sh
go test ./internal/acceptance/ -run TestOnboardingAcceptance -v
```

The test builds the command from the current commit **with the release
linker flags**, so the version identity check is meaningful rather than
reporting the development defaults.

It covers, in order:

1. Build the binary from the intended commit.
2. Verify `-version` reports that version, commit and build date.
3. Create a clean configuration destination.
4. Run the generic `setup` interview.
5. Confirm the generated file is accepted by the production configuration
   loader (`config.Parse`).
6. Run local `doctor` and require exit status 0.
7. Confirm the generic configuration grants only `read`: no SMTP block, no
   attachment-sending paths.
8. Repeat with `-preset purelymail`.
9. Confirm the preset defaults — `imap.purelymail.com`, port 993, implicit
   TLS — and that a custom-domain address becomes the username unchanged.
10. Start the MCP server over stdio.
11. Complete the `initialize` handshake.
12. Call `tools/list` and require exactly the seven expected tools.
13. Exercise a read-only workflow against the deterministic fixtures:
    `list_accounts` → `list_mailboxes` → `list_messages` → `read_message`,
    using the `uid_validity` returned by the listing.
14. Shut the server down cleanly and require no error.
15. Demonstrate the negative cases:
    - an invalid configuration makes `doctor` exit non-zero;
    - `send_message` on a read-only account returns a tool error naming the
      missing capability;
    - `SIGINT` during setup interrupts the blocked prompt within five
      seconds, writes no configuration and leaves no temporary files.

Everything is local. No credential command is executed, no socket is opened
and no message is sent.

## Manual acceptance (optional spot check)

The same steps by hand, with a throwaway destination:

```sh
tmp="$(mktemp -d)"
themailroom setup -config "$tmp/themailroom.yaml"
themailroom doctor -config "$tmp/themailroom.yaml"
themailroom -config "$tmp/themailroom.yaml"   # then send initialize / tools/list over stdin
```

`example` domains and `example.test` addresses are safe placeholders; never
point a hand test at a real mailbox unless you intend to.

## Live-provider acceptance (manual, authorised only)

This is a separate exercise and is **not** covered by the automated gate. A
passing local acceptance says nothing about a real provider.

### Preconditions

- Separate, explicit authorisation for the test.
- A designated test recipient that the operator owns. **No message may be
  sent to any other address.**
- Credentials supplied through a credential command, never pasted into a
  chat or a configuration file.
- The account is configured read-only first. Sending is enabled only for the
  send step and only when the provider supports it.

### Providers

1. **Purelymail**, configured with `themailroom setup -preset purelymail`.
2. **A second, independent IMAP/SMTP provider**, configured generically with
   no preset. It must not reuse Purelymail's servers.

### Procedure per provider

1. `doctor -config <file>` — local checks pass.
2. `doctor -config <file> -network` — record the result. This runs the
   credential command and opens connections.
3. Verify authentication succeeds for IMAP.
4. Verify read-only listing: `list_accounts`, `list_mailboxes`,
   `list_messages` on a small mailbox, and `read_message` for one message
   using the `uid_validity` from the listing.
5. If the account has the `send` capability and sending is authorised, send
   one message to the designated test recipient only, and confirm the tool
   reports SMTP acceptance. Acceptance is not delivery.
6. If sending is not supported or not authorised, record that and stop.
7. Record the provider's behaviour: TLS mode, authentication mechanism,
   mailbox naming, any search or listing quirks.

### Recording incompatibilities

Record a provider incompatibility rather than adding provider-specific
protocol workarounds. TheMailroom is provider-agnostic; a quirk that needs a
special case is a finding to review, not a patch to apply during acceptance.

### What live acceptance must not do

- It must not send to any address other than the designated test recipient.
- It must not change mailbox state, or delete or move messages.
- It must not retry an ambiguous submission.
- It must not store credentials anywhere except the operator's credential
  source.

## Deferred

The following are deliberately out of scope for this procedure and remain
open:

- Live acceptance against Purelymail and a second provider. It requires
  authorisation, a test account and a designated recipient.
- OAuth2-only providers, which TheMailroom does not support.
- Windows write support for `setup`, which fails closed because the standard
  library cannot set an access control list.
