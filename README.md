<!-- README standard: filmcraft-inspired/v4; profile: backend -->

<h1 align="center">icloud-mcp</h1>

<p align="center"><b>iCloud Calendar, Contacts, and Mail through one stdio MCP server.</b></p>

<p align="center">
  <a href="#get-started">Get started</a> |
  <a href="#mcp-tools">MCP tools</a> |
  <a href="#configuration">Configuration</a> |
  <a href="#documentation">Documentation</a>
</p>

Connect an MCP host to iCloud with one Go binary. Calendar uses CalDAV; optional
Contacts and Mail use CardDAV, IMAP, and SMTP. The host starts the process and
exchanges JSON-RPC through stdin and stdout.

<a id="how-this-differs"></a>
<!-- readme:section positioning -->
## Why icloud-mcp?

Use this server when an MCP host needs to search calendars, find free time, or
work with iCloud contacts and messages on a headless machine. It uses
app-specific passwords and remote protocols, without macOS frameworks, browser
automation, or private Apple APIs.

Capability gates let you start with Calendar reads and enable other operations
as needed. Each domain has its own protocol clients and network policy. All
enabled domains share one process: a process compromise can expose all loaded
credentials. Use separate processes and account configurations for stronger
isolation.

<a id="security-summary"></a>
<!-- readme:section privacy -->
## Privacy and security

**Accounts:** An Apple Account with iCloud Calendar and an app-specific password is required. Contacts uses the same identity. Optional Mail needs a full iCloud Mail address and can use a separate app-specific password. The server has no separate product account; the MCP host has its own account requirements.

**Telemetry:** The server has no telemetry or crash-reporting service. It writes operational logs and mutation audit records to stderr. The host controls log retention and has its own telemetry policy.

**Data:** Credentials are loaded at startup. Discovery state and keyed update results stay in process memory; the server writes no local data files. It sends authenticated requests to iCloud and returns account content to the MCP host. That host controls any further storage or transmission to a model provider.

- **Fixed destinations.** Calendar and Contacts use their iCloud hosts and
  validated shards on port 443. Mail uses `imap.mail.me.com:993` and
  `smtp.mail.me.com:587`, with mandatory STARTTLS for SMTP. TLS 1.2 or later is
  required. DAV ignores proxy environment variables.
- **Redaction and audit.** The server redacts configured identities, passwords,
  and Basic/SASL forms from errors, results, and logs. Mutation audits contain
  `domain`, `resourceType`, and a process-local HMAC `resourceToken`, without raw
  resource paths, UIDs, mailboxes, or recipients.
- **Host permissions.** Remote text can contain prompt injection. A compromised
  host model can invoke every registered tool. Start with global read-only mode
  and enable only the capabilities you need.

See the [threat model and reporting policy](SECURITY.md) and
[security implementation](docs/security.md).

<!-- readme:section capabilities -->
## Capability gates

| Capability | Operations | Enablement |
|---|---|---|
| Calendar | List calendars; search, get, and validate events; find free slots; create, update, and delete events | Always on; writes enabled by default |
| Contacts | List books; search and get contacts; create, update, and delete vCard 3.0 contacts | `ICLOUD_MCP_ENABLE_CONTACTS=true` |
| Mail read | List mailboxes; search messages; get plain text and attachment metadata | `ICLOUD_MCP_ENABLE_MAIL=true` |
| Mail mutation | Change flags; move or trash a message | Mail plus `ICLOUD_MCP_ENABLE_MAIL_WRITE=true` |
| Mail send | Submit a plain-text message through SMTP | Mail plus `ICLOUD_MCP_ENABLE_MAIL_SEND=true` and a recipient policy |

`ICLOUD_MCP_READ_ONLY=true` removes every mutation and send tool from
`tools/list`. It does not enable a disabled read domain. Contacts has no separate
write flag. Mail mutation and send have independent gates. A send policy is
required whenever the send flag is set, including under global read-only.

<!-- readme:section interface -->
## MCP tools

The server registers **10 tools by default**, **7 with global read-only and
optional domains off**, and **23 with all gates on**. Disabled tools are absent
from discovery. The MCP interface provides tools over **stdio**.

| Tool | Input | Result | Account changes |
|---|---|---|---|
| `icloud_capabilities` | No arguments | Enabled domains, effective capabilities, version, tool names and count | None; the call uses local state |
| `list_calendars` | No arguments | Calendar names and paths for subsequent calls | None |
| `search_events` | `start`, `end`; optional calendar and filters | Sorted event page, recurrence occurrences, and truncation or failure warnings | None |
| `update_event` | `calendar`, `uid`, changed fields; optional `etag`, `idempotency_key`, occurrence scope | Updated event or a structured error | Conditional Calendar write |
| `get_message` | `mailbox`, `uid_validity`, `uid`; optional body limit | Message metadata and bounded plain text | None; EXAMINE and PEEK preserve read state |
| `send_message` | `body`, optional `subject`, and at least one recipient across `to`, `cc`, `bcc` | Submission status or a structured error | Sends mail to allowed recipients |

The [complete inventory](docs/architecture.md#tool-inventory) lists all tools.
Discover their argument schemas with the host's tool list. Domain failures set
the MCP error flag and include a JSON `code` in text content. Follow the
[error and recovery reference](docs/error-codes.md).

<a id="quick-start"></a>
<!-- readme:section get-started -->
## Get started

This first run enables Calendar reads only. You need an MCP host, network
access to iCloud, and an Apple Account with two-factor authentication to
[create an app-specific password](https://support.apple.com/en-us/102654).
Never use the main Apple Account password.

### Install the binary

With **Go 1.26.0 or newer**:

```sh
go install github.com/ThomasCrouzet/icloud-mcp/cmd/icloud-mcp@latest
go env GOPATH GOBIN
```

The executable is installed in `GOBIN` when set, otherwise in `bin` under
`GOPATH` (normally `$HOME/go/bin`). Use its absolute path in the host config.

Without Go, use a [release archive](https://github.com/ThomasCrouzet/icloud-mcp/releases)
for Linux amd64/arm64 or macOS arm64. Archives include checksums, cosign
signatures, and certificates. Follow the
[release verification procedure](docs/testing.md#release-metadata-and-registry).

### Prepare credentials

Store the Apple Account identity and app-specific password in separate files
outside the repository. Each must be a regular file of at most 4 KiB, with mode
`0600` or stricter. The host user must be able to read them. Use absolute
`file://` paths; the server reads these files only at startup.

### Connect Claude Desktop

This example uses **Claude Desktop on macOS**. Open **Settings > Developer >
Edit Config**. Its user configuration file is
`~/Library/Application Support/Claude/claude_desktop_config.json`; see the
[official local-server setup guide](https://modelcontextprotocol.io/docs/develop/connect-local-servers)
and [environment configuration](https://modelcontextprotocol.io/docs/tools/debugging#environment-variables).
Merge this entry into `mcpServers`, with your actual absolute paths:

```json
{
  "mcpServers": {
    "icloud": {
      "command": "/Users/you/go/bin/icloud-mcp",
      "env": {
        "ICLOUD_EMAIL": "file:///Users/you/.config/icloud-mcp/email",
        "ICLOUD_PASSWORD": "file:///Users/you/.config/icloud-mcp/password",
        "ICLOUD_MCP_READ_ONLY": "true",
        "ICLOUD_MCP_DEFAULT_TZ": "Europe/Paris"
      }
    }
  }
}
```

Keep optional domains off for this run. Fully quit and reopen Claude Desktop.
The host launches the binary; Calendar discovery must succeed before MCP starts.
Open the host's connector tool list and confirm the `icloud` tools appear.

### Run your first operation

Ask the host:

```text
Call icloud_capabilities and confirm readOnly is true and toolCount is 7.
Then call list_calendars with no arguments and show the calendar names.
```

The first call confirms capability discovery. The second reads iCloud. It
returns `count` and a `calendars` array with `name` and `path` fields. An account
with no calendars returns an empty array. A synthetic protocol fixture returns:

```json
{
  "count": 1,
  "calendars": [
    {"path": "/fixture/calendars/home/", "name": "Fixture Calendar"}
  ]
}
```

This output comes from the repository fixture, not a live account. Your names,
paths, and count depend on your account. Next, ask the host to use
`search_events` for the next seven days.

For another host, configure a custom stdio process with the same command and
environment; its configuration format can differ. See
[host integration](docs/agent-hosts.md).

<!-- readme:section configuration -->
## Configuration

The binary reads exactly **12 product environment variables** from its process
environment. It does not load `.env` files or host configuration files.
[`.env.example`](.env.example) lists the supported keys. The host or launching
shell must supply them; restart the process after a change.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `ICLOUD_EMAIL` | Yes | None | Calendar and Contacts identity; supports `file://` |
| `ICLOUD_PASSWORD` | Yes | None | App-specific password; supports `file://` |
| `ICLOUD_MCP_READ_ONLY` | No | `false` | Remove every mutation and send tool |
| `ICLOUD_MCP_LOG_LEVEL` | No | `info` | Operational stderr log level |
| `ICLOUD_MCP_DEFAULT_TZ` | No | `UTC` | IANA zone for Calendar times without an offset and recurring-write fallback |
| `ICLOUD_MCP_ENABLE_CONTACTS` | No | `false` | Enable Contacts; writes also require read-only to be off |
| `ICLOUD_MCP_ENABLE_MAIL` | No | `false` | Enable Mail reads |
| `ICLOUD_MAIL_ADDRESS` | When Mail is on | None | Full iCloud Mail address; supports `file://` |
| `ICLOUD_MAIL_PASSWORD` | No | `ICLOUD_PASSWORD` | Separate Mail app-specific password; supports `file://` |
| `ICLOUD_MCP_ENABLE_MAIL_WRITE` | No | `false` | Enable IMAP mutations; requires Mail |
| `ICLOUD_MCP_ENABLE_MAIL_SEND` | No | `false` | Enable SMTP send; requires Mail and a recipient policy |
| `ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS` | When send is requested | None | Comma-separated exact addresses, or literal `*` |

Boolean values accept unset, `0`, `false`, `1`, and `true`; the parser trims
whitespace and ignores case. Invalid values fail before network access. Pass
environment values as strings, including quoted booleans in YAML. Mail write
or send without Mail is a boot error. Prefer exact recipient addresses: literal
`*` permits any syntactically valid recipient and causes a boot warning.

For Calendar times, use an explicit RFC3339 offset or a local time without an
offset in `ICLOUD_MCP_DEFAULT_TZ`. Do not label a local hour with `Z`. Timed
results include an explicit numeric offset; all-day dates use `YYYY-MM-DD`.
See the [full configuration and date rules](docs/agent-hosts.md#configuration-reference)
for log-level forms and optional-domain examples.

<!-- readme:section operations -->
## Operations

| Area | Behavior |
|---|---|
| Lifecycle | Configuration validates before network access. Calendar discovery runs at boot; Contacts discovery is lazy. Mail uses fresh sessions. EOF and SIGTERM stop the stdio process. Use a separate process for each account. |
| Logs | stdout is reserved for JSON-RPC. Operational logs and mutation audits go to stderr. `-audit-format=json` is the default; `text` is also supported. |
| Health | Disabled by default. `-health 127.0.0.1:8797` enables loopback-only `/healthz` and `/status` with domain status and rate limits. These are monitoring endpoints; MCP remains stdio. |
| Read retries | Calendar and Contacts retry bounded HTTP status failures. A transient IMAP read can use one replacement session. |
| Mutations | Calendar/Contacts writes, IMAP mutations, and SMTP never replay automatically. An `outcome_unknown` result requires reconciliation before another mutation. |
| Update keys | `update_event` and `update_contact` retain keyed results in memory. Known results last 15 minutes; uncertain results remain until process exit. Restart removes the cache without resolving outcomes. |

Use strong ETags for Calendar and Contacts updates and deletes. Creates use
`If-None-Match: *`; updates and deletes use a strong `If-Match`. For creates,
`client_uid` or its `idempotency_key` alias identifies the resource. For keyed
updates, follow the [cache contract](docs/architecture.md#update-idempotency)
and [recovery procedure](docs/error-codes.md#update-idempotency-and-recovery).

### Limits

Tools have a 25 s deadline and DAV a 30 s HTTP timeout. Stdio frames are limited
to 1 MiB and MCP results to 256 KiB. Calendar and Contacts each allow 60 reads
and 20 writes per minute. Mail has separate limits of 60 reads, 20 mutations,
and 20 sends per minute. See the [public limits](docs/architecture.md#public-limits)
for search, parser, recurrence, and concurrency bounds.

<!-- readme:section documentation -->
## Documentation

| Guide | Contents |
|---|---|
| [Host integration](docs/agent-hosts.md) | Secrets, all configuration keys, dates, health, multiple accounts, caller behavior |
| [Architecture](docs/architecture.md) | Startup, boundaries, full tool inventory, limits, update idempotency |
| [Error codes](docs/error-codes.md) | Structured errors, retry rules, mutation reconciliation |
| [CalDAV compatibility](docs/caldav-compatibility.md) | Calendar discovery, conditional writes, recurrence |
| [CardDAV compatibility](docs/carddav-compatibility.md) | Contacts discovery, search, vCard formats and writes |
| [Mail compatibility](docs/mail-compatibility.md) | IMAP identities, sessions, MIME bounds, mutations, SMTP |
| [Security policy](SECURITY.md) and [implementation](docs/security.md) | Threat model, private reporting, technical controls |
| [Testing](docs/testing.md) | Local checks, protocol evidence, CI, live integration, release verification |
| [Releases](https://github.com/ThomasCrouzet/icloud-mcp/releases) and [Changelog](CHANGELOG.md) | Published archives and version history |

### Build and test

From the repository root, use Go 1.26.0 or newer and Make. Build and test need
no iCloud account or running service. CI and release builds pin Go 1.26.10.

```sh
make build        # bin/icloud-mcp, host Go; version defaults to dev
make test         # go test ./... -race -cover
make lint         # go vet and pinned golangci-lint v2.15.0
```

`make release-all VERSION=vX.Y.Z` packages Linux amd64/arm64 and macOS arm64
with the host toolchain. `make release VERSION=vX.Y.Z` uses a pinned Docker
image for Linux arm64. Replace `vX.Y.Z` with a release version. Both require a
clean worktree, including untracked files, and refuse to overwrite output.
Tag releases build after CI and gitleaks pass. Live integration tests require
credentials and explicit gates; see [Testing](docs/testing.md).

### Contributing

Follow [CONTRIBUTING.md](CONTRIBUTING.md). Run `gofmt`, `make test`, and
`make lint` before opening a pull request. Report bugs through
[GitHub Issues](https://github.com/ThomasCrouzet/icloud-mcp/issues), with the
version, enabled domains, reproduction steps, and redacted errors. Report
vulnerabilities privately through [SECURITY.md](SECURITY.md).

<!-- readme:section status -->
## Status

The implemented scope is iCloud Calendar with optional Contacts and Mail.
Release archives cover Linux amd64/arm64 and macOS arm64; CI also smoke-builds
Windows amd64, which has no published archive.

Current limits to account for:

- **Calendar.** Recurring operations support a full series or one occurrence.
  `this-and-future` and RDATE are unsupported. A search range is at most 366 days.
- **Contacts.** Reads accept vCard 3.0 and 4.0. Writes support 3.0 only; vCard
  4.0 and groups are read-only. Full-object updates preserve PHOTO and unknown
  fields.
- **Mail.** Message identity is `(mailbox, UIDVALIDITY, UID)`. Reads return
  plain text and attachment metadata, without HTML or attachment bodies.
  `move_message` rejects SPECIAL-USE Trash and Junk destinations; use
  `trash_message` for intentional deletion. When CONDSTORE is advertised,
  conditional flag updates fail with `protocol_error` before STORE because the
  beta IMAP adapter cannot safely observe tagged MODIFIED responses.
- **Scope.** Reminders, Notes, Photos, Drive, Find My, Keychain, Messages, and
  Home are outside the supported scope. Modern Reminders are not exposed by
  this CalDAV connector.

<!-- readme:section architecture -->
## Architecture

![A stdio MCP client connects to capability gates and separate Calendar, Contacts, IMAP, and SMTP clients.](docs/diagrams/architecture.svg)

Each domain keeps separate credentials, transports or dialers, rate limiters,
and semaphores. Shared code handles redaction, audit, result bounds, and the
process lifecycle. See the [package map](docs/architecture.md#packages).

### Direct dependencies

The single Go module has exactly **10 direct dependencies**. Exact versions
are in [go.mod](go.mod); required notices are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

| Dependency | Purpose |
|---|---|
| `github.com/emersion/go-webdav` | CalDAV primitives; custom discovery and conditional requests preserve iCloud behavior |
| `github.com/emersion/go-ical` | iCalendar parsing and encoding |
| `github.com/mark3labs/mcp-go` | MCP stdio transport, schemas, and JSON-RPC |
| `github.com/teambition/rrule-go` | Bounded recurrence expansion with timezone preservation |
| `golang.org/x/time` | Independent domain rate limiters |
| `github.com/emersion/go-vcard` | vCard 3.0/4.0 reads and 3.0 writes |
| `github.com/emersion/go-imap/v2` | IMAP through the narrow `internal/mail/imapadapter` boundary |
| `github.com/emersion/go-message` | MIME parsing and bounded plain-text handling |
| `github.com/emersion/go-smtp` | SMTP submission with STARTTLS |
| `github.com/emersion/go-sasl` | SASL PLAIN authentication after STARTTLS |

<a id="license"></a>
<!-- readme:section license -->
## License and credits

MIT. Copyright (c) 2026 Thomas Crouzet. See [LICENSE](LICENSE) and
[third-party notices](THIRD_PARTY_NOTICES.md).

### Attribution

Calendar tool shape and several patterns were inspired by
[`github.com/roygabriel/mcp-icloud-calendar`](https://github.com/roygabriel/mcp-icloud-calendar)
(MIT, copyright 2026 Gabe). Code was rewritten, not copied.
