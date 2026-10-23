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
before the server starts. See the [configuration
reference](../README.md#configuration) for all 12 product variables.

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
