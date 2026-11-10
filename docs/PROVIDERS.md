# Provider presets

TheMailroom is provider-agnostic. A **preset** is a small, optional set of
interview defaults for the `themailroom setup` command. It fills in the
servers and ports a provider recommends; it is not an integration, and it
changes nothing outside the setup interview.

```sh
themailroom setup -preset purelymail
```

Without `-preset`, the generic provider-neutral interview runs exactly as
before. The preset is never selected automatically from the email domain:
detection would be a guess, and a custom domain is a first-class case.

## Purelymail

The defaults come from Purelymail's published technical setup guide,
<https://purelymail.com/docs/setup/technical>.

| Setting | Default |
|---|---|
| IMAP host | `imap.purelymail.com` |
| IMAP port | `993` |
| IMAP transport | implicit TLS |
| SMTP host | `smtp.purelymail.com` |
| SMTP port | `465` |
| SMTP transport | implicit TLS |
| Username | the full email address entered in the interview |

**Sending is still off by default.** The SMTP defaults are only offered
after you answer yes to "Enable sending (SMTP)?", and the block is only
written when you do. Attachment sending is never granted by the setup
command.

### STARTTLS instead of implicit TLS

Purelymail also accepts submission on port `587` with STARTTLS. Choose the
STARTTLS option at the SMTP transport prompt; the suggested port becomes
`587`. Both transports are verified TLS, and there is no plaintext mode.

### Usernames are full email addresses

Purelymail expects the complete address as the login name, for example
`you@purelymail.com` or `you@your-custom-domain.test`. The interview
defaults the username to the address you entered, and you can override it.

### App passwords when two-factor authentication is enabled

If the account uses two-factor authentication, Purelymail requires an app
password for IMAP and SMTP instead of the account password. Create one in
the Purelymail account settings and point the credential command at it, for
example:

```sh
themailroom setup -preset purelymail
# at the credential prompt:
#   pass show mail/purelymail-app-password
```

Setup never asks for, prints or stores a plaintext password. It records the
credential command you name and reports it by its executable name only.

### Custom domains

Custom-domain addresses work without any change: enter the address, and the
username default becomes that address. A `@purelymail.com` suffix is not
required and is not assumed.

### What the preset does not do

The preset supplies interview defaults and nothing else. It does **not**:

- create or manage Purelymail accounts,
- add, verify or configure custom domains,
- create or change DNS records, SPF, DKIM or DMARC,
- create, store or rotate credentials or app passwords,
- verify that the account exists or that the password is correct,
- test the connection.

Connectivity is a separate, explicit step that runs the credential command
and opens network connections:

```sh
themailroom doctor -config /etc/themailroom/themailroom.yaml -network
```

The provider name is written into the configuration as the informational
`provider` field, which TheMailroom does not act on. Removing it changes
nothing about how the account behaves.
