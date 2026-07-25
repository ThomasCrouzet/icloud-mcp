# icloud-mcp

[![CI](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/ThomasCrouzet/icloud-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

Unified **Apple/iCloud** MCP server for **Calendar, Contacts, and Mail**: one
static Go binary, [Model Context Protocol](https://modelcontextprotocol.io)
JSON-RPC on **stdio**.

**Remote protocols only** (CalDAV, CardDAV, IMAP, SMTP with app-specific
passwords). Not macOS EventKit, AppleScript, browser automation, or a private
Apple API. Runs headless on Linux, macOS, or Windows; suitable for agents and
orchestration, not only a desktop chat app.

**Host-agnostic.** Any MCP client that can spawn a child with an environment and
wire stdin/stdout works: personal agents, Hermes, OpenClaw, IDE bridges,
runners, or other stdio hosts. No preferred model vendor, chat product, or
reseller. Configuration is the process environment only; the binary does not
parse host-specific config files or `.env`.

| Domain | Protocol | Default |
|--------|----------|---------|
| Calendar | CalDAV HTTPS | Always on (read + write unless global read-only) |
| Contacts | CardDAV HTTPS | Off until `ICLOUD_MCP_ENABLE_CONTACTS` |
| Mail read | IMAP TLS | Off until `ICLOUD_MCP_ENABLE_MAIL` |
| Mail mutation | IMAP | Off until Mail + `ICLOUD_MCP_ENABLE_MAIL_WRITE` |
| Mail send | SMTP STARTTLS | Off until Mail + `ICLOUD_MCP_ENABLE_MAIL_SEND` + recipient policy |

Reminders, Notes, Photos, Drive, Messages, and similar apps are **out of scope**
until Apple documents a suitable remote third-party connector. Details:
[Supported scope](#supported-scope).

## Quick start

```bash
go install github.com/ThomasCrouzet/icloud-mcp/cmd/icloud-mcp@latest
# or: make build
# or: make release VERSION=v0.3.0
# or: make release-all VERSION=v0.3.0
```

1. Create an [app-specific password](https://appleid.apple.com) (never the main
   Apple Account password).
2. Export a minimal environment (recommended first deploy: Calendar **read-only**):

```bash
export ICLOUD_EMAIL='you@icloud.com'
export ICLOUD_PASSWORD='your-app-specific-password'
export ICLOUD_MCP_READ_ONLY=true
export ICLOUD_MCP_DEFAULT_TZ=Europe/Paris   # owner IANA zone; default UTC
```

3. Register **command** = absolute path to `icloud-mcp` and this **env** in your
   MCP host (YAML, JSON, TOML, UI, or orchestrator; format is host-specific).
4. Stdio = JSON-RPC; **stderr** = logs and mutation audit. Reload the host after
   env changes.

With that config the process exposes **7 tools** (Calendar reads + local
helpers + `icloud_capabilities`). No Contacts or Mail client is constructed.

Optional domains (still host-agnostic `export` form):

```bash
# Contacts reads (writes also need ICLOUD_MCP_READ_ONLY=false)
export ICLOUD_MCP_ENABLE_CONTACTS=true

# Mail reads: IMAP identity may differ from ICLOUD_EMAIL (e.g. name@icloud.com)
export ICLOUD_MCP_ENABLE_MAIL=true
export ICLOUD_MAIL_ADDRESS='mailbox@icloud.com'
# export ICLOUD_MAIL_PASSWORD='...'   # optional; else copy of ICLOUD_PASSWORD

# Mail mutation (flags / move / trash); independent of send
export ICLOUD_MCP_ENABLE_MAIL_WRITE=true
export ICLOUD_MCP_READ_ONLY=false

# Mail send: requires exact recipient allowlist even under global read-only
export ICLOUD_MCP_ENABLE_MAIL_SEND=true
export ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS='alice@example.com,bob@example.net'
```

Boot-only `file://` secrets (only regular files up to 4 KiB; never re-read after
start):

```bash
export ICLOUD_EMAIL='file:///run/secrets/icloud-email'
export ICLOUD_PASSWORD='file:///run/secrets/icloud-password'
export ICLOUD_MAIL_ADDRESS='file:///run/secrets/icloud-mail-address'
export ICLOUD_MAIL_PASSWORD='file:///run/secrets/icloud-mail-password'
```

See [.env.example](.env.example) for the full 12-variable contract.

## MCP tools

Maximum **23** tools. Disabled tools are absent from `tools/list`; disabled
domain clients are not constructed.

| Count | When |
|------:|------|
| **10** | Default: Calendar read+write + `icloud_capabilities` |
| **7** | Global read-only, optional domains off (recommended first run) |
| **23** | Contacts + Mail read + mutation + send, read-only off |

`ICLOUD_MCP_READ_ONLY=true` removes every Calendar/Contacts write, every Mail
mutation, and Mail send. It does not enable a disabled read domain.

| Group | Tools |
|-------|--------|
| Global | `icloud_capabilities` |
| Calendar read | `list_calendars`, `search_events`, `get_event`, `find_free_slots`, `validate_event`, `calendar_capabilities` |
| Calendar write | `create_event`, `update_event`, `delete_event` |
| Contacts read | `list_address_books`, `search_contacts`, `get_contact` |
| Contacts write | `create_contact`, `update_contact`, `delete_contact` |
| Mail read | `list_mailboxes`, `search_messages`, `get_message` |
| Mail mutation | `set_message_flags`, `move_message`, `trash_message` |
| Mail send | `send_message` |

**Highlights:** occurrence-aware Calendar update/delete with strong `If-Match`;
Contacts opaque book IDs and vCard 3.0 writes; Mail identity
`(mailbox, UIDVALIDITY, UID)`, PEEK reads, SMTP exact-recipient policy.
`set_message_flags` fails closed with `protocol_error` when CONDSTORE is
advertised and tagged MODIFIED cannot be observed (go-imap beta.8). Full
behavior notes: [docs/caldav-compatibility.md](docs/caldav-compatibility.md),
[docs/carddav-compatibility.md](docs/carddav-compatibility.md),
[docs/mail-compatibility.md](docs/mail-compatibility.md).

## Configuration

Exactly **12** product environment variables:

| Variable | Default | Contract |
|----------|---------|----------|
| `ICLOUD_EMAIL` | none | Required Calendar/Contacts identity. `file://` supported. |
| `ICLOUD_PASSWORD` | none | Required app-specific password; Mail fallback. `file://` supported. |
| `ICLOUD_MCP_READ_ONLY` | `false` | Global mutation kill switch. |
| `ICLOUD_MCP_LOG_LEVEL` | `info` | Stderr level; accepted forms are documented below. |
| `ICLOUD_MCP_DEFAULT_TZ` | `UTC` | IANA zone for offset-less Calendar inputs and recurring-write fallback. |
| `ICLOUD_MCP_ENABLE_CONTACTS` | `false` | Contacts tools; writes only if not read-only. |
| `ICLOUD_MCP_ENABLE_MAIL` | `false` | Mail reads; requires Mail address/password. |
| `ICLOUD_MAIL_ADDRESS` | none | Full IMAP/SMTP address when Mail is on. `file://` supported. |
| `ICLOUD_MAIL_PASSWORD` | `ICLOUD_PASSWORD` | Optional dedicated Mail app password. `file://` supported. |
| `ICLOUD_MCP_ENABLE_MAIL_WRITE` | `false` | Three IMAP mutation tools. |
| `ICLOUD_MCP_ENABLE_MAIL_SEND` | `false` | `send_message` (independent of Mail write). |
| `ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS` | none | Required if send requested: exact addresses or literal `*`. |

Booleans accept only unset, `0`, `false`, `1`, or `true`. Invalid values fail at
boot. Config is validated **before** any network access: Mail write/send without
Mail, Mail without address/password, or send without recipient policy are boot
errors (including under read-only for the send policy). Global read-only can
coexist with write/send flags but suppresses their registration.

Log levels are trimmed and case-insensitive: `debug`/`-4`, `info`,
`warn`/`warning`/`2`, and `error`/`4`. Unset or unrecognized values use `info`.

Flags: `-version`; optional `-health 127.0.0.1:port` (loopback-only `/healthz`).

### Dates

- Calendar `start`/`end`: RFC3339 with offset, or wall clock without offset in
  `ICLOUD_MCP_DEFAULT_TZ`. Prefer no-offset for the user's local time. Recurring
  or explicit-timezone creates write TZID + VTIMEZONE; non-recurring timed
  defaults to UTC `Z`. All-day uses `VALUE=DATE`.
- Contacts birthdays: write `YYYY-MM-DD` only.
- Mail search: `since` inclusive, `before` exclusive (`YYYY-MM-DD`).

## Security (summary)

Untrusted remote text can influence an LLM on the host; labels are not a
boundary. A compromised model can call every **registered** tool. Same model for
every host and vendor.

- **Egress fixed:** Calendar `caldav.icloud.com` / `p[0-9]{1,3}-caldav.icloud.com:443`;
  Contacts matching contacts hosts; IMAP `imap.mail.me.com:993`; SMTP
  `smtp.mail.me.com:587` with mandatory STARTTLS. No configurable destinations,
  no proxy env for DAV, TLS 1.2+ verified.
- **Isolation:** separate credentials, transports/dialers, limiters, semaphores,
  and protocol stacks per domain; no union authenticated HTTP client.
- **Secrets:** redacted (including Basic and SASL PLAIN forms); no `os/exec`,
  telemetry, or disk write after boot-only `file://` reads.
- **Audit:** mutations log `domain`, `resourceType`, process-local HMAC
  `resourceToken` only (never raw paths, UIDs, mailboxes, recipients).
- **Residual risk:** one process holds every enabled domain's credentials;
  feature flags do not remove compiled code. Prefer read-only, least domains,
  dedicated Mail password, or separate processes when stronger isolation is
  required.

Full policy: [SECURITY.md](SECURITY.md), [docs/security.md](docs/security.md).
Architecture: [docs/architecture.md](docs/architecture.md).

## Limits (summary)

| | |
|--|--|
| Tool deadline | 25s (DAV HTTP 30s) |
| Stdio / MCP result | 1 MiB frame; 256 KiB result; reflected protocol errors capped |
| Calendar | 366-day search; 400 returned / 10,000 materialized events; 2,000 expansions / 100k steps per series / 250k steps per search; 60 read / 20 write per minute; concurrency 4 / 2 |
| Contacts | 100 books; 100 summaries; 2000 cards scanned; 60/20 per minute; concurrency 4 |
| Mail | 60 read / 20 mutation / 20 send per minute; semaphores 2 / 1 / 1; no mutation/send retry |
| Writes | No automatic replay of Calendar PUT/DELETE, Contacts writes, IMAP mutations, or SMTP; ambiguous outcomes use `outcome_unknown` |

Lifecycle: eager Calendar discovery at boot; lazy Contacts discovery; fresh
IMAP/SMTP session per call. Rates, XML/iCal/vCard/MIME budgets, and retry rules:
[docs/testing.md](docs/testing.md), [docs/architecture.md](docs/architecture.md).

## Supported scope

| Data | Connector | Support |
|------|-----------|---------|
| Calendar | CalDAV | Always |
| Contacts | CardDAV | Optional |
| Mail read / mutation / send | IMAP + SMTP | Optional, independently gated |
| Modern Reminders, Notes, Photos, Drive, Find My, Keychain, Messages, Home | No suitable official remote connector for this model | Excluded |

Modern Reminders are not treated as generic CalDAV VTODO. Apple's third-party
documentation for this class of access covers Mail, Calendar, and Contacts.

## Dependencies

Go 1.25.12 or newer, one module, exactly **10** direct dependencies. Adding
another requires a written justification here.

| Dependency | Exact version | Justification |
|------------|---------------|---------------|
| `github.com/emersion/go-webdav` | `v0.7.0` | CalDAV primitives; discovery and conditional ops stay hand-rolled |
| `github.com/emersion/go-ical` | `v0.0.0-20250609112844-439c63cef608` | iCalendar parse/encode |
| `github.com/mark3labs/mcp-go` | `v0.57.0` | MCP stdio, schemas, JSON-RPC |
| `github.com/teambition/rrule-go` | `v1.8.2` | Bounded recurrence with timezone preservation |
| `golang.org/x/time` | `v0.15.0` | Per-domain rate limiters |
| `github.com/emersion/go-vcard` | `v0.0.0-20260618161152-d854b7e0e2d3` | vCard 3.0/4.0 read, 3.0 write |
| `github.com/emersion/go-imap/v2` | `v2.0.0-beta.8` | IMAP behind `internal/mail/imapadapter` |
| `github.com/emersion/go-message` | `v0.18.2` | MIME / plain-text bounds |
| `github.com/emersion/go-smtp` | `v0.24.0` | SMTP + STARTTLS |
| `github.com/emersion/go-sasl` | `v0.0.0-20241020182733-b788ff22d5a6` | SASL PLAIN after STARTTLS |

## Build and test

```bash
make build        # local host binary, VERSION defaults to dev
make test         # go test ./... -race -cover
make lint         # go vet + pinned golangci-lint
make release VERSION=v0.3.0      # packaged linux/arm64, pinned Go 1.25.12 image
make release-all VERSION=v0.3.0  # packaged linux/amd64, linux/arm64, darwin/arm64
make install      # host-compatible build to INSTALL_DIR (default ~/.local/bin)
```

Release targets reject an unset or `dev` version. Archives contain the binary,
`LICENSE`, and `THIRD_PARTY_NOTICES.md`; `dist/` also receives a SHA-256
checksum file. `-version` prefers the release ldflags value and falls back to Go
module build information, so `go install ...@version` reports that module
version.

CI: race tests, coverage floors (78% aggregate + package floors), fuzz smoke,
govulncheck, multi-arch build, egress/security AST guards, gitleaks, 20 MiB
binary budget. Live iCloud tests use the `integration` build tag, are opt-in,
and never run in CI. See [docs/testing.md](docs/testing.md).

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
