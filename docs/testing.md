# Testing

## Commands

`make lint` uses golangci-lint v2.15.0 and supports Go 1.27.3.
The linter requires Go 1.26 or newer to build. With an older host toolchain,
Go can download a compatible version when automatic toolchain selection is enabled.
The CI lint step uses Go 1.27.3. Other CI checks and release builds use Go 1.26.10.

Run the complete local gates:

```bash
GOMAXPROCS=2 GOFLAGS=-p=1 make test # go test ./... -race -cover
GOMAXPROCS=2 GOFLAGS=-p=1 make lint # go vet plus pinned golangci-lint
GOMAXPROCS=2 GOFLAGS=-p=1 make build # host-toolchain development binary
make release VERSION=v1.2.3 # packaged linux/arm64 in pinned Go 1.26.10 container
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

CI generates this directory on the runner and discards it after the job.
CI does not upload or archive reports on GitHub. It combines executable and Go
test coverage before it checks existing floors. The executable coverage adds
lifecycle evidence; no coverage threshold is reduced.

## Offline performance baseline

The benchmark suite records deterministic, offline protocol measurements. It
uses synthetic data and local fixtures only. It does not use Apple credentials,
live endpoints, or a service dependency.

Run one finite pass for correctness:

```bash
BENCH_EVIDENCE_DIR=/absolute/external/path/icloud-bench-smoke make bench-smoke
```

Record a bounded full baseline on demand:

```bash
BENCH_EVIDENCE_DIR=/absolute/external/path/icloud-bench-full make bench-full
```

Both targets call `scripts/benchmark_baseline.py` after they run the recorder
regression tests in `scripts/test_benchmark_baseline.py`. The runner needs
Python 3.9 or newer and the project Go toolchain. It refuses an evidence
directory inside the repository. Use a new directory for each run. The full
mode defaults to `-benchtime=500ms -count=3`; override it with the script
options when a stated method requires it. Neither mode defines a timing
threshold or a speedup claim.

The runner removes every inherited `ICLOUD_*` variable before it starts a child
process. It records the removed variable names, never their values. It fixes
`GOMAXPROCS=4` and Go `-p=4`. It records the revision, tree, branch, working
diff hash, untracked files, toolchain, platform, CPU, host load, and available
memory before and after the run.

Each run writes these files outside the repository:

| Artifact | Contents |
|----------|----------|
| `manifest.json` | Source, environment, commands, exits, settings, parsed samples, summary, and limitations |
| `benchmark.log` and `benchmark.err` | Raw Go benchmark output and standard error |
| `benchmarks.json` | Parsed benchmark records, including raw lines |
| `scenario-smoke.log` and `scenario-smoke.err` | One-pass correctness scenarios |
| `environment-before.json` and `environment-after.json` | Host and toolchain snapshots |

### Scenario coverage

| Area | Benchmark | Fixed input | Asserted result |
|------|-----------|-------------|-----------------|
| Calendar recurrence | `BenchmarkRecurrenceExpansion` | `calendar-index-v1`, Europe/Paris, 40 daily occurrences across each 2026 DST change, and 2000 hourly occurrences | Local 09:00 is stable, offsets change, the 2000 per-series cap truncates, and a 2000-count series is complete |
| Calendar imported UID | `BenchmarkCalendarImportedUID` | One imported UID, direct `<uid>.ics` miss, plus or minus 50-year REPORT fallback | Cold state uses 5 RPCs, warm state uses 3, and the internal path stays hidden |
| Calendar fan-out and free slots | `BenchmarkAvailabilityBudget` | 4 calendars with 10,000 events, 1,024 calendars, and overflow cases | Existing accepted scenario retains the 10,000-event availability budget and four-read bound |
| Calendar search and result | `BenchmarkSearchEventsSerialization` | 4 calendars with 10,000 events | 400 public results, 10,000 total, truncation, and 4 RPCs |
| Contacts | `BenchmarkContactsParseSearch2000` | `contacts-index-v1`, 2,000 vCard 3.0 contacts | One query match, no scan cap, 4 cold RPCs, and 1 warm RPC |
| Mail sparse UID | `BenchmarkMailSearchSparseUID` | `mail-pattern-v1`, 30 candidates near UID 4,000,000,000 | 20 results, truncation, one bounded 5,000-UID window, and 3 service-session calls |
| Mail body | `BenchmarkMailGetNearLimitBody` | 204,800 bytes and 204,801 bytes | The exact limit is returned; one byte above it is omitted with `body_too_large` |
| Mail wire metadata | `BenchmarkIMAPWireMetadata` | `imap-wire-metadata-v1`, one synthetic FETCH over `net.Pipe` | One parsed message, one wire FETCH per operation, sparse UID, and curated threading headers |
| MCP refusals | `BenchmarkSearchEventsRefusals` | 10,001 materialized events, invalid limit, synthetic 429, and 2 ms cancellation | Safe error payloads, zero RPCs for input validation, and active cancellation |
| MCP fan-out latency | `BenchmarkSearchEventsFanoutLatency` | 4 calendars with 1 ms synthetic peer latency | 4 RPCs, bounded 4-worker concurrency, and parallel elapsed time |
| MCP schema | `BenchmarkMCPToolSchema` | `search_events` input and output schema | A non-empty serialized schema with declared properties |

The ordinary test scenarios `TestCalendarBenchmarkScenarios`,
`TestContactsBenchmarkScenarios`, `TestMailBenchmarkScenarios`,
`TestIMAPWireBenchmarkScenario`, and `TestMCPBenchmarkScenarios` run under
normal test discovery. They execute each fixture once and keep every business
and refusal assertion. The Mail search scenario runs two operations. The
scenarios prove execution, not statistical stability.

### Metrics and limits

`ns/op` is the measured end-to-end elapsed time per operation after the stated
setup exclusion. `B/op` and `allocs/op` are Go allocation totals per operation.
They are not retained live heap. Custom metrics name RPCs, fixture sizes,
concurrency bounds, injected latency, truncation, and refusals.

`child_peak_rss_kib` is the maximum resident set size of the waited child
process tree. It is a process-level indicator. It cannot establish sampled Go
heap, retained record budgets, or allocation totals. Retained budgets are code
limits and appear as fixture metrics: 2,000 occurrences per series, 10,000
materialized events, 400 public search results, 2,000 scanned contacts, 100,000
per-series recurrence steps, 250,000 aggregate recurrence steps, 204,800 body
bytes, and 524,288 wire body bytes.

RPCs count protocol commands sent by the scenario. Calendar and Contacts metrics
count synthetic DAV HTTP requests. MCP metrics count backend search calls, not
transport frames. Mail `service-call/op` counts calls on the service test
session. Mail `wire-rpc/op` counts actual IMAP FETCH commands parsed through the
synthetic go-imap server and client. Mail session close commands are excluded.

The Mail wire fixture sends one synthetic FETCH response through `net.Pipe` and
parses it with the real go-imap client and adapter conversion. It covers
metadata and curated headers for that response. It does not cover every live
IMAP server behavior, a live mailbox, or Apple interoperability.

The Mail throughput benchmarks set the local read limiter to infinite for the
measurement fixture. They measure per-operation work, not the 60-per-minute
product gate. Existing Mail tests assert that the 11th burst read is rate
limited, and the MCP refusal benchmark covers a synthetic rate-limit response.

The suite covers cold and warm discovery for Calendar imported UID and Contacts.
Calendar fan-out is parallel with at most four workers. Mail search windows and
Contacts book scans are sequential. Synthetic latency, errors, rate-limit
responses, cancellation, and resource refusals are explicit benchmark cases.

Host load, CPU frequency scaling, and background work can move wall-clock
numbers. Compare only runs from the same host with the same method. The suite
does not publish a percentage claim without a separate before-and-after
experiment.

## Manifest-driven MCP contracts

Run the offline contract layer with the existing Go test runner:

```bash
go test ./internal/mcptools ./internal/mail/imapadapter -race -run 'TestContract|TestCuratedHeadersCanonical' -count=1 -v
```

Ordinary test discovery includes these cases in `make check`. No workflow,
dependency, coverage threshold, or product switch is required.

The initialized in-process client obtains actual `tools/list` schemas.
Raw schema bytes preserve large integer bounds before typed SDK maps can round them.
Production and fixture startup share a supported SDK prevalidation hook for
`set_message_flags.expected_modseq`. It clones the decoded map and preserves the
exact unsigned integer value as at most 20 canonical decimal digits. It does not
change raw arguments, other fields, schemas, dependencies, or validation settings.
The Mail raw-number parser also accepts exact integral JSON spellings such as
`1.0`, `1e0`, and `10e-1`. Fractional values, wrong types, and unsigned overflow
still fail before effects. Exponent work is bounded by token length, not magnitude.
Both initialized in-process and bounded-stdio tests retain the full uint64 maximum,
nearby values, overflow and refusal controls, with exact observed backend values.
Production registration, strict SDK input validation, and real domain services handle
the requests. Explicit business fixtures supply values, not replacement schemas.
New tools, top-level properties, and nested properties require explicit fixture values.

The cases exercise required and optional values, omission, declared defaults, wrong
primitive types, unknown keys, enum values, array items, and published boundaries.
Separate counterfactual fixtures contain enough data to observe default and maximum
page caps. Successful writes are read back, or checked at the synthetic protocol peer.
Idempotent repeats must not produce another mutation or SMTP DATA.

Each case logs its request, public result, and backend counters.
Contacts identifier discovery is recorded before measured dispatch.
Schema-invalid and undeclared arguments must not add any backend request or mutation.
Disabled tools must not dispatch. Permission profiles include default, read-only,
single-domain, mixed scoped permissions, and all-enabled configurations.

Every declared success output schema is compiled with the existing JSON Schema
validator. Actual `structuredContent` must validate. Error results have a separate
public error contract, not the success schema. Negative controls alter actual output
field types, remove required fields, and introduce uncovered fixture properties.

Distinguish schema refusal, business validation, and resource policy.
A schema-valid timezone string can fail timezone lookup. A valid collection path
can still produce an overlong complete resource path after a UID suffix is added.
These cases must record the actual refusal and zero mutations, not claim success.
Raw UTF-8 byte limits and the encoded message budget are independent.
Valid raw content can exceed the encoded budget and fail before SMTP.

DAV backends return in-memory data. IMAP uses in-memory connections.
Only the `mcptools` Go test child trusts an ephemeral SMTP fixture root.
The real SMTP adapter still verifies `smtp.mail.me.com`, its certificate chain,
STARTTLS, and TLS 1.2 or newer. No system trust store changes.
SMTP content comparisons account explicitly for DATA framing: `DotReader` converts
CRLF to LF, and the SMTP writer supplies a final line ending.
They do not trim arbitrary content or replace reply handling with send handling.

The pinned header parser returns identifiers without angle brackets.
The adapter restores canonical brackets on successfully parsed Message-ID,
In-Reply-To, and References values before applying existing metadata limits.
Absent identifiers stay absent. Malformed and oversized headers still fail closed.
The real reply fixture checks accepted SMTP output and exact threading headers.

These fixtures do not prove live Apple interoperability, production account access,
or delivery outside the synthetic peer. Keep live integration tests separate.
Store run commands, revisions, source hashes, logs, and coverage matrices outside
the repository. Do not upload these reports.

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
| All domains and all mutation/send capabilities | 24 |

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
- A fake without conditional-update support refuses before STORE. The real
  adapter can send one conditional UID STORE with `UNCHANGEDSINCE`.
- Conditional response validation checks the returned UID, flags, and MODSEQ.
  A malformed FETCH can return `protocol_error` after one STORE.
  Do not describe that result as proof that STORE was never dispatched.
- An SDK schema refusal with zero service calls proves neither conditional
  STORE behavior nor unsupported adapter capability.
- Non-CONDSTORE flag writes are delta-only and cannot set Deleted or keywords.
- Move uses native UID MOVE or a UIDPLUS-only one-message fallback; plain
  EXPUNGE is impossible.
- `move_message` rejects SPECIAL-USE Trash and Junk destinations before
  mutation dispatch. Attribute matching ignores case.
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

Release-tag CI runs a fuzz smoke test for each target in all five packages. A
manual workflow dispatch can run the same tests. CI builds for multiple
architectures and applies a 20 MiB binary limit. It also runs gitleaks and
versioned security AST guards.

`make check` runs the same guards, protocol evidence, and coverage floors as CI.
The guard sources live in `scripts/guards/`. The runner keeps a negative network
fixture: unresolved direct dial and HTTP destinations must fail with both
diagnostics. CI-only checks remain golangci-lint, govulncheck, multi-architecture
builds, fuzz smoke, gitleaks, and release publication.

Package coverage floors are:

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

Tag releases publish only after CI, gitleaks, release metadata, and both native
smoke matrix jobs succeed on the same ref. See the `release` job in
`.github/workflows/ci.yml`. GitHub archives use `make release-all` with Go
1.26.10 pinned. The setting is
`check-latest: false`.

Local `make release` remains the digest-pinned container path for linux/arm64.
CI also smoke-builds `windows/amd64`. GitHub Release archives do not contain
that build.

Tag and manual `native` CI runs extract the matching production archive on
native GitHub-hosted runners. They verify checksums, version output, and an
offline configuration failure. The matrix covers Linux amd64 and macOS arm64.
The macOS arm64 job uses the standard `macos-15` runner.
Linux arm64 remains cross-compiled and archive-tested only. Windows remains
compile-only and has no published archive or runtime support claim.

Live iCloud credentials and the `integration` build tag are never used in CI.

## Release metadata and registry

The release tag, `server.json`, `CHANGELOG.md`, and the public MCP Registry
must agree. The checks are separate.

```bash
make check-release-metadata   # server.json and the dated changelog section
make check-registry           # published registry state, network required
```

`make check-release-metadata` does not use the network or require a tag. It runs
on pull requests, `main`, tags, and manual `native` runs in the `release metadata`
job. It fails on an invalid `server.json`, a missing or empty changelog section,
or a tag that does not match the declared version on a tag push.
Registry-only manual runs do not enable release metadata or native smoke.

`make check-registry` fails when the latest registry version differs from
`server.json`, when the record is missing, or when the response is unreadable.
It runs only through the manual `published registry verification` workflow or
locally after publication. A pending next version does not fail `main`.

The tag job in `ci.yml` builds archives with `make release-all`, signs them with
cosign keyless, and creates the GitHub Release. It does not publish registry
metadata. Publish only after the archives exist.

```bash
mcp-publisher login github
mcp-publisher validate
mcp-publisher publish
make check-registry
curl -fsSL 'https://registry.modelcontextprotocol.io/v0/servers?search=io.github.ThomasCrouzet/icloud-mcp'
```

Verify one archive before publication:

```bash
gh release download vX.Y.Z --repo ThomasCrouzet/icloud-mcp \
  --pattern 'icloud-mcp-vX.Y.Z-linux-amd64.tar.gz*' \
  --pattern 'icloud-mcp-vX.Y.Z-checksums.txt'
grep '  icloud-mcp-vX.Y.Z-linux-amd64.tar.gz$' icloud-mcp-vX.Y.Z-checksums.txt \
  | sha256sum -c -
cosign verify-blob \
  --certificate icloud-mcp-vX.Y.Z-linux-amd64.tar.gz.pem \
  --signature icloud-mcp-vX.Y.Z-linux-amd64.tar.gz.sig \
  --certificate-identity "https://github.com/ThomasCrouzet/icloud-mcp/.github/workflows/ci.yml@refs/tags/vX.Y.Z" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  icloud-mcp-vX.Y.Z-linux-amd64.tar.gz
```

Do not publish when the checksum or signature check fails. `make check-registry`
must print that all four sources agree before you report a release as complete.

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

### Calendar CRUD integration

The Calendar lifecycle test requires explicit write enablement and a disposable
calendar selected by the test harness. It creates timed, all-day, and recurring
fixtures, checks a strong ETag, rejects a stale ETag, changes one occurrence
across a DST transition, deletes one occurrence without deleting the series,
checks that a dry-run delete leaves the resource unchanged, deletes the fixtures,
and verifies absence. It skips without all gates.

```bash
export ICLOUD_EMAIL='file:///run/secrets/icloud-email'
export ICLOUD_PASSWORD='file:///run/secrets/icloud-password'
export ICLOUD_MCP_READ_ONLY='false'
export ICLOUD_MCP_INTEGRATION_WRITES='true'
export ICLOUD_MCP_INTEGRATION_CALENDAR='/123456789/calendars/disposable/'
go test -tags=integration -count=1 -v -timeout=8m -run TestIntegration_CalendarCreateUpdateDeleteAndOccurrence .
```

`ICLOUD_MCP_INTEGRATION_CALENDAR` is a test-only selector. It is not a product
variable and does not change production endpoints. Cleanup targets only the
generated fixture UIDs in that calendar. An ambiguous delete outcome is reported
and never replayed. Logs omit raw live UIDs.

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
