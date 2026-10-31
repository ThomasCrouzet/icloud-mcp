# MCP host integration

`icloud-mcp` is a stdio MCP server. A compatible host starts the process and
connects its standard input and output to JSON-RPC. The binary reads only its
process environment. It does not read `.env` files or host configuration files.

## Common setup

1. Follow the [quick start](../README.md#quick-start).
2. Give the host the absolute path to `icloud-mcp`.
3. Pass each required environment variable as a string.
4. Use stdio as the transport.
5. Keep stdout exclusively for JSON-RPC.
6. Send stderr to the host log collector.
7. After an environment change, start the process again.

For the first deployment, enable only Calendar and global read-only mode. This
configuration registers seven tools. Use `icloud_capabilities` to show the
enabled domains, capabilities, and tool list.

## Configuration reference

Exactly **12** product environment variables:

| Variable | Default | Contract |
|----------|---------|----------|
| `ICLOUD_EMAIL` | none | Required Calendar/Contacts identity. `file://` supported (regular file, <=4 KiB, mode 0600+). |
| `ICLOUD_PASSWORD` | none | Required app-specific password. Mail uses it as a fallback. `file://` as above. |
| `ICLOUD_MCP_READ_ONLY` | `false` | Global mutation kill switch. |
| `ICLOUD_MCP_LOG_LEVEL` | `info` | Stderr level. See the accepted forms below. |
| `ICLOUD_MCP_DEFAULT_TZ` | `UTC` | IANA zone for offset-less Calendar inputs and recurring-write fallback. |
| `ICLOUD_MCP_ENABLE_CONTACTS` | `false` | Contacts tools. Writes require read-only mode to be off. |
| `ICLOUD_MCP_ENABLE_MAIL` | `false` | Mail reads. Requires a Mail address and password. |
| `ICLOUD_MAIL_ADDRESS` | none | Full IMAP/SMTP address when Mail is on. `file://` as above. |
| `ICLOUD_MAIL_PASSWORD` | `ICLOUD_PASSWORD` | Optional dedicated Mail app password. `file://` as above. |
| `ICLOUD_MCP_ENABLE_MAIL_WRITE` | `false` | Three IMAP mutation tools. |
| `ICLOUD_MCP_ENABLE_MAIL_SEND` | `false` | `send_message` (independent of Mail write). |
| `ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS` | none | Required if send is on. Use exact addresses or literal `*`. Literal `*` causes a boot warning. Use exact addresses when possible. |

Booleans accept only unset, `0`, `false`, `1`, or `true`. Invalid values cause a
boot failure. The server validates the configuration **before** network access.
Hosts must pass these values as strings. Quote a boolean in YAML, for example
`ICLOUD_MCP_READ_ONLY: "false"`. An unquoted YAML boolean fails host validation
before the server starts.

Mail write or send without Mail causes a boot error. Mail without an address or
password also causes a boot error. Send without a recipient policy causes a
boot error, including in read-only mode. Global read-only can coexist with write
and send flags, but it prevents their registration.

The server trims log levels and ignores case. It accepts `debug` or `-4`,
`info`, `warn`, `warning`, or `2`, and `error` or `4`. Unset or unrecognized
values use `info`.

Use `-version` to print the version. The optional
`-health 127.0.0.1:port` flag serves `/healthz` and `/status` on loopback. Both
endpoints return JSON with domains and rate limits. The optional
`-audit-format=json|text` flag selects the mutation audit format on stderr.
The default format is `json`.

### Optional domains

```bash
# Contacts reads (writes also need ICLOUD_MCP_READ_ONLY=false)
export ICLOUD_MCP_ENABLE_CONTACTS=true

# Mail reads: IMAP identity may differ from ICLOUD_EMAIL (for example name@icloud.com)
export ICLOUD_MCP_ENABLE_MAIL=true
export ICLOUD_MAIL_ADDRESS='file:///private/path/icloud-mail-address'
# Optional separate password; otherwise Mail uses ICLOUD_PASSWORD.
# export ICLOUD_MAIL_PASSWORD='file:///private/path/icloud-mail-password'

# Mail mutation (flags, move, trash); independent of send
export ICLOUD_MCP_ENABLE_MAIL_WRITE=true
export ICLOUD_MCP_READ_ONLY=false

# Mail send: requires an exact recipient allowlist even under global read-only
export ICLOUD_MCP_ENABLE_MAIL_SEND=true
export ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS='alice@example.com,bob@example.net'
```

### Dates

- Calendar **input** `start` and `end` accept RFC3339 with an offset. They also
  accept wall-clock values without an offset in `ICLOUD_MCP_DEFAULT_TZ`. Use
  values without an offset for the user's local time. Recurring creates and
  creates with an explicit timezone write TZID and VTIMEZONE. Non-recurring
  timed creates default to UTC `Z` on the wire. All-day creates use
  `VALUE=DATE`.
- Calendar **output** uses RFC3339 for timed events. It always includes an
  explicit numeric offset in `ICLOUD_MCP_DEFAULT_TZ`, never bare `Z`. All-day
  dates use `YYYY-MM-DD`. See `calendar_capabilities.outputFormat`.
- Contacts birthdays: write `YYYY-MM-DD` only.
- Mail search: `since` inclusive, `before` exclusive (`YYYY-MM-DD`).

## Host configuration

Host field names differ. Use the host's custom stdio server option. The
following JSON object shows the required process configuration:

```json
{
  "mcpServers": {
    "icloud": {
      "command": "/Users/you/.local/bin/icloud-mcp",
      "env": {
        "ICLOUD_EMAIL": "file:///private/path/icloud-email",
        "ICLOUD_PASSWORD": "file:///private/path/icloud-password",
        "ICLOUD_MCP_READ_ONLY": "true",
        "ICLOUD_MCP_DEFAULT_TZ": "Europe/Paris"
      }
    }
  }
}
```

Use absolute paths for the binary and secret files. Each secret file must be a
regular file of 4 KiB or less. Its mode must be 0600 or stricter.

Pass boolean values as strings. For YAML hosts, quote every boolean:

```yaml
ICLOUD_MCP_READ_ONLY: "true"
ICLOUD_MCP_ENABLE_CONTACTS: "false"
```

An unquoted YAML boolean becomes a host-side boolean. Some hosts reject it
before the server starts. See the [configuration reference](#configuration-reference)
for all 12 product variables.

## Logs and health

Stdio transports reserve stdout for MCP traffic. The server writes operational
logs and mutation audit records to stderr. Mutation audits use JSON by default.

A supervisor can enable the optional loopback health endpoint:

```bash
icloud-mcp -health 127.0.0.1:8797
curl --fail http://127.0.0.1:8797/healthz
```

The endpoint returns JSON status, enabled domains, and rate limits. The listener
accepts loopback addresses only. Give each process a different port.

## Multiple accounts

One process uses one iCloud identity. Start a separate process for each account.
Give each process a separate environment and server name.

Use separate secret files for each account. A host uses the server name to
identify each tool set.

## Caller behavior

- For Calendar writes, use wall-clock values without an offset. Set
  `ICLOUD_MCP_DEFAULT_TZ` to the owner's IANA time zone.
- Calendar responses use RFC3339 with an explicit offset in that zone. See
  `calendar_capabilities.outputFormat`.
- For `create_event` and `create_contact`, use `client_uid` or its
  `idempotency_key` alias as the resource UID.
- For `update_event` and `update_contact`, use `idempotency_key` with a strong
  `etag`.
- Known update results stay for 15 minutes. Uncertain results stay until process
  exit. See the [update contract](architecture.md#update-idempotency).
- Match the structured `code` field. See [error-codes.md](error-codes.md).
- Obey `retry_after_seconds` for `rate_limited` and `unavailable` errors.
- After an `outcome_unknown` update, read the resource before you select a new key.
- If the update cache is full, keep the key and wait for capacity.
