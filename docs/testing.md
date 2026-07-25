# Testing

## Commands

Run the complete local gates:

```bash
make test                 # go test ./... -race -cover
make lint                 # go vet plus pinned golangci-lint
make build                # host-toolchain development binary
make release VERSION=v0.3.0 # packaged linux/arm64 in pinned Go 1.25.x container
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

Native fuzz targets are mandatory in all five parser/security packages:

| Package | Targets |
|---------|---------|
| `./internal/icloud` | `FuzzValidateCalendarPath`, `FuzzValidateUID`, `FuzzParseDateTime`, `FuzzValidateRRULE`, `FuzzRedactLikeEventRoundTrip`, `FuzzExpandOccurrences`, `FuzzResolveDAVHref` |
| `./internal/security` | `FuzzRedactor`, `FuzzRedactingWriter`, `FuzzIsICloudHost` |
| `./internal/contacts` | `FuzzContactsDAVXML`, `FuzzStrictVCardDecode` |
| `./internal/mail` | `FuzzParseRecipientPolicy`, `FuzzDecodePlainBody` |
| `./internal/mail/imapadapter` | `FuzzIMAPInboundGuard`, `FuzzCompactUIDSetExpansion` |

Run every target in all five packages with the same discovery loop used by CI:

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
| Contacts fake CardDAV | `internal/contacts` | lazy discovery, redirects/hrefs, XML/vCard bounds, server-prefilter plus combined local search, phone all-card path, vCard 3.0/4.0, ETag CRUD, outcome classification |
| IMAP adapter | `internal/mail/imapadapter` | fresh login sessions, username fallback, protocol guard, BODYSTRUCTURE, PEEK, MOVE/UIDPLUS commands |
| Mail service/fake sessions | `internal/mail` | UIDVALIDITY, UID-window search, MIME output, flag/move/trash safety, SMTP recipient and failure matrices |
| MCP contract | `internal/mcptools` | schemas, handlers, exact registration counts, capability manifest, audit/error/redaction paths |
| Security | `internal/security` | all four destination policies, ports/TLS, dial-before-DNS rejection, encoded secret variants, audit tokens |
| MCP end-to-end | in-process MCP client | `tools/list`, global read-only, domain combinations, capabilities, panic redaction |
| Integration | root `integration_test.go`, build tag `integration` | real iCloud Calendar reads, opt-in Contacts reads/CRUD, explicitly gated Mail reads/mutation/self-send with exact fixture cleanup, local validation/free slots |

Handlers are concurrent, so all domain and MCP packages should be tested with
`-race`. Exact-cap and cap-plus-one cases are important for DAV bodies, vCards,
IMAP session data, MIME sections, SMTP messages, and serialized results.
They also cover the 1 MiB stdio frame, 64 KiB reflected-error threshold, 256 KiB
Calendar/MCP result budget, 1 MiB SMTP inbound budget, XML/IMAP/MIME parser
depth and item caps, and per-series/aggregate recurrence work budgets.

## Capability matrix tests

The five booleans are:

`ICLOUD_MCP_READ_ONLY`, `ICLOUD_MCP_ENABLE_CONTACTS`,
`ICLOUD_MCP_ENABLE_MAIL`, `ICLOUD_MCP_ENABLE_MAIL_WRITE`, and
`ICLOUD_MCP_ENABLE_MAIL_SEND`.

Configuration and registration tests cover their combinations, including child
flags without Mail, missing SMTP recipient policy, global read-only suppression,
and exact tool inventories. Important expected counts are:

| Scenario | Count |
|----------|-------|
| Default Calendar read/write plus global capability | 10 |
| Calendar global read-only | 7 |
| Calendar plus Contacts read/write | 16 |
| Calendar plus Mail read | 13 |
| All domains and all mutation/send capabilities | 23 |

`icloud_capabilities.tools` and `toolCount` must match the actual server
inventory. Disabled tools must be absent, not installed as handlers that return
`feature_disabled`.

## Mutation safety properties

Calendar and Contacts:

- Create sends `If-None-Match: *`.
- Real update/delete sends a specific strong `If-Match` after a full GET.
- Missing/weak/wildcard ETags fail closed.
- `dry_run` records no PUT/DELETE.
- Contacts vCard 3.0 update preserves opaque fields; vCard 4.0 and groups remain
  read-only.
- A known successful PUT followed by failed normalization GET is successful with
  `resultIncomplete`, not ambiguous.
- Calendar retries reads only. No PUT, DELETE, or full-series delete is replayed;
  ambiguous dispatched mutations return `outcome_unknown`.

Mail:

- Search/get reads use read-only SELECT (EXAMINE) and PEEK.
- Every message reference includes UIDVALIDITY, and mismatch occurs before
  mutation.
- A CONDSTORE server cannot receive unconditional STORE when MODIFIED detection
  is unavailable. The beta.8 path returns `protocol_error` before STORE and does
  not claim `concurrent_modification`.
- Non-CONDSTORE flag writes are delta-only and cannot set Deleted or keywords.
- Move uses native UID MOVE or a UIDPLUS-only one-message fallback; plain
  EXPUNGE is impossible.
- Trash requires exactly one selectable SPECIAL-USE Trash target.
- All SMTP recipients pass the local policy before dial, all RCPT commands pass
  before DATA, Bcc is absent from message headers, each of `to`/`cc`/`bcc` is
  optional, and the aggregate recipient set is non-empty.
- No SMTP retry occurs, and a non-definitive post-DATA failure maps to
  `outcome_unknown`.

## CI gates

`.github/workflows/ci.yml` runs formatting, vet, pinned golangci-lint, race tests
with a 78% aggregate coverage threshold, `govulncheck`, module
verification/tidy checks, fuzz smoke for every target in all five packages,
multi-architecture builds, a 20 MiB binary budget, gitleaks, and security source
guards. Package coverage floors are:

| Package | Floor |
|---------|-------|
| `internal/config` | 85% |
| `internal/security` | 80% |
| `internal/icloud` | 78% |
| `internal/mcptools` | 75% |
| `internal/contacts` | 65% |
| `internal/mail` | 65% |
| `internal/mail/imapadapter` | 60% |

Live iCloud credentials and the `integration` build tag are never used in CI.

## Real iCloud integration

The checked-in build-tagged suite always exercises Calendar reads and local
Calendar calculations when credentials are valid. Contacts and Mail cases have
additional product-domain opt-ins. Contacts CRUD also has a test-only write
opt-in. The Mail mutation/send test has additional product write/send gates,
global read-only must be explicitly false, and a test-only self-recipient gate
must match an exact non-wildcard recipient policy.

These live tests are opt-in, credentialed, and never run in CI. A green unit
suite alone is not evidence of a live iCloud run.

### Calendar integration command

Use a dedicated account or disposable Calendar where possible:

```bash
export ICLOUD_EMAIL='you@icloud.com'
export ICLOUD_PASSWORD='xxxx-xxxx-xxxx-xxxx'
export ICLOUD_MCP_READ_ONLY='true'
export ICLOUD_MCP_DEFAULT_TZ='Europe/Paris'
go test -tags=integration -count=1 -v -timeout=120s .
```

`file://` values are also supported:

```bash
export ICLOUD_EMAIL='file:///run/secrets/icloud-email'
export ICLOUD_PASSWORD='file:///run/secrets/icloud-password'
go test -tags=integration -count=1 -v -timeout=120s .
```

Without valid credentials the integration test skips or fails at Calendar
discovery. Never weaken TLS or an allowlist to make a live test pass.

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

Contacts integration discovers/lists books, performs a bounded search, and gets
one existing contact when available. Mail integration lists selectable
mailboxes, searches up to four candidates, gets one message twice, and verifies
that Seen is unchanged. The Contacts enable flag is the live integration gate;
without it the Contacts tests skip.

### Explicit write opt-ins

`ICLOUD_MCP_INTEGRATION_WRITES` and
`ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT` are test-harness variables, not part of
the binary's 12-variable product contract. The harness accepts the write opt-in
only when its trimmed value equals `true`, case-insensitively.

Contacts CRUD requires all of:

```bash
export ICLOUD_MCP_ENABLE_CONTACTS='true'
export ICLOUD_MCP_READ_ONLY='false'
export ICLOUD_MCP_INTEGRATION_WRITES='true'
go test -tags=integration -run=TestIntegration_ContactsCreateUpdateDelete \
  -count=1 -v -timeout=5m .
```

The test selects a discovered vCard 3.0 writable book, creates one uniquely
identified contact with independent opaque random `FN`, structured `N`, `EMAIL`,
`TEL`, and `ORG` values. It checks `query` matching for `FN`, `N`, `EMAIL`, and
`ORG`, the exact email filter, a digits-only phone filter, and a UID read before
update. It defers deletion of only the generated UID. Logs contain fixed labels,
booleans, and counts, never fixture values or live contact data.

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

All five boolean values shown above must be explicitly set to the shown values.
The self-recipient must exactly equal the normalized configured Mail address and
must be allowed by an exact product SMTP policy; literal `*` is rejected. Before
SMTP, the test also requires a complete mailbox list and exactly one selectable
SPECIAL-USE Trash mailbox so cleanup has a safe target.

The test builds the full Mail service with the fixed security IMAP/SMTP dialers
and a separately parsed recipient policy. It submits one opaque plain-text
self-message, asserts the SMTP accepted and recipient outcome model, polls every
selectable mailbox with independent opaque subject/body queries, and verifies
UIDVALIDITY plus Seen preservation. It exercises flag add/remove when safe. On a
CONDSTORE server, the deliberate beta.8 `protocol_error` path is accepted only
after a read proves that no flag changed.

Move prefers a distinct selectable SPECIAL-USE Archive mailbox, then another
non-Trash destination, re-finds the fixture by opaque query, and moves it to
Trash. A missing optional destination or safe move capability skips only that
subtest after submission. Deferred cleanup searches every selectable mailbox
for remaining fixture copies and moves each one to Trash; it never uses permanent
delete or plain EXPUNGE. Logs contain fixed labels, booleans, and counts, not
addresses, subjects, message IDs, bodies, or mailbox names.

SMTP `accepted` confirms server acceptance, not final delivery. Polling and
cleanup are bounded, so a copy materialized only after the cleanup window may
remain. Successful cleanup intentionally leaves the disposable fixture in Trash
because the product exposes no permanent-delete operation.

Revoke the app-specific password after testing when it was created solely for
the run. Never commit credentials, raw DAV/vCard/MIME captures, mailbox content,
recipient lists, or live resource identifiers.
