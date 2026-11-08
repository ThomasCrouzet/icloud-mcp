# Platform support and evidence

Checked on 2026-11-08. This page separates what is compiled, what is published,
what is verified from an extracted release archive, and what is executed
natively. A successful cross-compilation is not a runtime support claim.

## Support matrix

| Platform | Compiled in CI | Published archive | Archive verified | Native executed | Support claim |
|----------|----------------|-------------------|------------------|-----------------|---------------|
| `linux/amd64` | Yes | Yes | Yes | Yes, `ubuntu-latest` | Supported |
| `linux/arm64` | Yes | Yes | Yes | Yes, `ubuntu-24.04-arm` | Supported |
| `darwin/arm64` | Yes | Yes | Yes | Yes, `macos-15` | Supported |
| `windows/amd64` | Yes, compile smoke | No | No | No | Not supported, compile-only |
| `darwin/amd64` | No | No | No | No | Not claimed |

Definitions:

- **Compiled in CI** means a `CGO_ENABLED=0` build finishes for the target.
- **Published archive** means `make release-all` packages the target and the
  tagged release uploads it with checksums and Cosign signatures.
- **Archive verified** means the native job extracts the archive produced by the
  release build and checks `icloud-mcp-vX.Y.Z-checksums.txt`. Signature
  verification is a separate release step because signing happens after the
  native smoke job.
- **Native executed** means the extracted binary reports the release version,
  fails closed on an empty configuration, completes a synthetic MCP
  initialize and `tools/list` over stdio, and exits cleanly on SIGTERM. The
  command is `python3 scripts/native_smoke.py`.
- **Support claim** requires native execution plus the documented secret-file
  rules on that platform. Compilation alone is never enough.

`make check-platforms` verifies that this table, the README status table, the
Makefile `TARGETS`, and the CI native matrix agree. It uses no network.

## Native smoke scope

The smoke test runs on the target platform against the extracted archive. It
checks:

1. `icloud-mcp -version` equals the release tag.
2. An empty environment exits nonzero with `configuration error`.
3. A read-only Calendar configuration with `file://` secrets completes an MCP
   `initialize`, reports the same version, lists exactly seven tools, exposes no
   mutation tool, and exits zero on SIGTERM.

The smoke test does not contact iCloud. It does not prove account
compatibility, host installation, or interactive desktop behavior. Live
account evidence is separate; see [Testing](testing.md).

## Secret files by platform

The supported platforms use Unix permission bits. `file://` secrets must be
regular files of at most 4 KiB with mode `0600` or stricter, read once at
startup. The server writes no files after boot.

macOS uses the same check. Linux and macOS are the only platforms with a secret
file support claim. A host that cannot guarantee Unix mode bits should pass
short-lived environment values from its own secret store and accept the host's
threat model.

## Windows decision record

Windows amd64 stays compile-only. It has no published archive and no runtime
support claim.

Reasons:

- `internal/config` enforces mode `0600` with Unix permission bits. On Windows,
  `os.FileMode.Perm` does not represent an access-control list, so the current
  check does not establish equivalent secret-file protection.
- The nonblocking open flag and regular-file checks are not validated on
  Windows.
- Path quoting, spaces, absolute executable paths, stdio cancellation, and MCP
  host launch behavior are not tested on Windows.
- There is no Windows code-signing path and no native GitHub-hosted runner in
  the current matrix.

Required work before any claim:

1. A native Windows amd64 test job that runs the extracted archive.
2. An ACL-based secret-file design using native Windows APIs, with tests for
   inherited permissions and reparse points.
3. Startup, cancellation, stdin EOF, and SIGTERM-equivalent behavior tests.
4. Paths with spaces and shell-independent absolute executable paths.
5. A signed artifact and a documented install and uninstall procedure.

Until then, CI keeps the `GOOS=windows GOARCH=amd64` compile smoke only.
