# Official MCP Registry record

## Current record

Read on 2026-11-04 from
`https://registry.modelcontextprotocol.io/v0/servers?search=io.github.ThomasCrouzet/icloud-mcp`:

```json
{
  "name": "io.github.ThomasCrouzet/icloud-mcp",
  "title": "icloud-mcp",
  "description": "stdio MCP for iCloud Calendar (CalDAV), with optional Contacts and Mail.",
  "repository": {
    "url": "https://github.com/ThomasCrouzet/icloud-mcp",
    "source": "github"
  },
  "version": "0.4.3",
  "websiteUrl": "https://github.com/ThomasCrouzet/icloud-mcp#quick-start",
  "status": "active",
  "publishedAt": "2026-11-04T16:41:16.698962Z",
  "updatedAt": "2026-11-04T16:41:16.698962Z",
  "isLatest": true
}
```

The repository `server.json` declares version `0.4.3`. The tag, `server.json`,
`CHANGELOG.md`, and the registry agree, and `make check-registry` passes.

## How 0.4.3 was published

1. The `v0.4.3` tag published the release archives and cosign signatures.
2. The downloaded archives matched `icloud-mcp-v0.4.3-checksums.txt`.
3. `mcp-publisher` 1.8.7 accepted `server.json` with `validate`.
4. `mcp-publisher publish` ran after GitHub authentication.
5. `make check-registry` passed.

## How 0.4.0 was published

Verified:

- Commit `5d07247` added `server.json` for version `0.4.0` and says "publish
  registry metadata".
- The registry timestamp is `2026-07-25T12:06:22Z`. The commit time is
  `2026-07-25T12:06:44Z`.
- No workflow in `.github/workflows/` publishes to the registry.
- The registry stores no packages or remotes for this entry. It stores metadata
  only.

Unverified:

- The exact operator, command, and authentication token used for 0.4.0.
- Whether the publisher ran before or after the commit.

The likely path is a manual `mcp-publisher publish` from a maintainer
workstation after `mcp-publisher login github`. The repository has no log that
proves it.

## Publish commands

Install the official publisher from a pinned release. This example uses
`v1.8.7` on linux/amd64:

```bash
curl -fsSL -o /tmp/mcp-publisher.tar.gz \
  https://github.com/modelcontextprotocol/registry/releases/download/v1.8.7/mcp-publisher_linux_amd64.tar.gz
curl -fsSL -o /tmp/publisher_checksums.txt \
  https://github.com/modelcontextprotocol/registry/releases/download/v1.8.7/registry_1.8.7_checksums.txt
grep 'mcp-publisher_linux_amd64.tar.gz$' /tmp/publisher_checksums.txt \
  | sha256sum -c -
tar -xzf /tmp/mcp-publisher.tar.gz -C /tmp mcp-publisher
/tmp/mcp-publisher validate
```

Authenticate and publish from the repository root:

```bash
/tmp/mcp-publisher login github
/tmp/mcp-publisher publish
```

`login github` prints a device code. Complete the authorization in a browser.
The token stays in the publisher configuration, outside the repository.

Verify the result:

```bash
curl -fsSL \
  'https://registry.modelcontextprotocol.io/v0/servers?search=io.github.ThomasCrouzet/icloud-mcp'
make check-registry
```

Do not publish unless the `vX.Y.Z` archive exists and its cosign signature
verifies. Do not print or commit the publisher token.

## Fields to review on each release

| Field | Action |
|-------|--------|
| `name` | Never change. It is the registry identity. |
| `version` | Set to the new release without the `v` prefix. It must match the tag, `CHANGELOG.md`, and the archive. |
| `title` | Change only when the product name changes. |
| `description` | Keep at 100 characters or fewer. Change only when the product scope changes. |
| `repository.url` | Keep the canonical GitHub URL. |
| `websiteUrl` | Point to the current quick start or product page. |
| `packages` / `remotes` | Not present. Add only if the distribution model changes. |

Run `mcp-publisher validate` before publish. Run `make check-registry` after
publish. A mismatch is an error.
