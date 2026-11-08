# 0002 Windows support

- Status: research decision, accepted 2026-11-08.
- Issue: [#133](https://github.com/ThomasCrouzet/icloud-mcp/issues/133).
- Scope: packaging and runtime support for `windows/amd64`.
- Decision owner: maintainers.

## Decision

Keep `windows/amd64` compile-only. Do not publish a Windows archive, MCPB
bundle, or directory listing. Do not claim Windows compatibility.

The current evidence does not justify the security work that a Windows runtime
claim requires. This is a deliberate no-go, not an unexamined omission.

## Demand

No concrete Windows request is recorded in the issue tracker, the public
directory listings, or the host profiles in this repository. The only Windows
artifact is the CI compile smoke. A compile result does not measure demand, and
it does not test credential handling.

Demand alone would not be enough. A Windows claim also needs the security and
runtime evidence below.

## Why compile-only remains

- `internal/config` relies on Unix mode bits (`0600` or stricter) for `file://`
  secrets. On Windows, `os.FileMode.Perm()` is not an access-control list. The
  same numeric mode does not establish an equivalent owner-only ACL.
- Reparse points, inherited ACLs, and a secret file opened through a link need
  native validation that the current code does not perform.
- The nonblocking open, regular-file, path, and stdio behavior are not exercised
  on Windows.
- No Windows code-signing path exists. An unsigned archive would weaken the
  current release chain.
- The native release matrix has no Windows runner, so the exact published bytes
  could not pass the same native smoke as Linux and macOS.

## Reopening criteria

Open a new implementation issue only when all of these are true:

1. A recorded user or host demand identifies a specific Windows MCP host and
   version.
2. The secret-file design uses native Windows ACL APIs and rejects inherited or
   overly broad access, reparse points, and non-regular files.
3. Native Windows CI runs the exact extracted release bytes: startup, version,
   missing credentials, read-only `tools/list`, paths with spaces, stdin EOF,
   cancellation, and process termination.
4. The archive and MCPB bundle are signed and hash-verified through the same
   candidate pipeline as the other platforms.
5. `scripts/check_platforms.py`, README, release metadata, and the MCPB manifest
   agree on the new claim.

Until then, the compile smoke stays. It proves only that the Go source builds
for the target.

## Evidence basis

| Claim | Source | Label |
|---|---|---|
| Windows builds without CGO | CI `build` step, `GOOS=windows GOARCH=amd64` | Code-verified |
| Unix mode bits are the secret-file policy | `internal/config`, `docs/security.md` | Code-verified |
| No Windows demand is recorded | Issue tracker and repository profiles at 2026-11-08 | Repository-observed |
| Windows ACL and host behavior | No Windows run was performed | Unverified |
| Windows directory listings | Not published | Repository-observed |

## Consequences

- README and directory text keep Windows outside the supported platforms.
- No Windows download link is added to a release.
- A future request can reopen the decision without changing the current
  security policy by accident.
