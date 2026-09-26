# Agent host integration

`icloud-mcp` is a **stdio MCP server**. It works with any host that can start a
process, set its environment, and connect stdin and stdout. The binary does not
read host-specific configuration files or `.env` files.

## Common setup

1. Follow the [quick start](../README.md#quick-start) to install the binary and
   configure credentials.
2. Give the host the absolute path to `icloud-mcp` and the same environment.
3. Use stdio for JSON-RPC.
4. Read structured logs and mutation audits from stderr.

Mutation audits use JSON by default. The [configuration
reference](../README.md#configuration) describes all 12 product variables.

For the first deployment, enable only Calendar and global read-only mode. This
configuration provides seven tools.

## Hermes

Register the stdio MCP server under `mcp_servers` in
`~/.hermes/config.yaml`. See the current Hermes MCP documentation. Use this
example:

```yaml
mcp_servers:
  icloud:
    command: /home/you/.local/bin/icloud-mcp
    env:
      ICLOUD_EMAIL: you@icloud.com
      ICLOUD_PASSWORD: app-specific-password
      ICLOUD_MCP_READ_ONLY: "true"
      ICLOUD_MCP_DEFAULT_TZ: Europe/Paris
```

Store secrets in the Hermes profile `.env` when the host uses that file. Call
`icloud_capabilities` first to see the active tool list.

Hermes also provides an optional catalog entry under `optional-mcps/` in the
Hermes Agent repository. Catalog installation is separate from this manual
configuration.

## JSON host configuration

Use the host's "custom MCP server" or "stdio MCP" entry. Use this JSON example:

```json
{
  "mcpServers": {
    "icloud": {
      "command": "/Users/you/.local/bin/icloud-mcp",
      "env": {
        "ICLOUD_EMAIL": "you@icloud.com",
        "ICLOUD_PASSWORD": "app-specific-password",
        "ICLOUD_MCP_READ_ONLY": "true",
        "ICLOUD_MCP_DEFAULT_TZ": "Europe/Paris"
      }
    }
  }
}
```

Reload the host after environment changes. Never commit the password to a
shared configuration repository. Use an operating system secret store or a
boot-only `file://` secret. The secret must be a regular file of 4 KiB or less.
Its mode must be 0600 or stricter.

## OpenClaw and other orchestrators

Run `icloud-mcp` as a long-lived or per-session child process:

- **command**: absolute path to `icloud-mcp`
- **transport**: stdio JSON-RPC
- **env**: the [12 product variables](../README.md#configuration)
- **logs**: parse stderr NDJSON (`msg=audit` for mutations)

Supervisors can use the optional loopback health endpoint:

```bash
icloud-mcp -health 127.0.0.1:8797
# GET http://127.0.0.1:8797/healthz  -> JSON status, domains, rateLimits
```

## Multi-account

One process uses one iCloud identity. For multiple accounts, start **N
processes** with different environments. Use different health ports when you
enable health endpoints. Hosts identify each tool set by server name. The
server does not support multiple accounts in one process.

## Agent behavior tips

- For Calendar writes, use wall-clock times without an offset. Set
  `ICLOUD_MCP_DEFAULT_TZ` to the owner's IANA time zone.
- Responses use RFC3339 with an explicit offset in that zone. See
  `calendar_capabilities.outputFormat`.
- For `create_event` and `create_contact`, use `client_uid` or its
  `idempotency_key` alias as the resource UID.
- For `update_event` and `update_contact`, use `idempotency_key` with a strong
  `etag`. The key identifies a saved result within that process and tool.
- Known update results stay for 15 minutes after the request completes.
  Uncertain results stay until process exit. See the
  [update contract](architecture.md#update-idempotency).
- Match structured `code` fields. See [error-codes.md](error-codes.md).
- Obey `retry_after_seconds` for `rate_limited` and `unavailable` errors.
- After an ambiguous update, read with `get_event` or `get_contact` before
  selecting a new key. A new key or restart does not prove that the write failed.
- If the cache is full, keep the key. Wait for capacity.
