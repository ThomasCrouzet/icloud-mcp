# Contributing

## Development

```bash
make test    # go test ./... -race -cover
make lint    # go vet + golangci-lint (pinned in Makefile / CI)
make build   # local binary under bin/
```

You must use Go 1.25.13 or newer. `make release VERSION=vX.Y.Z` uses a
digest-pinned Go 1.25.13 image. It builds and packages static linux/arm64.
`make release-all VERSION=vX.Y.Z` uses the host toolchain. It cross-compiles
and packages linux/amd64, linux/arm64, and darwin/arm64. GitHub tag releases
use this target after CI and gitleaks succeed.

Release targets reject an unset version or a `dev` version. They also make
SHA-256 checksums. `make install` always builds for the current host.

## Rules

- **English only** in tracked files, commits, and tags.
- **No em dash** (U+2014). Use commas, colons, or different wording.
- Do not add automated `Co-Authored-By` trailers to commits or tags.
- Keep the **10 direct dependencies** in `go.mod`. If you add one, add a
  written justification to the README.
- Do not add `os/exec`, telemetry, private or reverse-engineered Apple APIs,
  browser automation, or UI automation. Do not add network destinations
  outside the Calendar, Contacts, IMAP, and SMTP allowlists. Do not write to
  disk. Read files only for boot-time `file://` secrets. These reads accept only
  regular files with mode 0600 or stricter.
- Keep separate credentials, transports, dialers, rate limits, semaphores, and
  destination policies for Calendar, Contacts, IMAP, and SMTP. Do not create
  one authenticated client for multiple domains. Do not make production
  endpoints configurable.
- Do not change the hand-written CalDAV discovery, REPORT, or conditional
  PUT/DELETE without reading
  [docs/caldav-compatibility.md](docs/caldav-compatibility.md).
- Retry only Calendar reads. Never replay PUT, DELETE, or a full-series delete.
  Keep `outcome_unknown` for ambiguous mutations after dispatch.
- Keep bounds on CardDAV href and redirect validation. Keep strong conditional
  writes. See [docs/carddav-compatibility.md](docs/carddav-compatibility.md).
- Keep the IMAP UIDVALIDITY and MODSEQ checks. Keep the decode-time limits and
  the UIDPLUS-only fallback. SMTP must use STARTTLS and must authorize each
  recipient. Never retry SMTP submission. See
  [docs/mail-compatibility.md](docs/mail-compatibility.md).
- Each production mutation audit must include `domain`, `resourceType`, and the
  opaque HMAC `resourceToken`. Never put raw Calendar paths, UIDs,
  mailbox identities, or recipients in audit records.
- Keep the stdio, result, and protocol parser limits. Keep the recurrence work
  limit. Each parser package must have native fuzz coverage.

## Writing

Use the principles of
[ASD-STE100 Issue 9](https://www.asd-ste100.org/assets/files/ASD-STE100_ISSUE9.pdf)
for project documentation. Use short sentences, active voice, and one term for
each concept. Use a maximum of 20 words in procedural sentences and 25 words in
descriptive sentences. Keep a paragraph to six sentences or fewer. These rules
are project guidelines and do not claim ASD-STE100 certification.

## Security

Report vulnerabilities privately via GitHub
[security advisories](https://github.com/ThomasCrouzet/icloud-mcp/security/advisories/new).
See [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md).

## Support and maintenance

These voluntary maintainer targets are not a paid SLA:

- **Issue triage:** within 5 business days when capacity allows.
- **PR review:** about 2 weeks for ordinary changes. We aim to review critical
  security fixes in a few days.
- **Supported versions:** latest release plus one prior minor line
  (for example v0.3.x and v0.2.x while both are current).
- **Breaking changes:** announced in CHANGELOG at least one release in advance
  when practical.
- **Security fixes:** expedited and backported to the prior supported line when
  feasible. See [SECURITY.md](SECURITY.md).

## Pull requests

- Prefer small, focused PRs.
- CI must pass all checks. These checks include gofmt, vet, golangci-lint, race
  tests, the coverage gate, govulncheck, and fuzz smoke for all five parser
  packages. They also include security AST guards, multi-architecture builds,
  gitleaks, and the public-text policy. The build check includes a
  windows/amd64 smoke build. The public-text policy checks the tracked tree and
  new commit messages in the PR or push range. Coverage must meet the package
  floors and the 78% aggregate threshold.
- Tag releases require green CI.
- Do not commit secrets, `.env` files, or local agent notes.
- Do not create or store audit or review reports in this repository.
  Keep them outside it, including reports that Git would ignore.
- Label roadmap work with `phase-1`, `phase-2`, or `phase-3` when applicable
  (see [ROADMAP.md](ROADMAP.md)).
