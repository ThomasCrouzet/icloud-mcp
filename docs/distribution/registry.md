# Official MCP Registry record

## Current record: 2026-11-08

Read on 2026-11-08 from
`https://registry.modelcontextprotocol.io/v0/servers?search=io.github.ThomasCrouzet/icloud-mcp`.
The latest record is version `0.5.2`, published at
`2026-11-08T11:33:00Z`, and it is the first record with packages:

```json
{
  "name": "io.github.ThomasCrouzet/icloud-mcp",
  "version": "0.5.2",
  "packages": [
    {
      "registryType": "mcpb",
      "identifier": "https://github.com/ThomasCrouzet/icloud-mcp/releases/download/v0.5.2/icloud-mcp-v0.5.2-linux-amd64.mcpb",
      "version": "0.5.2",
      "fileSha256": "4c4ccaa729fff875a29ec5680c2bb9b0ba7c6ed4ada1ff2e8210580385c887e4",
      "transport": {"type": "stdio"}
    },
    {
      "registryType": "mcpb",
      "identifier": "https://github.com/ThomasCrouzet/icloud-mcp/releases/download/v0.5.2/icloud-mcp-v0.5.2-linux-arm64.mcpb",
      "version": "0.5.2",
      "fileSha256": "edc49688736e28e9ced5dbffc94d283df7f40f0dc15f476d2f568c748d0466e1",
      "transport": {"type": "stdio"}
    },
    {
      "registryType": "mcpb",
      "identifier": "https://github.com/ThomasCrouzet/icloud-mcp/releases/download/v0.5.2/icloud-mcp-v0.5.2-darwin-arm64.mcpb",
      "version": "0.5.2",
      "fileSha256": "b5b2ad8dec6948bf18f2b742b21d56757de2de7d5dc586d7ed32ba6a76424879",
      "transport": {"type": "stdio"}
    }
  ],
  "isLatest": true
}
```

The downloaded bundles matched these hashes, all three Cosign signatures
verified against the `v0.5.2` tag workflow identity, and `make check-registry`
passed. The first publication attempt failed closed because the Registry could
not reach the GitHub asset URL before its timeout; a retry succeeded. No
metadata changed between the attempts.

## Historical record: 2026-11-04

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

At this snapshot, the repository `server.json` declared version `0.4.3`.
The tag, `server.json`, `CHANGELOG.md`, and the registry agreed, and
`make check-registry` passed. That record had no `packages` or `remotes`.

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

## MCPB package publication

Checked on 2026-11-08.

The official Registry schema dated 2025-12-11 accepts a `packages` array. For
`registryType` `mcpb`, it requires `identifier`, `transport`, and
`fileSha256`. The identifier is an HTTPS URL that ends in `.mcpb`. The transport
type is `stdio`. The package version equals the server version. This project has
no remote endpoint and declares no `remotes`.

The official publisher used here is `mcp-publisher` 1.8.8, built from the
`github.com/modelcontextprotocol/registry` module through the Go module proxy
and its checksum database. The verified commands are `init`, `login`, `logout`,
`publish`, `status`, and
`validate`. `login github-oidc` uses GitHub Actions OIDC. The workflow keeps
`permissions: contents: read` and `id-token: write`, and uses no long-lived
token.

### Order of operations

Do not publish before every earlier gate passes. The order is fixed:

1. Build the release archives locally and compute the deterministic bundle
   hashes with the pinned Go version.
2. Put the bundle URLs and SHA-256 values in `server.json`.
3. Validate release metadata and package hashes.
4. Commit and tag. CI builds the archives, rebuilds and verifies each bundle,
   runs the native bundle smoke matrix, signs everything, and creates the
   signed GitHub Release.
5. CI dispatches the Registry workflow after the release exists. A release
   created with `GITHUB_TOKEN` does not emit a second workflow run, so the
   dispatch is explicit.
6. The Registry workflow verifies hashes and signatures, publishes with GitHub
   OIDC, and verifies the published packages and version.

### 1. Build and verify bundles

Use the pinned Go version from `go.mod` and the release flags. Build the
archives first, then build bundles from the archives:

```bash
VERSION=v0.5.1
make release-all VERSION="$VERSION"
for target in linux/amd64 linux/arm64 darwin/arm64; do
  python3 scripts/mcpb.py build \
    --source "dist/icloud-mcp-$VERSION-${target%/*}-${target#*/}.tar.gz" \
    --target "$target" --version "$VERSION"
done
python3 scripts/mcpb.py verify \
  "dist/icloud-mcp-$VERSION-linux-amd64.mcpb"
```

Run `verify` only on a host of the same platform. Use
`scripts/check_release.py packages` for the other platforms.

### 2. Add package records

Add one `packages` entry for each claimed platform. Use the immutable release
asset URL and the bundle hash:

```bash
sha256sum dist/*.mcpb
```

```json
"packages": [
  {
    "registryType": "mcpb",
    "identifier": "https://github.com/ThomasCrouzet/icloud-mcp/releases/download/v0.5.1/icloud-mcp-v0.5.1-linux-amd64.mcpb",
    "version": "0.5.1",
    "fileSha256": "<64 lowercase hex characters>",
    "transport": {
      "type": "stdio"
    }
  }
]
```

Do not include a package for a platform that has no bundle. Do not include a
`remotes` array.

### 3. Validate local files

```bash
make check-release-metadata
python3 scripts/check_release.py packages --dir dist
python3 -m unittest scripts/test_mcpb.py scripts/test_check_release.py -v
```

`check-release-metadata` rejects a package version mismatch, a non-HTTPS URL, a
URL without `.mcpb`, a bad hash, a non-stdio transport, a duplicate identifier,
and a `remotes` array. `packages` rejects a missing bundle, a hash mismatch, a
wrong bundle version, and a wrong bundle platform.

### 4. Create the release

CI creates the release after the tag passes CI, gitleaks, release metadata, and
the native smoke jobs. The release job then:

1. Rebuilds every archive and bundle from the tag.
2. Verifies each bundle and checks it against `server.json`.
3. Appends the MCPB hashes to the checksums file.
4. Signs the archives, bundles, and checksums with cosign keyless.
5. Creates the GitHub Release with all signed assets.
6. Dispatches `.github/workflows/publish-registry.yml` for that tag.

The release job fails before signing when a claimed platform is missing, a
bundle hash differs from `server.json`, or a bundle verification fails.

### 5. Publish with least privilege

The workflow `.github/workflows/publish-registry.yml` runs on an explicit
workflow dispatch (and on a published release when a maintainer uses a token
that emits events). It downloads the assets, verifies hashes and signatures,
downloads the pinned publisher, validates `server.json`, logs in with GitHub
OIDC, publishes, and verifies the Registry.

The Registry job needs only:

```yaml
permissions:
  contents: read
  id-token: write
```

The release job needs `actions: write` only to dispatch that workflow. The
Registry workflow does not run on pull requests or `main` pushes. It fails
closed when an asset is missing, a hash differs from `server.json`, a signature
fails, or the published packages do not match.

For a manual publisher run from a workstation, use the device flow:

```bash
GOBIN=/tmp/mcpbin go install \
  github.com/modelcontextprotocol/registry/cmd/publisher@v1.8.8
/tmp/mcpbin/publisher validate server.json
/tmp/mcpbin/publisher login github
/tmp/mcpbin/publisher publish server.json
```

The Go module proxy and checksum database pin the publisher source. The
workflow uses the same command and never downloads an unverified binary from a
release asset.

`login github` prints a device code. Complete the authorization in a browser.
The token stays in the publisher configuration, outside the repository.

### 6. Verify the result

```bash
curl -fsSL \
  'https://registry.modelcontextprotocol.io/v0/servers?search=io.github.ThomasCrouzet/icloud-mcp'
make check-registry
```

`make check-registry` compares the latest published version and, when
`server.json` has packages, the exact identifier and `fileSha256` set. A missing
package, an extra package, or a hash mismatch fails. Report success only after
this command passes.

### Rollback

Registry records are append-only in normal use. Correct a bad publication with
the publisher status command, then verify again:

```bash
/tmp/mcpbin/publisher status --status deprecated \
  --message "Superseded by v0.5.2" \
  io.github.ThomasCrouzet/icloud-mcp 0.5.1
```

Use `--status deleted` only for a security or integrity failure. Restore with
`--status active` after the replacement version is verified. Do not delete the
GitHub release if the package URL is already published; delete it only before
publication. Revert a bad `server.json` change with a new commit and a new
version. Do not reuse a published version.

## Fields to review on each release

| Field | Action |
|-------|--------|
| `name` | Never change. It is the registry identity. |
| `version` | Set to the new release without the `v` prefix. It must match the tag, `CHANGELOG.md`, the archive, and each bundle. |
| `title` | Change only when the product name changes. |
| `description` | Keep at 100 characters or fewer. Change only when the product scope changes. |
| `repository.url` | Keep the canonical GitHub URL. |
| `websiteUrl` | Point to the current quick start or product page. |
| `packages` | One `mcpb` entry for each claimed platform. Keep the URL and hash exact. |
| `remotes` | Keep absent. This server is stdio only. |

Run `mcp-publisher validate` before publish. Run `make check-registry` after
publish. A mismatch is an error.
