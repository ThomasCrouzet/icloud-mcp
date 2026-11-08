# Real iCloud compatibility evidence matrix

This matrix separates evidence that runs without an account from evidence that
requires a live iCloud account.

**No live iCloud run is recorded for this revision.** Every live status in this
document is `UNVERIFIED`. Fixture and synthetic test passes are not proof of
Apple compatibility. A maintainer can change a status to `LIVE VERIFIED` only
after a recorded, privacy-safe run for the exact revision and account class,
using [the live run template](evidence/live-run-template.md).

## Evidence classes

| Class | Source | What it can prove | What it cannot prove |
|-------|--------|-------------------|----------------------|
| Synthetic and fixture | Go tests with in-memory DAV, IMAP, and SMTP peers | Local protocol handling, bounds, error mapping, and mutation safety | Current Apple server behavior |
| Static and policy | Configuration tests, CI guards, and source review | Endpoint allowlists, TLS policy, redaction, and capability gates | Any live account behavior |
| Live account | Root `integration_test.go` with build tag `integration` | One account, one date, and one server response set | Every account, region, or future server change |
| Native smoke | Release archive checks on GitHub-hosted runners | Startup and archive integrity | Protocol compatibility |

A `LIVE VERIFIED` row needs all of these:

- A recorded run from the same revision.
- The evidence template completed outside the repository.
- Sanitized artifacts that contain no addresses, resource identifiers, or
  message content.
- A cleanup result that shows the generated resources are gone, or a stated
  residual item such as a Trash copy.

## Calendar

The Calendar domain is always enabled. Read-only mode removes every Calendar
write tool. The checked-in live tests skip when their gates are absent.

| Capability | Synthetic coverage | Live status | Prerequisites and environment gates | Exact opt-in flags | Expected result | Cleanup and rollback |
|---|---|---|---|---|---|---|
| Discovery and calendar list | `internal/icloud/client_test.go`: `TestClient_Discover_HappyPath`, `TestClient_ListCalendars_FiltersTechnicalCalendars` | UNVERIFIED | Reachable fixed CalDAV hosts; one principal and one calendar home set | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true` | One validated principal and home set; calendar names and absolute paths; no mutation | Read-only. No resource cleanup. Revoke the test app-specific password. |
| Read event by UID | `internal/icloud/v2_client_test.go`: `TestClient_GetEvent_ByUID`; `internal/icloud/client_test.go`: `TestClient_UpdateEvent_UsesGETNotFilteredREPORTData` | UNVERIFIED | At least one existing event in the selected calendar; live test `TestIntegration_DiscoverListSearchGet` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true` | Returned UID matches the request; a strong ETag is present when the server supplies one | Read-only. No cleanup. |
| Search events and bounded result pages | `internal/icloud/client_test.go`: `TestClient_SearchEvents_TimeRangeTransmitted`; `internal/mcptools/search_events_test.go`: `TestSearchEventsHandler_HappyPath` | UNVERIFIED | At least one event in the search window; live test `TestIntegration_DiscoverListSearchGet` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true` | Sorted event page; recurrence occurrences; truncation or failure warnings; no silent partial page | Read-only. No cleanup. |
| Recurrence expansion | `internal/icloud/recurrence_test.go`: `TestExpandOccurrences_DailyWithCount`, `TestExpandOccurrences_WeeklyWithExdate`, `TestExpandOccurrences_OverrideReplacesOccurrence`, `TestExpandOccurrences_IncludesOccurrenceOverlappingRangeStart` | UNVERIFIED | A disposable calendar; live test `TestIntegration_CalendarCreateUpdateDeleteAndOccurrence` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_INTEGRATION_WRITES=true`, `ICLOUD_MCP_INTEGRATION_CALENDAR=<disposable path>` | A bounded series with preserved timezone; one occurrence update and one occurrence delete; the series rule remains | Deferred delete of generated UIDs only. An ambiguous delete is recorded and never replayed. Manual delete after reconciliation. |
| Create, update, and delete events | `internal/icloud/v2_client_test.go`: `TestClient_CreateEvent_EnrichedFields`, `TestClient_UpdateEvent_OccurrenceScope`, `TestClient_DeleteEvent_IfMatch412`; `internal/icloud/client_test.go`: `TestClient_CreateEvent`, `TestClient_UpdateEvent_ChangeAndClearFields`, `TestClient_DeleteEvent_ReturnsTitleAndDeletesRealPath` | UNVERIFIED | A disposable calendar and write gate; live test `TestIntegration_CalendarCreateUpdateDeleteAndOccurrence` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_INTEGRATION_WRITES=true`, `ICLOUD_MCP_INTEGRATION_CALENDAR=<disposable path>` | Timed, all-day, and recurring creates succeed; update and delete succeed; dry-run delete leaves the resource unchanged; deleted UID returns `not_found` | Deferred delete of generated UIDs only. If cleanup fails, keep the account, inspect the disposable calendar, and delete only recorded UIDs. |
| ETag and conditional writes | `internal/icloud/retry_etag_test.go`: `TestClient_UpdateEvent_ConditionalPUTSendsIfMatch`, `TestClient_UpdateEvent_FailsClosedWithoutETag`, `TestClient_DeleteEvent_FailsClosedWithoutETag`, `TestClient_UpdateEvent_Concurrent412PreconditionFailed` | UNVERIFIED | A disposable calendar and write gate; live test `TestIntegration_CalendarCreateUpdateDeleteAndOccurrence` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_INTEGRATION_WRITES=true`, `ICLOUD_MCP_INTEGRATION_CALENDAR=<disposable path>` | Strong ETag accepted; missing or weak ETag fails closed; stale ETag returns `concurrent_modification`; the first update remains | Deferred delete of generated UIDs only. Never retry a rejected conditional mutation. |
| DST transitions | `internal/icloud/recurrence_test.go`: `TestExpandOccurrences_PreservesTimezoneAcrossDST`, `TestExpandOccurrences_PreservesNominalDurationAcrossDST`; `internal/icloud/occurrence_duration_test.go`: `TestOccurrenceDurationPreservesNominalDaysAcrossDST`; `internal/icloud/freeslots_test.go`: `TestFindFreeSlots_ParisSpringForward_WorkingHoursWallClock`, `TestFindFreeSlots_ParisFallBack_CivilDayProgression` | UNVERIFIED | Europe/Paris series that crosses a DST change; live test `TestIntegration_CalendarCreateUpdateDeleteAndOccurrence` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_DEFAULT_TZ=Europe/Paris`, `ICLOUD_MCP_INTEGRATION_WRITES=true`, `ICLOUD_MCP_INTEGRATION_CALENDAR=<disposable path>` | Local wall time stays stable; offset changes at the transition; occurrence duration stays positive | Deferred delete of generated UIDs only. |
| Recurrent exceptions | `internal/icloud/client_test.go`: `TestClient_SearchEvents_RecurringWithExdateAndOverride`; `internal/icloud/v2_client_test.go`: `TestClient_DeleteEvent_OccurrenceAddsEXDATE`; `internal/icloud/occurrence_write_test.go`: `TestExpandOccurrences_ExposesRecurrenceIDClearsRRULE`; `internal/icloud/calendar_correctness_test.go`: `TestCalendarObjectRejectsAmbiguousOverrideIdentity` | UNVERIFIED | A disposable recurring series; live test `TestIntegration_CalendarCreateUpdateDeleteAndOccurrence` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_INTEGRATION_WRITES=true`, `ICLOUD_MCP_INTEGRATION_CALENDAR=<disposable path>` | One override is returned with its RECURRENCE-ID; one deleted occurrence adds EXDATE; the series rule remains | Deferred delete of the generated series UID only. An ambiguous occurrence delete is not replayed. |
| Timeouts and cancellation | `internal/icloud/calendar_correctness_test.go`: `TestCancellationAndBackoffReturnTypedTimeouts`; `internal/icloud/calendar_protocol_test.go`: `TestRateLimiterContextCancellationIsTimeout`; `internal/icloud/recurrence_test.go`: `TestExpandOccurrencesContext_Cancellation` | UNVERIFIED | A controlled network fault or cancellation point; no checked-in live timeout scenario is safe to force against Apple | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true` | Typed timeout error; no partial success claim; no read replay beyond the documented bound | No mutation. No cleanup. |
| Ambiguous mutation outcomes | `internal/icloud/calendar_protocol_test.go`: `TestProductionRetryWrapperMutationTransportIsOutcomeUnknown`, `TestCalendarMutationNilResponseIsOutcomeUnknown`; `internal/icloud/security_pass2_test.go`: `TestCalendarMutationInternalServerErrorIsOutcomeUnknown`, `TestCalendarDAVAutomaticallyFollowedMutationIsOutcomeUnknown` | UNVERIFIED | A post-dispatch transport fault. This is not reproducible against live Apple without a controlled proxy | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_INTEGRATION_WRITES=true`, `ICLOUD_MCP_INTEGRATION_CALENDAR=<disposable path>` | `outcome_unknown` with bounded reconciliation; the mutation is never replayed | Read the resource by UID before any new mutation. Delete only after reconciliation. |
| Large payload and result limits | `internal/icloud/report_limits_test.go`: `TestReportMultiStatusStreamingLimits`; `internal/icloud/security_pass2_test.go`: `TestCalendarGETBodyIsBoundedBeforeICalDecode`, `TestRemoteEventFieldLimits`; `internal/icloud/robustness_pass4_test.go`: `TestRuntimeLimitsCountUTF8Bytes` | UNVERIFIED | An in-memory oversized DAV peer; live Apple cannot be instructed to return an oversized body on demand | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true` | `payload_too_large` or `protocol_error` before full materialization; bounded error text | Read-only. No cleanup. |

## Contacts

Contacts requires `ICLOUD_MCP_ENABLE_CONTACTS=true`. There is no separate
Contacts write flag. Writes also require global read-only to be false.

| Capability | Synthetic coverage | Live status | Prerequisites and environment gates | Exact opt-in flags | Expected result | Cleanup and rollback |
|---|---|---|---|---|---|---|
| Discovery and address book list | `internal/contacts/contacts_test.go`: `TestDiscoveryPreservesAuthoritiesAndValidatesShardCallback`, `TestDiscoveryFailureIsNotCached` | UNVERIFIED | Reachable fixed CardDAV hosts; live test `TestIntegration_ContactsDiscoverListSearchGet` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_CONTACTS=true` | One or more validated address books with opaque identifiers; a failed discovery is not cached | Read-only. No cleanup. |
| Search and local re-filter | `internal/contacts/contacts_test.go`: `TestSearchFiltersGroupsAndSorts`, `TestSearchQueryUsesEscapedServerSideFiltersAndBoundedFullData`, `TestSearchScanAndResultCaps` | UNVERIFIED | At least one contact; live tests `TestIntegration_ContactsDiscoverListSearchGet` and `TestIntegration_ContactsCreateUpdateDelete` | Read: `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_CONTACTS=true`. Write probe: add `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_INTEGRATION_WRITES=true` | Server prefilter plus local re-filter; bounded scan; query, email, and digit-normalized phone matches | Read-only, or delete only the generated contact UID. |
| vCard 3.0 and 4.0 read | `internal/contacts/contacts_test.go`: `TestV3CreateUpdateDeleteAndOpaquePreservation`, `TestV4ReadAndUpdateRejected`, `TestV4WithoutNReadsInSearchAndGet`, `TestVCardVersionPropertyRules` | UNVERIFIED | An existing contact. The live read test does not assert the stored vCard version | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_CONTACTS=true` | Version 3.0 and 4.0 read; missing N accepted only for 4.0; PHOTO bytes omitted | Read-only. No cleanup. |
| vCard 3.0 write | `internal/contacts/contacts_test.go`: `TestV3CreateUpdateDeleteAndOpaquePreservation`, `TestStrongETagValidationAnd412Mappings`, `TestDeleteDryRunRequiresServerStrongETag` | UNVERIFIED | A discovered writable vCard 3.0 book; live test `TestIntegration_ContactsCreateUpdateDelete` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_ENABLE_CONTACTS=true`, `ICLOUD_MCP_INTEGRATION_WRITES=true` | Create with unique fields; search by FN, N, EMAIL, ORG, exact email, and normalized phone; update by ETag; delete; opaque fields preserved | Deferred delete of the generated UID only. An ambiguous delete is not replayed. Inspect the book and delete only that UID. |
| Groups read-only | `internal/contacts/contacts_test.go`: `TestSearchFiltersGroupsAndSorts`; `internal/mcptools/contacts_tools_test.go`: `TestContactsRegisterCountsSchemasAndAnnotations` | UNVERIFIED | Existing groups; live read test sets `IncludeGroups: true` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_CONTACTS=true` | Group summaries are readable; no group write tool is registered | Read-only. No cleanup. |
| Card and field limits | `internal/contacts/contacts_test.go`: `TestFieldAndCardCaps`, `TestStrictVCardFramingAndLogicalLines`; `internal/contacts/fuzz_test.go`: `TestMultiStatusStreamingLimits` | UNVERIFIED | An in-memory oversized CardDAV peer; live Apple cannot be instructed to return oversized data on demand | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_CONTACTS=true` | Bounded card, field, XML, and list limits; typed refusal without remote content | Read-only. No cleanup. |

## Mail

Mail reads require `ICLOUD_MCP_ENABLE_MAIL=true`. Mutation and send have
separate product gates. The destructive live test sends first, so it requires
all three Mail gates and the test-only write and self-recipient gates.

| Capability | Synthetic coverage | Live status | Prerequisites and environment gates | Exact opt-in flags | Expected result | Cleanup and rollback |
|---|---|---|---|---|---|---|
| IMAP discovery and mailbox list | `internal/mail/service_test.go`: `TestListMailboxesCapsObjects`; `internal/mail/imapadapter/fuzz_test.go`: `TestAdapterListSelectAndSearchProtocol` | UNVERIFIED | Reachable `imap.mail.me.com:993`; live test `TestIntegration_MailListSearchGetMetadata` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_MAIL=true`, `ICLOUD_MAIL_ADDRESS`, `ICLOUD_MAIL_PASSWORD` | Mailbox list with SPECIAL-USE attributes; bounded object count; truncation flag | Read-only. No cleanup. |
| Read one message | `internal/mail/service_test.go`: `TestGetMessageUsesExaminePeekAndOmitsHTML`, `TestGetMessageUIDValidityAndBodyLimits` | UNVERIFIED | At least one message in a selectable mailbox; live test `TestIntegration_MailListSearchGetMetadata` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_MAIL=true`, `ICLOUD_MAIL_ADDRESS`, `ICLOUD_MAIL_PASSWORD` | Metadata and bounded plain text; attachment metadata without bytes; EXAMINE and PEEK leave Seen unchanged | Read-only. No cleanup. |
| Search messages | `internal/mail/service_test.go`: `TestSearchUIDWindowsAndIdentity`, `TestSearchUsesTypedCriteriaAndReturnsNewestUIDFirst`, `TestSearchCountTruncationDoesNotSkipOrDuplicate` | UNVERIFIED | A selectable mailbox; live test `TestIntegration_MailListSearchGetMetadata` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_MAIL=true`, `ICLOUD_MAIL_ADDRESS`, `ICLOUD_MAIL_PASSWORD` | Bounded UID-window search; newest UID first; nonzero UIDVALIDITY; no duplicate or skipped result | Read-only. No cleanup. |
| UIDVALIDITY identity | `internal/mail/service_test.go`: `TestSearchUIDWindowsAndIdentity`, `TestGetMessageUIDValidityAndBodyLimits`; `internal/mcptools/contract_preconditions_test.go`: `TestContractMailIdentityPreconditions` | UNVERIFIED | A selectable mailbox; live test `TestIntegration_MailListSearchGetMetadata` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_MAIL=true`, `ICLOUD_MAIL_ADDRESS`, `ICLOUD_MAIL_PASSWORD` | `(mailbox, UIDVALIDITY, UID)` identity is stable; a mismatch fails before mutation | Read-only. No cleanup. |
| Flag, move, and trash mutations | `internal/mail/service_test.go`: `TestSetFlagsCapabilityGateAndDeltaFallback`, `TestMoveCapabilityPathsAndPartialSafety`, `TestMoveRejectsSpecialUseDeletionTargetsBeforeDispatch`, `TestTrashRequiresExactlyOneSpecialUseMailbox` | UNVERIFIED | One selectable SPECIAL-USE Trash; a distinct safe move destination; live test `TestIntegration_MailMutationAndSend` | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=false`, `ICLOUD_MCP_ENABLE_MAIL=true`, `ICLOUD_MCP_ENABLE_MAIL_WRITE=true`, `ICLOUD_MCP_ENABLE_MAIL_SEND=true`, `ICLOUD_MAIL_ADDRESS`, `ICLOUD_MAIL_PASSWORD`, `ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS=<exact self address>`, `ICLOUD_MCP_INTEGRATION_WRITES=true`, `ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT=<exact self address>` | Flag add and remove; Seen preserved; move by MOVE or UIDPLUS fallback; trash to the single Trash mailbox; a CONDSTORE fail-closed result is accepted only after flags are re-read unchanged | Deferred search moves every fixture copy to Trash. No permanent delete. A copy outside the cleanup window can remain in Trash. Rollback is another trash move, never permanent delete. |
| SMTP self-send | `internal/mail/service_test.go`: `TestSMTPOrderMIMEAndAllRecipientPolicy`, `TestSMTPRejectsBeforeDataAndContinuesRCPT`, `TestSMTPAmbiguousDataOutcome`, `TestSendLimits` | UNVERIFIED | Reachable `smtp.mail.me.com:587`; one exact self recipient; live test `TestIntegration_MailMutationAndSend` | Same flags as the mutation row. The policy must contain the exact self address and must not contain `*` | SMTP accepted; recipient outcome model; no Sent copy claim; local Message-ID present | Poll each selectable mailbox and move the fixture to Trash. SMTP accepted does not prove delivery. Never resend after an ambiguous result. |
| Optional Sent APPEND | `internal/mail/service_test.go`: `TestSendArchiveSentCopyHasSeparateOutcome`, `TestArchiveSentRequiresExplicitCapability`, `TestArchiveSentRequiresUniqueSpecialUseMailbox`, `TestArchiveSentHonorsResourceScope`; `internal/mail/archive_protocol_test.go`: `TestProtocolArchiveNoAppendWithoutAcceptance` | UNVERIFIED | Mail send plus `mail.archive` permission; exactly one selectable SPECIAL-USE Sent mailbox. The integration suite does not exercise archival and asserts `SentCopyUnavailable` | No live opt-in flag exists. A tool call must set `archive_sent: true` under a permission profile that includes `mail.archive` | Separate `archivalStatus`: `stored`, `failed`, or `unknown`; SMTP status stays `accepted`; no SMTP or APPEND replay | Inspect the authorized Sent mailbox. Move a test copy to Trash if present. Never resend. Report an unknown APPEND as unknown. |
| Mail body and message limits | `internal/mail/service_test.go`: `TestGetMessageUIDValidityAndBodyLimits`; `internal/mail/smtp_test.go`: `TestSMTPInboundBudgetRejectsOversizedGreetingWithoutWaitingForDeadline`; `internal/mcptools/contract_byte_bounds_test.go`: `TestContractEncodedMailResourceLimit` | UNVERIFIED | An in-memory oversized peer; live Apple cannot be instructed to exceed the declared limits on demand | `ICLOUD_EMAIL`, `ICLOUD_PASSWORD`, `ICLOUD_MCP_READ_ONLY=true`, `ICLOUD_MCP_ENABLE_MAIL=true`, `ICLOUD_MAIL_ADDRESS`, `ICLOUD_MAIL_PASSWORD` | `body_too_large`, `payload_too_large`, or a bounded SMTP refusal before full materialization | Read-only. No cleanup. |

## Recording a live run

The harness validates local gates without network access:

```bash
python3 scripts/live_evidence.py --check
```

A read-only run writes sanitized evidence outside the repository:

```bash
python3 scripts/live_evidence.py --confirm-live \
  --output /absolute/private/evidence/icloud-live-read-1
```

A destructive run also requires `ICLOUD_MCP_INTEGRATION_WRITES=true` and an
explicit disposable Calendar selector in the environment:

```bash
python3 scripts/live_evidence.py --confirm-live --destructive \
  --output /absolute/private/evidence/icloud-live-write-1
```

A passing harness run does not change this matrix. A maintainer must review the
artifact, complete the template, and update only the rows that the run actually
covered. Keep raw transcripts, addresses, and resource identifiers outside the
repository.
