# Verify after submission

Run these checks seven days after each submission. Save the output outside this
repository. Do not upload reports or SARIF files.

## Registry record

```bash
curl -fsSL \
  'https://registry.modelcontextprotocol.io/v0/servers?search=io.github.ThomasCrouzet/icloud-mcp'
make check-registry
```

Confirm all of these values:

- `version` is the current release without the `v` prefix.
- `_meta.io.modelcontextprotocol.registry/official.isLatest` is `true`.
- `updatedAt` is newer than the previous publication.
- `status` is `active`.
- The tag, `server.json`, `CHANGELOG.md`, and registry agree.

## GitHub traffic

The traffic API needs push access and reports the last 14 days:

```bash
gh api repos/ThomasCrouzet/icloud-mcp/traffic/popular/referrers
gh api repos/ThomasCrouzet/icloud-mcp/traffic/views
gh api repos/ThomasCrouzet/icloud-mcp/traffic/clones
```

Record the referrer host, count, and uniques for each directory. A new referrer
is evidence that a listing sends traffic. No referrer is not proof that a
listing failed.

## Directory pages

Open each submitted listing and record the HTTP status, title, description, and
link target:

- Official MCP Registry:
  `https://registry.modelcontextprotocol.io/v0/servers?search=io.github.ThomasCrouzet/icloud-mcp`
- Glama: `https://glama.ai/mcp/servers/@ThomasCrouzet/icloud-mcp`
- mcp.so: `https://mcp.so/search?q=icloud-mcp`
- mcpservers.org: `https://mcpservers.org/submit`
- Cline: `https://github.com/cline/mcp-marketplace/issues`
- MCP Market: `https://mcpmarket.com/submit`
- Awesome MCP Servers:
  `https://github.com/punkpeye/awesome-mcp-servers`

Check that the description still matches `docs/distribution/listing-copy.md` and
that no listing asks for a password.

## Install path

```bash
go install github.com/ThomasCrouzet/icloud-mcp/cmd/icloud-mcp@latest
icloud-mcp -version
gh release view --repo ThomasCrouzet/icloud-mcp \
  --json tagName,assets --jq '{tagName, assets: [.assets[].name]}'
```

Confirm that the installed version matches the latest tag and that the release
still has the archives, `checksums.txt`, `.sig`, and `.pem` assets. Download one
archive and run the checksum and signature checks in
[the testing guide](../testing.md#release-metadata-and-registry).

## Search checks

Search GitHub, the registry, and each directory for `ThomasCrouzet/icloud-mcp`.
Record a missing result as missing. Do not report a listing as live without a
public URL.

## Record

Write one local record with:

- Date, directory, submission URL, and account.
- Public listing URL and HTTP status.
- Description text and character count.
- Referrer rows from the traffic API.
- Registry `version`, `updatedAt`, and `isLatest`.
- Install and signature result.
- Follow-up action and owner.

Keep the record outside the repository, including files that Git ignores.
