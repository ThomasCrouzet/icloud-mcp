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

For the first deployment, choose one of the three read-only profiles below.
Use `icloud_capabilities` to show the enabled domains, capabilities, and tool list.
This call uses local configuration and makes no account request.

The local binary can return account data to the configured MCP host and model.
Their policies govern onward transmission. Stdio is not a privacy boundary
against that host or model. Review those policies before any account read.

## Configuration reference

Exactly **14** product environment variables:

| Variable | Default | Contract |
|----------|---------|----------|
| `ICLOUD_EMAIL` | none | Required Calendar/Contacts identity. `file://` supported (regular file, <=4 KiB, mode 0600+). |
| `ICLOUD_PASSWORD` | none | Calendar/Contacts app-specific password, or Mail fallback when no separate Mail password is set. `file://` as above. |
| `ICLOUD_MCP_READ_ONLY` | `false` | Global mutation kill switch. |
| `ICLOUD_MCP_PERMISSIONS` | legacy switches | Optional comma-separated capability list. It replaces the implicit write rules. |
| `ICLOUD_MCP_RESOURCE_SCOPES` | all resources | Optional action resource allowlists for Calendar paths, Contacts address books, and Mail mailboxes. |
| `ICLOUD_MCP_LOG_LEVEL` | `info` | Stderr level. See the accepted forms below. |
| `ICLOUD_MCP_DEFAULT_TZ` | `UTC` | IANA zone for offset-less Calendar inputs and recurring-write fallback. |
| `ICLOUD_MCP_ENABLE_CONTACTS` | `false` | Contacts tools. Writes require read-only mode to be off. |
| `ICLOUD_MCP_ENABLE_MAIL` | `false` | Mail reads. Requires a Mail address and password. |
| `ICLOUD_MAIL_ADDRESS` | none | Full IMAP/SMTP address when Mail is on. `file://` as above. |
| `ICLOUD_MAIL_PASSWORD` | `ICLOUD_PASSWORD` | Optional dedicated Mail app password. `file://` as above. |
| `ICLOUD_MCP_ENABLE_MAIL_WRITE` | `false` | Three IMAP mutation tools. |
| `ICLOUD_MCP_ENABLE_MAIL_SEND` | `false` | `send_message` and `reply_message` (independent of Mail write). |
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

`ICLOUD_MCP_RESOURCE_SCOPES` uses `action=value[,value]` entries separated by
`;`. Actions are `calendar.read`, `calendar.write`, `calendar.delete`,
`contacts.read`, `contacts.write`, `contacts.delete`, `mail.read`,
`mail.mutate`, and `mail.archive`. A single value `*` means unrestricted.
Calendar values are paths. Contacts values are opaque identifiers or exact
display names. Mail values are exact mailbox names. A write, delete, or mutate
scope falls back to the domain read scope when unset. Ambiguous or stale
Contacts names fail closed. Unknown, duplicate, empty, or malformed entries fail
before network access.

`ICLOUD_MCP_PERMISSIONS` accepts `calendar.read`, `calendar.write`,
`calendar.delete`, `contacts.read`, `contacts.write`, `contacts.delete`,
`mail.read`, `mail.mutate`, `mail.trash`, `mail.send`, and `mail.archive`. Unknown, duplicate,
or empty tokens cause a boot error. A Contacts or Mail token needs its domain
switch. When the variable is set, it replaces `ICLOUD_MCP_ENABLE_MAIL_WRITE`,
`ICLOUD_MCP_ENABLE_MAIL_SEND`, and the implicit Calendar and Contacts write
rules. Global read-only still removes every mutation and send tool.

The server trims log levels and ignores case. It accepts `debug` or `-4`,
`info`, `warn`, `warning`, or `2`, and `error` or `4`. Unset or unrecognized
values use `info`.

Enabled domains start independently. Calendar and Contacts discovery runs on the
first call for that domain. A temporary Calendar outage does not stop Mail or
Contacts. Mail-only operation does not require Calendar credentials or Calendar
discovery.

Use `-version` to print the version. The optional
`-health 127.0.0.1:port` flag serves `/healthz` and `/status` on loopback. Both
endpoints return JSON with domains and rate limits. Enabled domains start as
`not_checked`; health polling does not create hidden network traffic. The
optional `-audit-format=json|text` flag selects the mutation audit format on
stderr. The default format is `json`.

## Supported profiles

These complete JSON files use Claude Desktop's `mcpServers` stdio shape.
Merge exactly one `icloud` entry into your host configuration.
Replace the absolute command and secret paths. Do not put passwords in JSON.
Each secret file must be regular, at most 4 KiB, and mode 0600 or stricter.
The binary does not expand `~` or shell variables in paths.

### Recommended read-only first run

| Profile | Explicit permissions | Resources |
|---|---|---|
| [Calendar](../examples/profiles/calendar-read-only.json) | `calendar.read` | All calendars, reads only |
| [Calendar and Contacts](../examples/profiles/calendar-contacts-read-only.json) | `calendar.read,contacts.read` | All calendars and address books, reads only |
| [Mail](../examples/profiles/mail-read-only.json) | `mail.read` | Exact mailbox `INBOX` |

All three set `ICLOUD_MCP_READ_ONLY=true`. No mutation or send tool is registered.
They also set an explicit permission list. Unrelated optional gates cannot add
capabilities to that list. Contacts and Mail permission tokens still require
their domain gate; a missing gate causes a boot error, not implicit access.

Mail-only uses `ICLOUD_MAIL_ADDRESS` and a separate `ICLOUD_MAIL_PASSWORD`.
It needs neither `ICLOUD_EMAIL` nor `ICLOUD_PASSWORD`. It constructs no Calendar
or Contacts client. Calendar and Contacts share the configured identity but keep
separate credential copies and transports. Mail keeps its own credentials and transports.

Replace `INBOX` only with an exact authorized mailbox name.
Optional read scopes use the same grammar described above.

### Effectful opt-in, not a first-run recommendation

| Profile | Explicit permissions | Authorized effects |
|---|---|---|
| [Scoped Calendar writes](../examples/profiles/calendar-scoped-write.json) | `calendar.read,calendar.write` | Create and update in selected calendars; no delete |
| [Exact-recipient Mail send](../examples/profiles/mail-exact-send.json) | `mail.send` | Send only to the exact recipient policy; no reads, reply, IMAP mutation, or Sent APPEND |

Both deliberately set `ICLOUD_MCP_READ_ONLY=false`. Select one only after you
authorize its effects. A host approval prompt does not replace server-side scopes.

The Calendar example sets both read and write scopes to `/cal/allowed/`.
This synthetic path is not an account path. First obtain paths with the read-only
Calendar profile and an authorized `list_calendars` call. Copy the exact selected
paths into both scope entries. Use commas to select more than one calendar.
Do not use `*` for a mutation scope. A scope restricts an existing capability;
it never grants the capability. Global read-only still removes these write tools.

The send example uses the non-deliverable placeholder `recipient@example.invalid`.
Replace it with exact recipients that you explicitly authorize. Each To, Cc, and
Bcc recipient must match. Mixed allowed and forbidden recipients fail before SMTP
submission. Never use `*`. The profile does not enable `reply_message`, Mail reads,
or Sent archival. Render real mail with the canonical template workflow described
in [Mail compatibility](mail-compatibility.md) and authorize each submission.

With explicit permissions, `mail.send` replaces the legacy send switch as the
capability grant. This example also sets the send switch to show deliberate intent.
Removing that switch alone does not revoke an explicit `mail.send` grant.
Remove the permission or enable global read-only to revoke it, then restart.

## Offline profile validation

Run these commands from a source checkout with the documented Go toolchain and Python 3.
Choose a new absolute evidence directory outside the checkout:

```bash
GOMAXPROCS=4 GOFLAGS=-p=4 python3 cmd/icloud-mcp/setup_profiles_test.py \
  --output /absolute/private/evidence/profiles-run-1
GOMAXPROCS=4 go test -p 4 -race -json ./internal/mcptools ./cmd/icloud-mcp \
  -run '^TestSetupProfiles' -count=1 -timeout=120s \
  > /absolute/private/evidence/profiles-tests.jsonl
```

The Python check builds the actual binary. It reads the committed JSON, substitutes
only synthetic secret paths and the test binary, then initializes MCP over stdio.
It checks `tools/list`, `icloud_capabilities`, disabled-tool calls, and clean exit.
It uses an isolated HOME and synthetic mode-0600 secret files. It never calls a
live account tool or `-doctor`. Dependencies must already be available for a
fully disconnected build. Keep the result JSON, transcripts, and fixture identities private.

The Go scenarios load the same files and clear inherited `ICLOUD_*` values.
They verify exact manifests, unrelated gates, missing domain gates, legacy defaults,
and denied effects. Genuine handlers and services use counted synthetic DAV and
verified STARTTLS Mail peers. Allowed writes and send must act exactly once;
forbidden resources and recipients must fail before backend access without disclosure.

This validates offline host JSON, CLI startup, and MCP behavior. It does not prove
a desktop GUI installation, Apple compatibility, or distribution installation.
After configuring your real host, compare `tools/list` with `icloud_capabilities.tools`
before any account call. Verify the enabled domains and `capabilityGroups`, not
only the tool count. `icloud_capabilities` does not display resource paths or recipients;
review those exact values in your local configuration as well.

## Authenticated diagnostics

`-doctor` is a separate authenticated read-only **network operation**, not an
offline profile validator. It runs bounded probes and exits. Each enabled domain
has a 10-second timeout. Exit status is 0 when all enabled domains are ready,
1 on a failed probe, and 2 for an invalid output format.

The diagnostic discovers Calendar collections and Contacts address books.
For Mail, it lists mailboxes and can select one read-only to inspect capabilities.
It does not apply MCP resource filtering to these discovery probes.
It never submits SMTP, verifies recipient delivery, or makes account changes.
It prints no identities, passwords, secret paths, or resource names.

Only run it after separate authorization for those diagnostic reads.
For the unrestricted Calendar read-only profile, use the same identity and permissions:

```bash
env -i \
  ICLOUD_EMAIL=file:///Users/you/.config/icloud-mcp/email \
  ICLOUD_PASSWORD=file:///Users/you/.config/icloud-mcp/password \
  ICLOUD_MCP_READ_ONLY=true ICLOUD_MCP_PERMISSIONS=calendar.read \
  /Users/you/go/bin/icloud-mcp -doctor -doctor-format=json
```

Replace only the absolute paths. Contacts diagnostics also need the Contacts read
permission and domain gate. For scoped Calendar or Mail profiles, doctor can discover
outside the MCP resource allowlist. Do not treat its output as scope conformance.
Do not run doctor under the send-only profile: it would attempt IMAP diagnostics
despite the absence of `mail.read`. Validate send capabilities locally instead.
Use a separately authorized read configuration if broader network diagnostics are needed.
Do not remove scopes or add permissions merely to make a diagnostic pass.

## Future v1.0.0 default migration

The design target is a **breaking v1.0.0** default-read-only migration.
This is not a v0.5.x patch change, release announcement, or automatic upgrade action.
Current defaults remain unchanged: with no explicit permissions and an unset
`ICLOUD_MCP_READ_ONLY`, Calendar writes and deletes remain available.
Legacy Contacts also gains writes when its domain gate is enabled.
An explicit permission list replaces these implicit grants.

Before implementing that target, publish advance release and migration notes.
Include notices in a preceding release and require operators to review effective
permissions before upgrading. Document removal of mutation tools from default discovery.
Preserve credential separation, domain gates, exact recipient checks, and resource scopes.

Operators who retain writes must configure `ICLOUD_MCP_READ_ONLY=false`, explicit
write/send permissions, required domain gates, and narrow scopes or exact recipients.
They must not rely on omission or on a domain gate alone.
Validate the resulting manifest and scopes before an authorized operation.
Read-only users should keep `ICLOUD_MCP_READ_ONLY=true` and explicit read permissions now.
Do not wait for v1.0.0 to use the recommended profiles.

## Dates

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
        "ICLOUD_MCP_PERMISSIONS": "calendar.read",
        "ICLOUD_MCP_ENABLE_CONTACTS": "false",
        "ICLOUD_MCP_ENABLE_MAIL": "false",
        "ICLOUD_MCP_ENABLE_MAIL_WRITE": "false",
        "ICLOUD_MCP_ENABLE_MAIL_SEND": "false",
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
for all 14 product variables.

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
