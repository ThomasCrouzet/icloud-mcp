# Security Policy

## Threat model

`icloud-mcp` runs as a stdio child process driven by an LLM-powered MCP host, and is
built on the assumption that **the agent driving it can be compromised or manipulated**
(e.g. prompt injection). Its blast radius is deliberately minimal:

- **Hard network allowlist**: the only reachable destination is
  `https://caldav.icloud.com` and its `pXX-caldav.icloud.com` shards. Every other host,
  scheme, or explicit port is rejected before DNS resolution, on every redirect hop, and
  TLS is always verified.
- **Secret redaction**: the iCloud app-specific password (and its derived Basic-auth
  and URL-encoded forms) and the account email are redacted from every output: logs,
  errors, and MCP responses, including the JSON-RPC error path on panic.
- **Read-only by default**: with `ICLOUD_MCP_READ_ONLY=1` the write tools are not even
  registered. Calendar is the only Apple service touched; there is zero `os/exec` and no
  disk writes (the sole disk read is the `file://` credentials at startup).
- **App-specific password**: credentials are always an app-specific password (never the
  main Apple ID password), revocable at any time on appleid.apple.com with no impact on
  the account.

At worst, an attacker controlling the driving agent can read (and, if read-only mode is
lifted, modify or delete) the configured calendar, nothing else. Credentials cannot be
exfiltrated, and no pivot to another Apple service or network destination is possible.

## Reporting a vulnerability

Please report security issues privately through GitHub's
[private vulnerability reporting](https://github.com/ThomasCrouzet/icloud-mcp/security/advisories/new)
rather than opening a public issue. You should receive an acknowledgement within a few
days.
