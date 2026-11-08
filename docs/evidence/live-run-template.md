# Live iCloud evidence record template

Copy this template to a new file outside the repository. Fill every field from
one recorded run. Do not put raw addresses, account identifiers, resource paths,
message identifiers, tokens, or remote content in this file.

**Privacy rule:** the repository holds the template and the scheme, not the
private values. Keep the exact account data, calendar path, address book
identifier, mailbox names, fixture identities, and raw logs in local private
storage. Do not upload them to CI or attach them to a public issue.

## 1. Record identity

| Field | Value |
|-------|-------|
| Evidence record ID | `<date-revision-short-random>` |
| Recorded at (UTC) | `<YYYY-MM-DDThh:mm:ssZ>` |
| Recorded by | `<maintainer role>` |
| Git revision | `<full commit SHA>` |
| Git describe | `<tag-or-describe-output>` |
| Working tree | `<clean or list of tracked changes>` |
| Binary or release version | `<release tag, or go test temporary test binary>` |
| Build command | `<make build, go test -tags=integration, or release command>` |
| Go version | `<go version output>` |
| Python version | `<python3 --version output>` |

## 2. Environment

| Field | Value |
|-------|-------|
| OS | `<Linux, macOS, or other>` |
| OS release | `<release>` |
| Architecture | `<amd64 or arm64>` |
| Network class | `<home, office, or hosted; no hostname>` |
| Time zone | `<IANA zone used by the run>` |
| Account class | `<dedicated test account: yes or no>` |
| Account password type | `<app-specific password>` |
| Optional domains | `<Contacts on/off, Mail on/off>` |

## 3. Gate flags

Record names and boolean values only. Record whether a selector was set, not
the selector value.

| Gate | Value |
|------|-------|
| `ICLOUD_MCP_READ_ONLY` | `<true or false>` |
| `ICLOUD_MCP_ENABLE_CONTACTS` | `<true or false>` |
| `ICLOUD_MCP_ENABLE_MAIL` | `<true or false>` |
| `ICLOUD_MCP_ENABLE_MAIL_WRITE` | `<true or false>` |
| `ICLOUD_MCP_ENABLE_MAIL_SEND` | `<true or false>` |
| `ICLOUD_MCP_INTEGRATION_WRITES` | `<true or false>` |
| `ICLOUD_MCP_INTEGRATION_CALENDAR` set | `<yes or no>` |
| `ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT` set | `<yes or no>` |
| SMTP recipient policy exact | `<yes; wildcard is never valid>` |
| Credentials from `file://` | `<yes or no>` |

## 4. Commands run

Use placeholders for private paths and values. The actual commands and their
sanitized output stay in the private evidence directory.

```bash
python3 scripts/live_evidence.py --check
python3 scripts/live_evidence.py --confirm-live \
  --output <absolute-private-directory-outside-repository>
python3 scripts/live_evidence.py --confirm-live --destructive \
  --output <absolute-private-directory-outside-repository>
go test -tags=integration -count=1 -v -timeout=<seconds> .
```

| Command | Exit status | Artifact |
|---------|-------------|----------|
| `<command>` | `<status>` | `<private artifact name>` |

## 5. Apple protocol observations

Record observed behavior classes only. Do not paste raw DAV, vCard, IMAP, MIME,
SMTP, or transcript content.

| Area | Observation | Result |
|------|-------------|--------|
| CalDAV discovery | `<principal and home set count; shard redirect class>` | `<pass, fail, or not covered>` |
| Calendar REPORT | `<bare calendar-data accepted; time-range class>` | `<pass, fail, or not covered>` |
| Calendar ETag | `<strong ETag returned; stale ETag rejected>` | `<pass, fail, or not covered>` |
| Calendar recurrence | `<TZID preserved; override and EXDATE behavior>` | `<pass, fail, or not covered>` |
| CardDAV discovery | `<home set and book count class>` | `<pass, fail, or not covered>` |
| Contact read | `<vCard version class; PHOTO omitted>` | `<pass, fail, or not covered>` |
| Contact write | `<vCard 3.0 accepted; ETag accepted>` | `<pass, fail, or not covered>` |
| IMAP capabilities | `<MOVE, UIDPLUS, CONDSTORE, SPECIAL-USE presence>` | `<pass, fail, or not covered>` |
| IMAP identity | `<UIDVALIDITY present; Seen preserved>` | `<pass, fail, or not covered>` |
| SMTP submission | `<STARTTLS, AUTH, accepted status>` | `<pass, fail, or not covered>` |
| Sent APPEND | `<stored, failed, unknown, or not covered>` | `<pass, fail, or not covered>` |

## 6. Disposable resource naming scheme

State the scheme, not the private values.

| Resource | Scheme | Count created |
|----------|--------|---------------|
| Calendar events | `icloud-mcp-<revision7>-<utcstamp>-<random6>` in the disposable calendar | `<count>` |
| Contacts | `<scheme>` in one writable vCard 3.0 book | `<count>` |
| Mail fixture | `<scheme>` in the subject and body token | `<count>` |
| Calendar selector | One disposable calendar path, recorded privately | `<count>` |

The run must create only these resources. It must not modify unrelated events,
contacts, messages, mailboxes, or account settings.

## 7. Scenario outcomes

Copy the relevant matrix rows and record the observed result. Use the harness
JSON and Markdown artifact as the source of pass, fail, and skip counts.

| Matrix row | Result | Covered by test | Notes |
|------------|--------|-----------------|-------|
| `<capability>` | `<pass, fail, or skip>` | `<Go test name>` | `<bounded observation>` |

Summary:

| Counter | Value |
|---------|-------|
| Passed | `<count>` |
| Failed | `<count>` |
| Skipped | `<count>` |
| Timed out | `<true or false>` |
| Overall | `<PASS or FAIL>` |

## 8. Cleanup proof

| Check | Evidence | Result |
|-------|----------|--------|
| Calendar fixtures absent | Read each generated UID and observed `not_found` | `<yes or no>` |
| Contact fixture absent | Read the generated UID and observed `not_found` | `<yes or no>` |
| Mail fixture copies found | Search each selectable mailbox for the fixture token | `<count>` |
| Mail copies moved to Trash | Move result for each copy | `<count>` |
| Residual Trash copies | Count at the end of the cleanup window | `<count>` |
| Permanent delete used | The product has no permanent delete | `<no>` |
| App-specific password revoked | `<yes, no, or not created for this run>` | `<value>` |

A successful cleanup can leave a fixture copy in Trash. State that residual
copy. Do not claim that Trash is empty.

## 9. Redaction checklist

- [ ] No email address, account name, or display name.
- [ ] No app-specific password, password file path, or Authorization value.
- [ ] No Basic, SASL, Bearer, or other token value.
- [ ] No calendar path, address book identifier, mailbox name, or resource UID.
- [ ] No Message-ID, In-Reply-To, References, subject, body, or attachment name.
- [ ] No raw DAV, vCard, IMAP, MIME, SMTP, or stdio transcript.
- [ ] No host name, IP address, or private network detail.
- [ ] The private artifact directory is outside the repository.
- [ ] The harness sanitized the stored log before writing it.
- [ ] A second reviewer compared the record with the redaction list.

## 10. Limitations and sign-off

State what the run does not prove:

- One account does not represent every account, region, or shard.
- One date does not represent future Apple server changes.
- A synthetic or fixture pass is not Apple compatibility proof.
- SMTP accepted does not prove final delivery.
- Skipped tests are not live evidence.
- The run does not cover an unsupported protocol form.

| Field | Value |
|-------|-------|
| Reviewer | `<maintainer role>` |
| Review date | `<YYYY-MM-DD>` |
| Matrix rows approved for `LIVE VERIFIED` | `<row names or none>` |
| Remaining `UNVERIFIED` rows | `<row names or all>` |
| Artifact location class | `<private local storage outside the repository>` |
