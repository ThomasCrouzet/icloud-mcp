# Contributing

## Development

```bash
make test    # go test ./... -race -cover
make lint    # go vet + golangci-lint (pinned in Makefile / CI)
make build   # local binary under bin/
```

Go 1.25+ is required. Production release binaries are built with
`make release` inside `golang:1.25` (static linux/arm64).

## Rules

- **English only** in tracked files, commits, and tags.
- **No em dash** (U+2014); use commas, colons, or rephrase.
- **No AI tool trailers** (`Co-Authored-By: ...`) in commits or tags.
- Keep the **five direct dependencies** in `go.mod` unless the README is
  updated with a written justification for a new one.
- Do not add `os/exec`, disk writes (beyond boot `file://` secret reads),
  telemetry, or network destinations outside the CalDAV allowlist.

## Security

Report vulnerabilities privately via GitHub
[security advisories](https://github.com/ThomasCrouzet/icloud-mcp/security/advisories/new).
See [SECURITY.md](SECURITY.md) and [docs/security.md](docs/security.md).

## Pull requests

- Prefer small, focused PRs.
- CI must pass: gofmt, vet, golangci-lint, race tests, coverage gate,
  govulncheck, fuzz smoke, security greps, multi-arch build.
- Do not commit secrets, `.env` files, or local agent notes.
