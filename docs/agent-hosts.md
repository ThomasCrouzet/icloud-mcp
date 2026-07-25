# Agent host integration

`icloud-mcp` is a **stdio MCP server**. Any host that can spawn a process with
an environment and wire stdin/stdout works. The binary does not parse
host-specific config files or `.env`.

## Common setup

1. Install the binary (`go install`, `make install`, or a release archive).
2. Create an [app-specific password](https://appleid.apple.com).
3. Export at least:

```bash
export ICLOUD_EMAIL='you@icloud.com'
export ICLOUD_PASSWORD='app-specific-password'
export ICLOUD_MCP_READ_ONLY=true
export ICLOUD_MCP_DEFAULT_TZ='Europe/Paris'
```

4. Point the host at the absolute path of `icloud-mcp` and pass the same env.
5. Stdio = JSON-RPC; stderr = structured logs + mutation audit (JSON by default).

Recommended first deploy: Calendar only, global read-only (7 tools).

## Hermes

Register a stdio MCP server whose command is the binary and whose environment
contains the variables above. Example shape (host YAML varies by Hermes
version):

```yaml
mcp:
  servers:
    icloud:
      command: /home/you/.local/bin/icloud-mcp
      env:
        ICLOUD_EMAIL: you@icloud.com
        ICLOUD_PASSWORD: app-specific-password
        ICLOUD_MCP_READ_ONLY: "true"
        ICLOUD_MCP_DEFAULT_TZ: Europe/Paris
```

Call `icloud_capabilities` first to see the effective tool list.

## Claude Desktop / Claude Code / OpenAI-compatible MCP bridges

Use the host's "custom MCP server" / "stdio MCP" entry. JSON-style example:

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

Reload the host after env changes. Never commit the password into a shared
config repo; prefer OS secret stores or boot-only `file://` secrets (regular
file, at most 4 KiB, mode 0600 or stricter).

## OpenClaw and other orchestrators

Treat `icloud-mcp` as a long-lived or per-session child process:

- **command**: absolute path to `icloud-mcp`
- **transport**: stdio JSON-RPC
- **env**: the 12 product variables (see README)
- **logs**: parse stderr NDJSON (`msg=audit` for mutations)

Optional loopback health for supervisors:

```bash
icloud-mcp -health 127.0.0.1:8797
# GET http://127.0.0.1:8797/healthz  -> JSON status, domains, rateLimits
```

## Multi-account

One process = one iCloud identity. For multiple accounts, spawn **N processes**
with distinct env (and distinct health ports if used). Hosts multiplex tools by
server name. In-process multi-account is intentionally out of scope.

## Agent behavior tips

- Prefer wall-clock times without offset for Calendar writes; set
  `ICLOUD_MCP_DEFAULT_TZ` to the owner's IANA zone. Responses use RFC3339 with
  an explicit offset in that zone (`calendar_capabilities.outputFormat`).
- Pass `client_uid` / `idempotency_key` on creates and updates so timeouts are
  safer to recover from. Create keys are server-side UIDs; update keys are
  process-local for 15 minutes only and do not survive restart.
- Match structured `code` fields; see [error-codes.md](error-codes.md).
- Honor `retry_after_seconds` on `rate_limited` / `unavailable`.
- On `outcome_unknown`, reconcile before replaying a mutation.
