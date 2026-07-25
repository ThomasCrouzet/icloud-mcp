# Contributing

## Development

```bash
make test    # go test ./... -race -cover
make lint    # go vet + golangci-lint (pinned in Makefile / CI)
make build   # local binary under bin/
```

Go 1.25.12 or newer is required. `make release VERSION=vX.Y.Z` builds and
packages static linux/arm64 with a digest-pinned Go 1.25.12 image.
`make release-all VERSION=vX.Y.Z` cross-compiles and packages linux/amd64,
linux/arm64, and darwin/arm64 with the host toolchain. Release targets reject
an unset or `dev` version and emit SHA-256 checksums. `make install` always
builds for the current host.

## Rules

- **English only** in tracked files, commits, and tags.
- **No em dash** (U+2014); use commas, colons, or rephrase.
- Do not add automated `Co-Authored-By` trailers to commits or tags.
- Keep the **10 direct dependencies** in `go.mod` unless the README is
  updated with a written justification for a new one.
- Do not add `os/exec`, disk writes (beyond boot `file://` secret reads),
  telemetry, private/reverse-engineered Apple APIs, browser/UI automation, or
  network destinations outside the per-domain Calendar, Contacts, IMAP, and
  SMTP allowlists.
- Keep Calendar, Contacts, IMAP, and SMTP credentials, transports/dialers,
  rate limits, semaphores, and destination policies isolated. Do not create a
  union authenticated client or make production endpoints configurable.
- Do not "simplify" hand-rolled CalDAV discovery, REPORT, or conditional
  PUT/DELETE; see [docs/caldav-compatibility.md](docs/caldav-compatibility.md).
- Keep Calendar automatic retries read-only. Never replay PUT, DELETE, or
  full-series delete; ambiguous dispatched mutations must remain
  `outcome_unknown`.
- Keep CardDAV href/redirect validation and strong conditional writes bounded;
  see [docs/carddav-compatibility.md](docs/carddav-compatibility.md).
- Keep IMAP UIDVALIDITY/MODSEQ checks, decode-time limits, UIDPLUS-only fallback,
  mandatory SMTP STARTTLS, recipient authorization, and no-retry submission;
  see [docs/mail-compatibility.md](docs/mail-compatibility.md).
- Keep every production mutation audit on the opaque `domain`/`resourceType`/
  HMAC `resourceToken` shape. Never restore raw Calendar paths, UIDs, mailbox
  identities, or recipients to audit records.
- Preserve the stdio/result/protocol parser caps and recurrence work budget.
  Every parser package must retain native fuzz coverage.

## Security

Report vulnerabilities privately via GitHub
[security advisories](https://github.com/ThomasCrouzet/icloud-mcp/security/advisories/new).
See [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md).

## Pull requests

- Prefer small, focused PRs.
- CI must pass: gofmt, vet, golangci-lint, race tests, coverage gate,
  govulncheck, fuzz smoke across all five parser packages, security greps,
  multi-arch build. Coverage must meet the documented package floors and 78%
  aggregate threshold.
- Do not commit secrets, `.env` files, or local agent notes.
