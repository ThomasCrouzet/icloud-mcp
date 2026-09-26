# Testing

## Commands

`make lint` uses golangci-lint v2.13.2 and supports Go 1.27.1.
The linter requires Go 1.26 or newer to build. With an older host toolchain,
Go can download a compatible version when automatic toolchain selection is enabled.
The CI lint step uses Go 1.27.1. Other CI checks and release builds use Go 1.26.8.

Run the complete local gates:

```bash
GOMAXPROCS=2 GOFLAGS=-p=1 make test # go test ./... -race -cover
GOMAXPROCS=2 GOFLAGS=-p=1 make lint # go vet plus pinned golangci-lint
GOMAXPROCS=2 GOFLAGS=-p=1 make build # host-toolchain development binary
make release VERSION=v0.4.0 # packaged linux/arm64 in pinned Go 1.26.8 container
```

Useful focused commands for the unified domains:

```bash
go test ./internal/config ./cmd/icloud-mcp -race -count=1
go test ./internal/contacts -race -count=1
go test ./internal/mail/... -race -count=1
go test ./internal/mcptools ./internal/security -race -count=1
go test ./... -race -shuffle=on -count=10
```

Run one test or package while iterating:

```bash
go test ./internal/contacts -run TestClient -race -v
go test ./internal/mail/... -run TestService -race -v
go test ./internal/mcptools -run TestRegister -race -v
go test ./internal/icloud -run TestExpandOccurrences -race -v
```

All five parser and security packages must have native fuzz targets:

| Package | Targets |
|---------|---------|
| `./internal/icloud` | `FuzzValidateCalendarPath`, `FuzzValidateUID`, `FuzzParseDateTime`, `FuzzValidateRRULE`, `FuzzRedactLikeEventRoundTrip`, `FuzzExpandOccurrences`, `FuzzResolveDAVHref` |
| `./internal/security` | `FuzzRedactor`, `FuzzRedactingWriter`, `FuzzIsICloudHost` |
| `./internal/contacts` | `FuzzContactsDAVXML`, `FuzzStrictVCardDecode` |
| `./internal/mail` | `FuzzParseRecipientPolicy`, `FuzzDecodePlainBody` |
| `./internal/mail/imapadapter` | `FuzzIMAPInboundGuard`, `FuzzCompactUIDSetExpansion` |

Use the CI discovery loop to run each target in all five packages:

```bash
for package in ./internal/icloud ./internal/security ./internal/contacts ./internal/mail ./internal/mail/imapadapter; do
  for target in $(go test "$package" -list '^Fuzz' | awk '/^Fuzz[[:alnum:]_]+$/ { print }'); do
    go test "$package" -run=^$ -fuzz="^${target}$" -fuzztime=10s
  done
done
```

## Test layers

| Layer | Location | Coverage |
|-------|----------|----------|
| Configuration | `internal/config`, `cmd/icloud-mcp` | strict booleans, all capability combinations, child-gate errors, file secrets, Mail fallback, recipient policy, optional client construction |
| Calendar unit/fake DAV | `internal/icloud` | discovery, REPORT, iCalendar, recurrence work budgets, free slots, conditional PUT/DELETE, read-only retries, ambiguous mutation outcomes, limits |
| Contacts fake CardDAV | `internal/contacts` | Lazy discovery, redirects/hrefs, and XML/vCard bounds.<br>Server prefilter, combined local search, phone all-card path, vCard 3.0/4.0, ETag CRUD, and outcome classification. |
| IMAP adapter | `internal/mail/imapadapter` | fresh login sessions, username fallback, protocol guard, BODYSTRUCTURE, PEEK, MOVE/UIDPLUS commands |
| Mail service/fake sessions | `internal/mail` | UIDVALIDITY, UID-window search, MIME output, flag/move/trash safety, SMTP recipient and failure matrices |
| MCP contract | `internal/mcptools` | schemas, handlers, exact registration counts, capability manifest, audit/error/redaction paths |
| Security | `internal/security` | all four destination policies, ports/TLS, dial-before-DNS rejection, encoded secret variants, audit tokens |
| MCP in-process | `internal/mcptools` | Protocol dispatch, stateful idempotency fixtures, real Calendar client with synthetic TLS DAV, capabilities, panic redaction |
| Executable protocol | `scripts/protocol_evidence.py`, `cmd/icloud-mcp/protocol_fixture_test.go` | Shared startup, discovery, bounded stdio, registration, cancellation, domain failure isolation, shutdown |
| Integration | root `integration_test.go`, build tag `integration` | real iCloud Calendar reads, opt-in Contacts reads/CRUD, explicitly gated Mail reads/mutation/self-send with exact fixture cleanup, local validation/free slots |

Handlers run concurrently. Thus, test all domain and MCP packages with `-race`.
Test values at each exact limit and one unit above it. This rule applies to DAV
bodies, vCards, IMAP session data, MIME sections, SMTP messages, and serialized
results.

These tests also cover the 1 MiB stdio frame and 64 KiB reflected-error limit.
They cover the 256 KiB Calendar and MCP result limit. They also cover the 1 MiB
SMTP inbound limit. Parser tests cover XML, IMAP, and MIME depth and item limits.
Recurrence tests cover per-series and total work limits.

## Repeatable protocol evidence

Use Python 3.9 or newer and the project Go toolchain.
Select a new output directory outside the repository:

```bash
make protocol-evidence EVIDENCE_DIR=/absolute/external/path/icloud-protocol-run
```

The runner limits Go to two processors and one build job. It executes each
scenario in sequence. It does not inherit product credentials or proxy settings.
All identities, passwords, and remote content are synthetic.

The runner compiles the `cmd/icloud-mcp` test executable with race detection and
coverage. Its test-only entry point supplies synthetic domain transports.
The executable uses the product configuration loader, discovery, lifecycle,
registration, middleware, and bounded stdio code. It is not the release binary.
Production endpoint policies remain fixed; fixture switches exist only in tests.

Executable scenarios check:

- Initialization from a split input frame and the exact capability manifest.
- Default, read-only, full, and Mail-read-only tool inventories.
- Direct calls to absent mutation tools, which must return protocol errors.
- Contacts authentication and malformed XML failures through the real client.
- IMAP authentication and malformed greeting failures through the real adapter.
- Calendar reads and unchanged capabilities after repeated optional-domain failures.
- Request cancellation, followed by a successful Calendar read.
- Frames at 1 MiB and one byte above it. Oversized input must not reach dispatch.
- Clean EOF and SIGTERM shutdown, including a Calendar request in progress.

Calendar and Contacts fixture transports return in-memory DAV responses.
The IMAP fixture uses an in-memory connection. These scenarios do not test live
iCloud access or production TLS connections.

The same run executes `TestProtocol` scenarios in `internal/mcptools`:

- Stateful update fixtures verify success, conflict, canceled waiters, ambiguous
  dispatch, concurrent callers, cache expiry, and capacity.
- Calendar scenarios use the real client with a local TLS DAV server.
  Synthetic iCalendar files combine overrides, EXDATE, DST, and all-day events.
- Exact occurrences and free slots cover 23-hour and 25-hour days.
  Limit fixtures cover truncation, iterator work, aggregate work, and materialization.
  Incomplete busy data must never produce free slots.

Each output directory contains:

| Artifact | Contents |
|----------|----------|
| `manifest.json` | Commands, revision, environment, working diff hash, source and fixture hashes, results, artifact hashes |
| `working.patch` | Tracked changes relative to the recorded revision |
| `fixtures/` | Protocol test sources, runner source, and synthetic data used by the run |
| `*.jsonl` | Executable input/output, stderr, exit status, and in-process request/result records |
| `*.coverage.out` | Per-process executable coverage |
| `executable-coverage.out` | Combined executable coverage |
| `protocol-fixture` | The compiled fixture executable |

Large input frames are recorded as a base message, padding length, and SHA-256.
This representation reconstructs the exact bytes without repeated padding in logs.
The runner saves failure metadata before it exits with a nonzero status.
It refuses an existing output directory to preserve previous evidence.

CI uploads this directory as `protocol-evidence` for 14 days, including failed
runs. It combines executable and Go test coverage before it checks existing floors.
The executable coverage adds lifecycle evidence; no coverage threshold is reduced.

## Capability matrix tests

The five booleans are:

`ICLOUD_MCP_READ_ONLY`, `ICLOUD_MCP_ENABLE_CONTACTS`,
`ICLOUD_MCP_ENABLE_MAIL`, `ICLOUD_MCP_ENABLE_MAIL_WRITE`, and
`ICLOUD_MCP_ENABLE_MAIL_SEND`.

Configuration and registration tests cover these combinations. They include child
flags without Mail and a missing SMTP recipient policy. They also include global
read-only suppression and exact tool inventories. The important expected counts
are:

| Scenario | Count |
|----------|-------|
| Default Calendar read/write plus global capability | 10 |
| Calendar global read-only | 7 |
| Calendar plus Contacts read/write | 16 |
| Calendar plus Mail read | 13 |
| All domains and all mutation/send capabilities | 23 |

`icloud_capabilities.tools` and `toolCount` must match the server inventory.
Disabled tools must be absent. Do not install handlers that return
`feature_disabled` for them.

## Mutation safety properties

Calendar and Contacts:

- Create sends `If-None-Match: *`.
- Real update/delete sends a specific strong `If-Match` after a full GET.
- Missing/weak/wildcard ETags fail closed.
- `dry_run` records no PUT/DELETE.
- Contacts vCard 3.0 update preserves opaque fields; vCard 4.0 and groups remain
  read-only.
- A known successful PUT can have a failed normalization GET. This result stays
  successful with `resultIncomplete` and is not ambiguous.
- Calendar retries only reads. It never repeats PUT, DELETE, or full-series
  delete. An ambiguous dispatched mutation returns `outcome_unknown`.
- Keyed updates keep ambiguous outcomes until process exit. Duplicate callers
  cannot release pending claims. See the [cache contract](architecture.md#update-idempotency).

Mail:

- Search/get reads use read-only SELECT (EXAMINE) and PEEK.
- Each message reference includes UIDVALIDITY. A mismatch occurs before mutation.
- A CONDSTORE server cannot receive unconditional STORE when MODIFIED detection
  is unavailable.
- The beta.8 path returns `protocol_error` before STORE. It does not report
  `concurrent_modification`.
- Non-CONDSTORE flag writes are delta-only and cannot set Deleted or keywords.
- Move uses native UID MOVE or a UIDPLUS-only one-message fallback; plain
  EXPUNGE is impossible.
- Trash requires exactly one selectable SPECIAL-USE Trash target.
- All SMTP recipients pass the local policy before connection. All RCPT commands
  pass before DATA. Message headers exclude Bcc.
- Each of `to`, `cc`, and `bcc` is optional. Together, they contain at least one
  recipient.
- SMTP does not retry. An ambiguous failure after DATA maps to
  `outcome_unknown`.

## CI gates

`.github/workflows/ci.yml` runs formatting, vet, and pinned golangci-lint. It
runs race tests with a 78% total coverage threshold. It also runs
`govulncheck`, module verification, and tidy checks.

CI runs a fuzz smoke test for each target in all five packages. It builds for
multiple architectures and applies a 20 MiB binary limit. It also runs gitleaks
and security source guards. Package coverage floors are:

| Package | Floor |
|---------|-------|
| `internal/config` | 85% |
| `internal/health` | 90% |
| `internal/security` | 80% |
| `internal/icloud` | 78% |
| `internal/mcptools` | 75% |
| `internal/contacts` | 65% |
| `internal/mail` | 65% |
| `internal/mail/imapadapter` | 60% |
| `cmd/icloud-mcp` | 55% |

Tag releases publish only after the CI and gitleaks jobs succeed on the same
ref. See the `release` job in `.github/workflows/ci.yml`. GitHub archives use
`make release-all` with Go 1.26.8 pinned. The setting is
`check-latest: false`.

Local `make release` remains the digest-pinned container path for linux/arm64.
CI also smoke-builds `windows/amd64`. GitHub Release archives do not contain
that build.

Live iCloud credentials and the `integration` build tag are never used in CI.

## Real iCloud integration

With valid credentials, the checked-in build-tagged suite always tests Calendar
reads and local Calendar calculations. Contacts and Mail tests have additional
product-domain gates. Contacts CRUD also has a test-only write gate.

The Mail mutation and send test has additional product write and send gates.
Global read-only must be explicitly false. A test-only self-recipient gate must
match an exact recipient policy. This policy cannot contain a wildcard.

These live tests are optional, use credentials, and never run in CI. Passing unit
tests do not prove that a live iCloud run occurred.

### Calendar integration command

Use a dedicated account or disposable Calendar where possible:

```bash
export ICLOUD_EMAIL='you@icloud.com'
export ICLOUD_PASSWORD='xxxx-xxxx-xxxx-xxxx'
export ICLOUD_MCP_READ_ONLY='true'
export ICLOUD_MCP_DEFAULT_TZ='Europe/Paris'
go test -tags=integration -count=1 -v -timeout=120s .
```

You can also use `file://` values. Use a regular file of 4 KiB or less, with mode
0600 or stricter:

```bash
export ICLOUD_EMAIL='file:///run/secrets/icloud-email'
export ICLOUD_PASSWORD='file:///run/secrets/icloud-password'
# chmod 600 the secret files before boot
go test -tags=integration -count=1 -v -timeout=120s .
```

Without valid credentials, the integration test skips or fails during Calendar
discovery. Never weaken TLS or an allowlist to pass a live test.

### Optional-domain read integration

Keep global read-only enabled and opt in only to the domain under test.

Contacts read:

```bash
export ICLOUD_MCP_READ_ONLY='true'
export ICLOUD_MCP_ENABLE_CONTACTS='true'
go test -tags=integration -count=1 -v -timeout=120s .
```

Mail read:

```bash
export ICLOUD_MCP_READ_ONLY='true'
export ICLOUD_MCP_ENABLE_MAIL='true'
export ICLOUD_MAIL_ADDRESS='mailbox@icloud.com'
export ICLOUD_MAIL_PASSWORD='dedicated-mail-app-password'
go test -tags=integration -count=1 -v -timeout=120s .
```

Contacts integration discovers and lists books. It runs a bounded search and
gets one existing contact when available. Mail integration lists selectable
mailboxes and searches at most four candidates. It gets one message twice and
verifies that Seen does not change.

The Contacts enable flag is the live integration gate. Without it, the Contacts
tests skip.

### Explicit write opt-ins

`ICLOUD_MCP_INTEGRATION_WRITES` and
`ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT` are test harness variables. They are not
part of the binary's 12-variable product contract. The harness removes
surrounding spaces from the write gate. It accepts the gate only when its value
equals `true`, without case sensitivity.

Contacts CRUD requires all of:

```bash
export ICLOUD_MCP_ENABLE_CONTACTS='true'
export ICLOUD_MCP_READ_ONLY='false'
export ICLOUD_MCP_INTEGRATION_WRITES='true'
go test -tags=integration -run=TestIntegration_ContactsCreateUpdateDelete \
  -count=1 -v -timeout=5m .
```

The test selects a discovered writable book that supports vCard 3.0. It creates
one contact with a unique identity. The contact has independent opaque random
`FN`, structured `N`, `EMAIL`, `TEL`, and `ORG` values.

The test checks `query` matching for `FN`, `N`, `EMAIL`, and `ORG`. It checks the
exact email filter and a digits-only phone filter. It also reads the UID before
update. Deferred cleanup deletes only the generated UID.

Logs contain fixed labels, Boolean values, and counts. They never contain
fixture values or live contact data.

The Mail mutation/send gate test requires all of:

```bash
export ICLOUD_MCP_ENABLE_MAIL='true'
export ICLOUD_MCP_ENABLE_MAIL_WRITE='true'
export ICLOUD_MCP_ENABLE_MAIL_SEND='true'
export ICLOUD_MCP_READ_ONLY='false'
export ICLOUD_MAIL_ADDRESS='mailbox@icloud.com'
export ICLOUD_MAIL_PASSWORD='dedicated-mail-app-password'
export ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS='mailbox@icloud.com'
export ICLOUD_MCP_INTEGRATION_WRITES='true'
export ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT='mailbox@icloud.com'
go test -tags=integration -run=TestIntegration_MailMutationAndSend \
  -count=1 -v -timeout=10m .
```

Set all five Boolean values above explicitly to the shown values. The
self-recipient must equal the normalized configured Mail address. An exact
product SMTP policy must permit this address. The test rejects the literal `*`.

Before SMTP, the test also requires a complete mailbox list. It requires exactly
one selectable SPECIAL-USE Trash mailbox. This mailbox gives cleanup a safe
target.

The test builds the full Mail service with the fixed security IMAP and SMTP
dialers. It parses the recipient policy separately. Then, it submits one opaque
plain-text self-message. It verifies the SMTP accepted and recipient outcome
model.

The test polls each selectable mailbox with independent opaque subject and body
queries. It verifies UIDVALIDITY and makes sure that Seen does not change. It
tests flag addition and removal when safe.

On a CONDSTORE server, the test permits the deliberate beta.8 `protocol_error`
path. It first reads the flags to prove that none changed.

Move first selects a distinct selectable SPECIAL-USE Archive mailbox. If none is
available, it selects another destination that is not Trash. It finds the
fixture again with an opaque query and moves it to Trash.

A missing optional destination or safe move capability skips only that subtest
after submission. Deferred cleanup searches each selectable mailbox for fixture
copies. It moves each copy to Trash. It never uses permanent delete or plain
EXPUNGE.

Logs contain fixed labels, Boolean values, and counts. They do not contain
addresses, subjects, message IDs, bodies, or mailbox names.

SMTP `accepted` confirms server acceptance. It does not confirm final delivery.
Polling and cleanup have limits. Thus, a copy that appears after the cleanup
window can remain.

Successful cleanup intentionally leaves the disposable fixture in Trash. The
product has no permanent-delete operation.

Revoke the app-specific password after testing if you created it only for this
run. Never commit credentials, raw DAV, vCard, or MIME captures. Never commit
mailbox content, recipient lists, or live resource identifiers.
