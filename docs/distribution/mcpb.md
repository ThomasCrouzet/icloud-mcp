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
| Linux arm64 | Cross build | Packaged. Native bundle smoke passes on `ubuntu-24.04-arm` in the tag workflow. |
| macOS arm64 | Cross build | Packaged. Native bundle smoke passes on `macos-15` in the tag workflow. |
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
go build -trimpath -ldflags='-s -w -X main.version=v1.0.0' \
  -o "$stage/icloud-mcp" ./cmd/icloud-mcp
cp LICENSE THIRD_PARTY_NOTICES.md "$stage/"
python3 scripts/mcpb.py build \
  --source "$stage" --target linux/amd64 --version v1.0.0
python3 scripts/mcpb.py verify \
  dist/icloud-mcp-v1.0.0-linux-amd64.mcpb
smoke="$(mktemp -d)"
python3 -m zipfile -e dist/icloud-mcp-v1.0.0-linux-amd64.mcpb "$smoke"
npx -y @anthropic-ai/mcpb@2.1.2 validate "$smoke/manifest.json"
npx -y @anthropic-ai/mcpb@2.1.2 info \
  dist/icloud-mcp-v1.0.0-linux-amd64.mcpb
```

The build accepts a release archive instead of a directory:

```bash
python3 scripts/mcpb.py build \
  --source dist/icloud-mcp-v1.0.0-linux-amd64.tar.gz \
  --target linux/amd64 --version v1.0.0
```

`verify` prints one JSON object. It checks the entry set, manifest fields,
version equality with `server.json`, executable mode, one platform, path and
credential rules, and the extracted binary. It runs `-version` and requires the
reported version to equal the manifest version. A failure prints a JSON error
and exits with status 1. Use `--no-exec` on a host that cannot run the bundle
architecture; that mode still checks every structural and hash rule.

Inspect the manifest without building:

```bash
python3 scripts/mcpb.py manifest --target darwin/arm64 --version v1.0.0
```

Smoke test an extracted bundle:

```bash
smoke="$(mktemp -d)"
python3 -m zipfile -e dist/icloud-mcp-v1.0.0-linux-amd64.mcpb "$smoke"
chmod 755 "$smoke/server/icloud-mcp"
"$smoke/server/icloud-mcp" -version
```

The expected output is `v1.0.0`.

`python3 scripts/check_release.py packages --dir dist` verifies every bundle
claimed by `server.json`. It recomputes each SHA-256 and reads each manifest. It
does not run a foreign-platform executable. This is the cross-platform check for
linux/arm64 and darwin/arm64 bundles.

## Install from a downloaded bundle

**Host installation status: UNVERIFIED.** The bundle is built, hashed, signed,
validated, and started natively. A real desktop host install, settings
persistence, secret storage, upgrade, and uninstall have not been recorded. The
exact open work is [#126](https://github.com/ThomasCrouzet/icloud-mcp/issues/126).

### Pinned inputs required from the user

A verifiable install needs these values supplied by the maintainer or user.
Without them, the steps below are a procedure, not a test result.

| Input | Current value |
|---|---|
| MCPB-capable host name | Not recorded |
| Host version and build | Not recorded |
| OS and architecture | Candidate platforms are Linux amd64/arm64 and macOS arm64 |
| Install mechanism | Host UI, host CLI, or file association; not recorded |
| User profile state | Must be clean; not recorded |

Do not report a host as supported until the issue records these values and the
observed results for install, first start, restart, upgrade, disable, and
uninstall.

### Procedure

1. Download the `.mcpb` file for the host platform from the GitHub release.
   Verify its SHA-256 and Cosign signature with the matching checksum, `.sig`,
   and `.pem` files.
2. Open the file in an MCPB-capable host, or use that host's install command.
   The host reads the manifest, asks for the listed settings, and starts:

   ```text
   ${__dirname}/server/icloud-mcp
   ```

3. Enter `icloud_email` and `icloud_password`. Keep `read_only=true`,
   `permissions=calendar.read`, `enable_contacts=false`, and
   `enable_mail=false`. Set `default_timezone`.
4. Confirm the host starts one process, lists exactly seven read-only tools,
   and reports `readOnly=true` from `icloud_capabilities`.
5. Fully quit and restart the host. Confirm the settings survive.
6. Disable and uninstall the connector. Confirm the process stops and the
   settings are removed, or record which settings survive. Revoke the
   app-specific password at Apple.
7. Record host name, version, OS/arch, bundle SHA-256, observed results, and
   cleanup in the privacy-safe evidence template. Never include credentials,
   account identifiers, or transcript payloads.

If the host cannot store secrets, use a documented `file://` value. If the host
rejects manifest version `0.4` or the bundle platform, use the release archive
and report the rejection, with no credential, in #126.

The server needs `ICLOUD_EMAIL` and `ICLOUD_PASSWORD` for Calendar or Contacts,
or the Mail address and password for a Mail profile. Keep read-only enabled for
the first run. Check `icloud_capabilities` to see the effective tools.

See the [synthetic first-run transcript](../demo/read-only-transcript.md) for
the expected read-only shape without a host or account.

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
  --certificate-identity "https://github.com/ThomasCrouzet/icloud-mcp/.github/workflows/ci.yml@refs/tags/v1.0.0" \
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
| linux/arm64 and darwin/arm64 structure, hash, and native run | Verified on the matching CI runners for `v1.0.0`. |
| Windows bundle | Not produced. Compile only. |
| Release asset build, smoke, and signing of MCPB files | Verified for `v1.0.0`: three signed bundles with hashes in the checksums file. |
| Official Registry publication with packages | Published `1.0.0` with three packages on 2026-11-08 at `19:08:03Z`; `make check-registry` passes. |
| MCPB client installation | Not performed and not claimed. Host name, version, OS/arch, install mechanism, settings persistence, secret storage, upgrade, and uninstall are unverified (#126). |
