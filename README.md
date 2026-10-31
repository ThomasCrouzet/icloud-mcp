# icloud-mcp

[![CI](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ThomasCrouzet/icloud-mcp)](https://github.com/ThomasCrouzet/icloud-mcp/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Access **iCloud Calendar, Contacts, and Mail** through
[Model Context Protocol](https://modelcontextprotocol.io). One Go binary serves
JSON-RPC over **stdio** and connects through CalDAV, CardDAV, IMAP, and SMTP.

![A stdio MCP client connects to capability gates and four separate iCloud protocol clients.](docs/diagrams/architecture.svg)

The server runs headless with app-specific passwords. It uses remote protocols,
without macOS frameworks, browser automation, or private Apple APIs.
Release archives cover Linux amd64/arm64 and macOS arm64.

Reminders, Notes, Photos, Drive, Find My, Keychain, Messages, and Home are outside
the supported scope. Modern Reminders do not use this server's CalDAV connector.

## Quick start

1. Install the binary.

```bash
go install github.com/ThomasCrouzet/icloud-mcp/cmd/icloud-mcp@latest
```

Prebuilt archives are on [GitHub Releases](https://github.com/ThomasCrouzet/icloud-mcp/releases).
Each archive has a SHA-256 checksum, a cosign signature, and a certificate. See
[release verification](docs/testing.md#release-metadata-and-registry).

2. Create an [app-specific password](https://appleid.apple.com). Never use the
main Apple Account password. Store the identity and the password in separate
regular files outside this repository. Give each file mode `0600` or stricter
and at most 4 KiB.

3. Set the two credential variables. Start in read-only mode for the first run.

```bash
export ICLOUD_EMAIL='file:///private/path/icloud-email'
export ICLOUD_PASSWORD='file:///private/path/icloud-password'
export ICLOUD_MCP_READ_ONLY=true
export ICLOUD_MCP_DEFAULT_TZ=Europe/Paris
```

4. Register the absolute binary path and environment in your MCP host. This
generic example uses the common `mcpServers` shape:

```json
{"mcpServers":{"icloud":{"command":"/home/you/.local/bin/icloud-mcp","env":{"ICLOUD_EMAIL":"file:///private/path/icloud-email","ICLOUD_PASSWORD":"file:///private/path/icloud-password","ICLOUD_MCP_READ_ONLY":"true","ICLOUD_MCP_DEFAULT_TZ":"Europe/Paris"}}}}
```

5. Restart the host and ask it: "Use `search_events` to list my events for the
next seven days." This read-only configuration exposes **7** tools.

Stdio carries JSON-RPC. Stderr carries logs and audit records. The server reads
credential files only at startup; Calendar discovery needs network access first.

## Capability gates

| Domain | Protocol | Default |
|--------|----------|---------|
| Calendar | CalDAV HTTPS | Always on (read + write unless global read-only) |
| Contacts | CardDAV HTTPS | Off until `ICLOUD_MCP_ENABLE_CONTACTS` |
| Mail read | IMAP TLS | Off until `ICLOUD_MCP_ENABLE_MAIL` |
| Mail mutation | IMAP | Off until Mail + `ICLOUD_MCP_ENABLE_MAIL_WRITE` |
| Mail send | SMTP STARTTLS | Off until Mail + `ICLOUD_MCP_ENABLE_MAIL_SEND` + recipient policy |

`ICLOUD_MCP_READ_ONLY=true` removes every Calendar/Contacts write, every Mail
mutation, and Mail send. It does not enable a disabled read domain. Contacts has
no separate write flag. Mail read, mutation, and send are independent gates. Send
needs an exact recipient policy even under global read-only.

## How this differs

- Calendar, Contacts, and Mail are in one binary, with independent capability
  gates and separate protocol clients.
- The server is headless and uses app-specific passwords over public iCloud
  protocols. It needs no desktop app and no Apple developer account.
- It does not use EventKit, macOS frameworks, browser automation, or private
  Apple APIs.

## MCP tools

The server exposes at most **23** tools: **10** by default, **7** with global
read-only and optional domains off, and **23** with every optional gate on.
Disabled tools are absent from `tools/list`.

The full inventory is in the [architecture guide](docs/architecture.md#tool-inventory).
Protocol behavior and compatibility matrices are in the
[CalDAV](docs/caldav-compatibility.md), [CardDAV](docs/carddav-compatibility.md),
and [Mail](docs/mail-compatibility.md) documents.

Calendar update and delete operations support occurrences and use strong
`If-Match`. Contacts uses opaque book IDs and writes vCard 3.0. Mail uses
`(mailbox, UIDVALIDITY, UID)` identities, PEEK reads, and an exact recipient
policy for SMTP. `move_message` rejects SPECIAL-USE Trash and Junk destinations.
Use `trash_message` to delete a message. `set_message_flags` fails closed with
`protocol_error` when CONDSTORE is advertised.

Keyed updates keep results in process memory. See the
[cache contract](docs/architecture.md#update-idempotency) and
[recovery procedure](docs/error-codes.md#update-idempotency-and-recovery).

## Configuration

The server reads exactly **12** product environment variables, not `.env` files.
Booleans accept only unset, `0`, `false`, `1`, or `true`. Pass them as strings,
and quote them in YAML. Full reference, optional-domain examples, log levels,
and date rules: [configuration reference](docs/agent-hosts.md#configuration-reference).

## Security (summary)

Untrusted remote text can influence an LLM on the host. Labels do not form a
security boundary. A compromised model can call every **registered** tool.

- **Egress fixed:** Calendar and Contacts use their iCloud hosts and shards on
  port 443. IMAP uses `imap.mail.me.com:993`. SMTP uses
  `smtp.mail.me.com:587` with mandatory STARTTLS. Destinations are not
  configurable. The server ignores DAV proxy variables and requires TLS 1.2 or later.
- **Isolation:** each domain has separate credentials, transports, dialers,
  limiters, semaphores, and protocol stacks.
- **Secrets:** the server removes passwords and Basic or SASL forms from stderr,
  errors, and results. Boot-only `file://` reads require mode 0600 or stricter.
  The server uses no `os/exec`, telemetry, or disk write after boot.
- **Audit:** mutation logs include `domain`, `resourceType`, and an opaque HMAC
  `resourceToken`. They exclude raw paths, UIDs, mailboxes, and recipients.

Full policy: [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md).

## Limits

The tool deadline is 25 s, with a 30 s DAV timeout. Stdio frames are limited to
1 MiB and MCP results to 256 KiB. Calendar and Contacts allow 60 reads and 20
writes per minute. Mail allows 60 reads, 20 mutations, and 20 sends per minute.
Mutations and send are never replayed. See the
[public limits](docs/architecture.md#public-limits).

## Build and test

```bash
make build        # local host binary, VERSION defaults to dev
make test         # go test ./... -race -cover
make lint         # go vet + pinned golangci-lint
make release-all VERSION=v1.2.3  # linux/amd64, linux/arm64, darwin/arm64
```

Release targets need a clean worktree and a version such as `v1.2.3`. They
refuse to replace output. The project has exactly **10** direct dependencies:
[dependency table](docs/architecture.md#direct-dependencies). CI runs all
gates, and tags build after CI and gitleaks pass. See [Testing](docs/testing.md)
for protocol evidence and the release procedure.

## Documentation

| Document | Purpose |
|----------|---------|
| [Host integration](docs/agent-hosts.md) | Stdio setup, secrets, configuration, dates, health, multiple accounts |
| [Error codes](docs/error-codes.md) | Retry rules and mutation recovery |
| [Architecture](docs/architecture.md) | Startup, boundaries, tools, limits, dependencies, idempotency |
| [CalDAV compatibility](docs/caldav-compatibility.md) | Calendar discovery, conditional writes, recurrence, limits |
| [CardDAV compatibility](docs/carddav-compatibility.md) | Contacts discovery, vCard support, search, conditional writes |
| [Mail compatibility](docs/mail-compatibility.md) | IMAP sessions, message identity, mutation, SMTP submission |
| [Security policy](SECURITY.md) and [implementation](docs/security.md) | Threat model, reporting, and technical controls |
| [Testing](docs/testing.md) | Local checks, protocol evidence, CI, live gates |
| [Contributing](CONTRIBUTING.md) | Development, release, writing, testing, support |
| [Changelog](CHANGELOG.md) | Release history |

## Attribution

Calendar tool shape and several patterns were inspired by
[`github.com/roygabriel/mcp-icloud-calendar`](https://github.com/roygabriel/mcp-icloud-calendar)
(MIT, copyright 2026 Gabe). Code was rewritten, not copied. This server adds
hard per-domain egress, redaction, bounded parsers, conditional mutation,
Contacts, Mail, and no telemetry.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Run `gofmt`, `make test`, and `make lint`
before opening a pull request.

## License

MIT. See [LICENSE](LICENSE) and [third-party notices](THIRD_PARTY_NOTICES.md).
