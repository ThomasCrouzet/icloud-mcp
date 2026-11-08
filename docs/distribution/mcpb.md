# MCPB bundles

An MCPB bundle is a zip archive with a `manifest.json` at its root. A host that
supports MCPB can install the bundle, ask for the declared user settings, and
start the server without a manual command line. This project ships one native
Go executable in each bundle. It does not ship a JavaScript or Python runtime.

## Contents

Every bundle has exactly these entries:

```text
manifest.json
server/icloud-mcp
LICENSE
THIRD_PARTY_NOTICES.md
```

A Windows experiment bundle uses `server/icloud-mcp.exe`. The manifest uses
MCPB manifest version `0.4`, `server.type` `binary`, and this command:

```text
${__dirname}/server/icloud-mcp
```

The build writes fixed zip timestamps, fixed modes, and sorted entries. Two
builds from the same executable and text files have the same SHA-256.

## Platform matrix

| Platform | Build | Bundle status |
|----------|-------|---------------|
| Linux amd64 | Native or cross build | Packaged. Verify runs the bundled binary on a Linux amd64 host. |
| Linux arm64 | Cross build | Packaged. Structure and hash checks pass. No native run was performed here. |
| macOS arm64 | Cross build | Packaged. Structure and hash checks pass. No native run was performed here. |
| Windows amd64 | Compile only | Not packaged. `build` refuses `windows/amd64` unless `--allow-untested-windows` is passed. |

Only linux/amd64, linux/arm64, and darwin/arm64 are release targets. The Windows
override exists for an explicit experiment and is not a support claim.

## Profiles

The five documented profiles are environment choices in one bundle. They are
not five bundles. The install default is Calendar read-only.

| Profile | Settings |
|---------|----------|
| Calendar read-only | `read_only=true`, `permissions=calendar.read`, `enable_contacts=false`, `enable_mail=false` |
| Calendar and Contacts read-only | `read_only=true`, `permissions=calendar.read,contacts.read`, `enable_contacts=true` |
| Calendar scoped write | `read_only=false`, `permissions=calendar.read,calendar.write`, `resource_scopes=calendar.read=/cal/allowed/;calendar.write=/cal/allowed/` |
| Mail read-only | `read_only=true`, `permissions=mail.read`, `enable_mail=true`, `mail_address`, `mail_password` |
| Mail exact send | `read_only=false`, `permissions=mail.send`, `enable_mail=true`, `enable_mail_send=true`, `smtp_allowed_recipients` |

The same values are in `examples/profiles/`. Use exact recipients. A literal `*`
is unsafe.

## User settings and credentials

The manifest declares these `user_config` entries. Each one maps to a process
environment variable:

| Setting | Environment variable | Secret |
|---------|----------------------|--------|
| `icloud_email` | `ICLOUD_EMAIL` | No |
| `icloud_password` | `ICLOUD_PASSWORD` | Yes |
| `read_only` | `ICLOUD_MCP_READ_ONLY` | No |
| `permissions` | `ICLOUD_MCP_PERMISSIONS` | No |
| `resource_scopes` | `ICLOUD_MCP_RESOURCE_SCOPES` | No |
| `default_timezone` | `ICLOUD_MCP_DEFAULT_TZ` | No |
| `enable_contacts` | `ICLOUD_MCP_ENABLE_CONTACTS` | No |
| `enable_mail` | `ICLOUD_MCP_ENABLE_MAIL` | No |
| `mail_address` | `ICLOUD_MAIL_ADDRESS` | No |
| `mail_password` | `ICLOUD_MAIL_PASSWORD` | Yes |
| `enable_mail_write` | `ICLOUD_MCP_ENABLE_MAIL_WRITE` | No |
| `enable_mail_send` | `ICLOUD_MCP_ENABLE_MAIL_SEND` | No |
| `smtp_allowed_recipients` | `ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS` | No |

The two password settings have `sensitive: true`. The host must store and mask
them. The manifest contains only `${user_config.NAME}` references. It contains
no credential value.

For hosts that cannot guarantee secret storage, enter a documented `file://`
value in a string setting. The server opens a regular file of 4 KiB or less with
mode 0600 or stricter. For example:

```text
file:///run/secrets/icloud-email
file:///run/secrets/icloud-password
```

The file must be readable by the server process. The bundle does not copy the
file and writes no secret to disk.

`ICLOUD_EMAIL` and `ICLOUD_PASSWORD` are optional in the manifest so a Mail-only
profile can install without Calendar credentials. Calendar and Contacts need
them. Global read-only defaults to `true`.

## Build, verify, and smoke test

Run these commands from the repository root. Use a temporary staging directory
outside the repository for the source files.

```bash
stage="$(mktemp -d)"
go build -trimpath -ldflags='-s -w -X main.version=v0.5.1' \
  -o "$stage/icloud-mcp" ./cmd/icloud-mcp
cp LICENSE THIRD_PARTY_NOTICES.md "$stage/"
python3 scripts/mcpb.py build \
  --source "$stage" --target linux/amd64 --version v0.5.1
python3 scripts/mcpb.py verify \
  dist/icloud-mcp-v0.5.1-linux-amd64.mcpb
smoke="$(mktemp -d)"
python3 -m zipfile -e dist/icloud-mcp-v0.5.1-linux-amd64.mcpb "$smoke"
npx -y @anthropic-ai/mcpb@2.1.2 validate "$smoke/manifest.json"
npx -y @anthropic-ai/mcpb@2.1.2 info \
  dist/icloud-mcp-v0.5.1-linux-amd64.mcpb
```

The build accepts a release archive instead of a directory:

```bash
python3 scripts/mcpb.py build \
  --source dist/icloud-mcp-v0.5.1-linux-amd64.tar.gz \
  --target linux/amd64 --version v0.5.1
```

`verify` prints one JSON object. It checks the entry set, manifest fields,
version equality with `server.json`, executable mode, one platform, path and
credential rules, and the extracted binary. It runs `-version` and requires the
reported version to equal the manifest version. A failure prints a JSON error
and exits with status 1. Use `--no-exec` on a host that cannot run the bundle
architecture; that mode still checks every structural and hash rule.

Inspect the manifest without building:

```bash
python3 scripts/mcpb.py manifest --target darwin/arm64 --version v0.5.1
```

Smoke test an extracted bundle:

```bash
smoke="$(mktemp -d)"
python3 -m zipfile -e dist/icloud-mcp-v0.5.1-linux-amd64.mcpb "$smoke"
chmod 755 "$smoke/server/icloud-mcp"
"$smoke/server/icloud-mcp" -version
```

The expected output is `v0.5.1`.

`python3 scripts/check_release.py packages --dir dist` verifies every bundle
claimed by `server.json`. It recomputes each SHA-256 and reads each manifest. It
does not run a foreign-platform executable. This is the cross-platform check for
linux/arm64 and darwin/arm64 bundles.

## Install from a downloaded bundle

Download the `.mcpb` file for the host platform from the GitHub release. Open
the file in an MCPB-capable host, or use that host's install command. The host
reads the manifest, asks for the listed settings, and starts:

```text
${__dirname}/server/icloud-mcp
```

The server needs `ICLOUD_EMAIL` and `ICLOUD_PASSWORD` for Calendar or Contacts,
or the Mail address and password for a Mail profile. Keep read-only enabled for
the first run. Check `icloud_capabilities` to see the effective tools.

This document does not claim that a client installation was performed. See the
status table.

## Trust chain

1. The tag workflow builds the release archives, builds one deterministic MCPB
   bundle per target from those archives, and verifies every bundle before
   signing. The native smoke matrix runs each extracted bundle on its own
   platform before the release job starts.
2. `ci.yml` signs each archive, bundle, and the checksum file with cosign
   keyless, then creates the GitHub Release. The checksum file includes the MCPB
   hashes.
3. `server.json` records the MCPB identifier URL and `fileSha256` for each
   claimed platform. `scripts/check_release.py packages` recomputes the hash.
   The release job fails before signing when a hash is wrong or a claimed
   platform is missing.
4. The Registry publication workflow verifies every archive and bundle hash and
   signature again before `mcp-publisher publish`. Verify a downloaded bundle
   with cosign:

```bash
cosign verify-blob \
  --certificate bundle.mcpb.pem \
  --signature bundle.mcpb.sig \
  --certificate-identity "https://github.com/ThomasCrouzet/icloud-mcp/.github/workflows/ci.yml@refs/tags/v0.5.1" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  bundle.mcpb
```

## Status

Checked on 2026-11-08.

| Item | Status |
|------|--------|
| Deterministic bundle build | Verified locally with synthetic and native fixtures. |
| Manifest v0.4 validation | Verified locally with `@anthropic-ai/mcpb` 2.1.2. |
| Linux amd64 bundle binary `-version` | Verified locally. |
| linux/arm64 and darwin/arm64 structure and hash | Verified locally. Native run requires the matching CI runner. |
| Windows bundle | Not produced. Compile only. |
| Release asset build, smoke, and signing of MCPB files | Wired in the tag workflow. No tagged release with bundles exists yet. |
| Official Registry publication with packages | Workflow wired and fail-closed. No publication with packages yet. |
| MCPB client installation | Not performed and not claimed. |
