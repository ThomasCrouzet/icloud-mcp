package mcptools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

// paginationExpectedRows returns the canonical total order the fixture must
// produce: normalized display name, UID, then opaque address-book identifier.
func paginationExpectedRows(t *testing.T, client *contacts.Client) []contacts.ContactSummary {
	t.Helper()
	books, err := client.ListAddressBooks(context.Background())
	if err != nil {
		t.Fatalf("ListAddressBooks() error = %v", err)
	}
	if len(books) != paginationFixtureBooks {
		t.Fatalf("address books = %d, want %d", len(books), paginationFixtureBooks)
	}
	var rows []contacts.ContactSummary
	for index := range paginationFixturePerBook {
		for book := range paginationFixtureBooks {
			name := paginationDisplayName(book, index)
			rows = append(rows, contacts.ContactSummary{
				AddressBook: books[book].Identifier,
				UID:         fmt.Sprintf("u-%04d", index),
				DisplayName: strings.TrimSpace(name),
			})
		}
	}
	sortPaginationRows(rows)
	return rows
}

func sortPaginationRows(rows []contacts.ContactSummary) {
	// Insertion sort keeps the helper dependency-free and obviously correct.
	for index := 1; index < len(rows); index++ {
		for cursor := index; cursor > 0 && paginationRowLess(rows[cursor], rows[cursor-1]); cursor-- {
			rows[cursor], rows[cursor-1] = rows[cursor-1], rows[cursor]
		}
	}
}

func paginationRowLess(left, right contacts.ContactSummary) bool {
	name, other := contacts.NormalizeSortName(left.DisplayName), contacts.NormalizeSortName(right.DisplayName)
	if name != other {
		return name < other
	}
	if left.UID != right.UID {
		return left.UID < right.UID
	}
	return left.AddressBook < right.AddressBook
}

// TestContactsPaginationSnapshotAvoidsPerPageRescan proves that one cursor walk
// does not repeat a full multi-book REPORT for every page. Before the snapshot
// design the walk issued one REPORT per book per page.
func TestContactsPaginationSnapshotAvoidsPerPageRescan(t *testing.T) {
	const limit = 100
	counters := &paginationCounters{}
	client, _ := newPaginationClient(counters)
	expected := paginationExpectedRows(t, client)
	deps := paginationDeps(t, client, newContactsCursorCodec())
	counters.reports.Store(0)
	counters.reportBytes.Store(0)

	rows, pages := walkPaginationPages(t, deps, nil, limit)
	if len(rows) != len(expected) {
		t.Fatalf("rows = %d, want %d", len(rows), len(expected))
	}
	if want := (len(expected) + limit - 1) / limit; pages != want {
		t.Fatalf("pages = %d, want %d", pages, want)
	}
	for index := range rows {
		if paginationRowKey(rows[index]) != paginationRowKey(expected[index]) {
			t.Fatalf("row %d = %q, want %q", index, paginationRowKey(rows[index]), paginationRowKey(expected[index]))
		}
	}
	reports, reportBytes := counters.snapshot()
	if want := int64(paginationFixtureBooks); reports != want {
		t.Fatalf("one walk issued %d REPORTs, want %d (one per book)", reports, want)
	}
	t.Logf("walk_seed=%s pages=%d reports=%d report_bytes=%d rows=%d",
		paginationFixtureSeed, pages, reports, reportBytes, len(rows))
}

// TestContactsPaginationSnapshotKeepsRowsDistinct covers equal normalized names
// and identical UIDs in distinct books across page boundaries.
func TestContactsPaginationSnapshotKeepsRowsDistinct(t *testing.T) {
	counters := &paginationCounters{}
	client, _ := newPaginationClient(counters)
	expected := paginationExpectedRows(t, client)
	deps := paginationDeps(t, client, newContactsCursorCodec())

	rows, _ := walkPaginationPages(t, deps, nil, 37)
	seen := make(map[string]int, len(rows))
	perUID := make(map[string]int, len(rows))
	for _, summary := range rows {
		seen[paginationRowKey(summary)]++
		perUID[summary.UID]++
	}
	for index := range paginationFixturePerBook {
		uid := fmt.Sprintf("u-%04d", index)
		if perUID[uid] != paginationFixtureBooks {
			t.Fatalf("uid %q observed %d times, want %d", uid, perUID[uid], paginationFixtureBooks)
		}
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("row %q observed %d times across pages", key, count)
		}
	}
	if len(rows) != len(expected) {
		t.Fatalf("rows = %d, want %d", len(rows), len(expected))
	}
	for index := range rows {
		if paginationRowKey(rows[index]) != paginationRowKey(expected[index]) {
			t.Fatalf("row %d = %q, want %q", index, paginationRowKey(rows[index]), paginationRowKey(expected[index]))
		}
	}
}

// TestContactsPaginationSnapshotIgnoresLaterEdits defines the edit semantics:
// an open walk keeps the rows of its snapshot, and a fresh walk observes the
// edit.
func TestContactsPaginationSnapshotIgnoresLaterEdits(t *testing.T) {
	const limit = 100
	const renamed = "Aaa Renamed"
	counters := &paginationCounters{}
	client, doer := newPaginationClient(counters)
	expected := paginationExpectedRows(t, client)
	deps := paginationDeps(t, client, newContactsCursorCodec())
	handler := searchContactsHandler(deps)

	first, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": limit}))
	if err != nil || first.IsError {
		t.Fatalf("first page: %v %s", err, resultText(t, first))
	}
	var firstPage contacts.SearchResult
	decodeResult(t, first, &firstPage)
	if firstPage.NextCursor == "" {
		t.Fatal("first page returned no cursor")
	}
	if len(firstPage.Contacts) != limit {
		t.Fatalf("first page rows = %d, want %d", len(firstPage.Contacts), limit)
	}

	// Rename the last fixture row so it sorts first after the edit.
	doer.renameCard(0, paginationFixturePerBook-1, renamed)

	// A fresh walk must observe the edit.
	freshRows, _ := walkPaginationPages(t, deps, nil, limit)
	if len(freshRows) == 0 || freshRows[0].DisplayName != renamed {
		t.Fatalf("a fresh walk did not observe the edit: %q", freshRows[0].DisplayName)
	}

	// Resume the first walk from its token and follow the whole chain. The
	// snapshot rows must still hold, so the walk returns the pre-edit set.
	resumedRows := append([]contacts.ContactSummary(nil), firstPage.Contacts...)
	cursor := firstPage.NextCursor
	for pages := 1; ; pages++ {
		if pages > 20 {
			t.Fatal("resumed walk did not terminate")
		}
		resumed, callErr := handler(context.Background(), newReq(map[string]any{"cursor": cursor, "limit": limit}))
		if callErr != nil || resumed.IsError {
			t.Fatalf("resumed page %d: %v %s", pages, callErr, resultText(t, resumed))
		}
		var resumedPage contacts.SearchResult
		decodeResult(t, resumed, &resumedPage)
		for _, summary := range resumedPage.Contacts {
			if summary.DisplayName == renamed {
				t.Fatal("an open walk observed an edit made after its snapshot")
			}
		}
		resumedRows = append(resumedRows, resumedPage.Contacts...)
		if resumedPage.NextCursor == "" {
			break
		}
		cursor = resumedPage.NextCursor
	}
	if len(resumedRows) != len(expected) {
		t.Fatalf("resumed walk rows = %d, want %d", len(resumedRows), len(expected))
	}
	for index := range resumedRows {
		if paginationRowKey(resumedRows[index]) != paginationRowKey(expected[index]) {
			t.Fatalf("resumed row %d = %q, want the pre-edit row %q",
				index, paginationRowKey(resumedRows[index]), paginationRowKey(expected[index]))
		}
	}
}

// TestContactsPaginationSnapshotExpiryFailsClosed covers an expired snapshot.
func TestContactsPaginationSnapshotExpiryFailsClosed(t *testing.T) {
	counters := &paginationCounters{}
	client, _ := newPaginationClient(counters)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	codec := newContactsCursorCodec()
	codec.now = func() time.Time { return now }
	deps := paginationDeps(t, client, codec)
	handler := searchContactsHandler(deps)

	first, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 100}))
	if err != nil || first.IsError {
		t.Fatalf("first page: %v %s", err, resultText(t, first))
	}
	var page contacts.SearchResult
	decodeResult(t, first, &page)
	if page.NextCursor == "" {
		t.Fatal("first page returned no cursor")
	}

	beforeReports, beforeBytes := counters.snapshot()
	now = now.Add(contactsCursorTTL + time.Second)
	stale, err := handler(context.Background(), newReq(map[string]any{"cursor": page.NextCursor, "limit": 100}))
	if err != nil {
		t.Fatal(err)
	}
	if !stale.IsError {
		t.Fatalf("expired cursor was accepted: %s", resultText(t, stale))
	}
	payload := contactsErrorPayloadFromResult(t, stale)
	if payload.Code != string(contacts.CodeValidation) {
		t.Fatalf("expired cursor error code = %q", payload.Code)
	}
	afterReports, afterBytes := counters.snapshot()
	if beforeReports != afterReports || beforeBytes != afterBytes {
		t.Fatalf("stale cursor reached the backend: reports %d -> %d, bytes %d -> %d",
			beforeReports, afterReports, beforeBytes, afterBytes)
	}
}

// TestContactsPaginationSnapshotEvictionFailsClosed covers cache eviction. A
// token whose snapshot left the bounded cache must fail, never rebase onto a
// newer snapshot with the same filters.
func TestContactsPaginationSnapshotEvictionFailsClosed(t *testing.T) {
	// A controllable clock makes the eviction order defined: the oldest
	// snapshot leaves first.
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	service := newPaginationFakeService()
	codec := newContactsCursorCodec()
	codec.now = func() time.Time { return now }
	deps := paginationDeps(t, service, codec)
	handler := searchContactsHandler(deps)

	first, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 10}))
	if err != nil || first.IsError {
		t.Fatalf("first page: %v %s", err, resultText(t, first))
	}
	var page contacts.SearchResult
	decodeResult(t, first, &page)
	oldToken := page.NextCursor
	if oldToken == "" {
		t.Fatal("no cursor")
	}

	// Open more walks than the snapshot cap allows.
	for _, query := range []string{"Person 0001", "Person 0002", "Person 0003", "Person 0004", "Person 0005"} {
		now = now.Add(time.Second)
		res, callErr := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 10, "query": query}))
		if callErr != nil || res.IsError {
			t.Fatalf("walk %q: %v %s", query, callErr, resultText(t, res))
		}
	}
	if got := codec.snapshotCount(); got > maxContactsSnapshots {
		t.Fatalf("retained snapshots = %d, want at most %d", got, maxContactsSnapshots)
	}

	searchesBefore := service.searchCount()
	stale, err := handler(context.Background(), newReq(map[string]any{"cursor": oldToken, "limit": 10}))
	if err != nil {
		t.Fatal(err)
	}
	if !stale.IsError {
		t.Fatalf("evicted cursor was accepted: %s", resultText(t, stale))
	}
	payload := contactsErrorPayloadFromResult(t, stale)
	if payload.Code != string(contacts.CodeValidation) {
		t.Fatalf("evicted cursor error code = %q", payload.Code)
	}
	if got := service.searchCount(); got != searchesBefore {
		t.Fatalf("evicted cursor reached the backend: searches %d -> %d", searchesBefore, got)
	}
}

// TestContactsPaginationSnapshotMemoryIsBounded checks both retention caps on
// many concurrent walks over one codec.
func TestContactsPaginationSnapshotMemoryIsBounded(t *testing.T) {
	service := newPaginationFakeService()
	codec := newContactsCursorCodec()
	deps := paginationDeps(t, service, codec)
	handler := searchContactsHandler(deps)

	for index := range maxContactsSnapshots + 4 {
		query := fmt.Sprintf("Person %04d", index)
		res, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 10, "query": query}))
		if err != nil || res.IsError {
			t.Fatalf("walk %q: %v %s", query, err, resultText(t, res))
		}
		var page contacts.SearchResult
		decodeResult(t, res, &page)
		if page.NextCursor == "" {
			t.Fatalf("walk %q returned no cursor", query)
		}
	}
	if got := codec.snapshotCount(); got > maxContactsSnapshots {
		t.Fatalf("retained snapshots = %d, want at most %d", got, maxContactsSnapshots)
	}
	if got := codec.snapshotBytes(); got > maxContactsSnapshotBytes {
		t.Fatalf("retained snapshot bytes = %d, want at most %d", got, maxContactsSnapshotBytes)
	}
	if got := codec.entryCount(); got > maxContactsCursorEntries {
		t.Fatalf("retained cursor entries = %d, want at most %d", got, maxContactsCursorEntries)
	}
	if codec.snapshotBytes() <= 0 {
		t.Fatal("snapshot accounting reported no retained bytes")
	}
	t.Logf("retention snapshots=%d snapshot_bytes=%d entries=%d",
		codec.snapshotCount(), codec.snapshotBytes(), codec.entryCount())
}

// TestContactsPaginationSnapshotRejectsUnorderedRows proves the codec refuses a
// result set whose order would skip or repeat rows at a page boundary.
func TestContactsPaginationSnapshotRejectsUnorderedRows(t *testing.T) {
	counters := &paginationCounters{}
	client, _ := newPaginationClient(counters)
	expected := paginationExpectedRows(t, client)
	reversed := make([]contacts.ContactSummary, len(expected))
	for index := range expected {
		reversed[index] = expected[len(expected)-1-index]
	}
	codec := newContactsCursorCodec()
	if _, _, ok := codec.storeSnapshot(expected); !ok {
		t.Fatal("the canonical order was refused")
	}
	if _, _, ok := codec.storeSnapshot(reversed); ok {
		t.Fatal("an unordered result set was retained")
	}
	if _, _, ok := codec.storeSnapshot(nil); ok {
		t.Fatal("an empty result set was retained")
	}
}

// TestContactsPaginationConcurrentSessionsStayIsolated covers concurrent walks
// with different filters over the same codec.
func TestContactsPaginationConcurrentSessionsStayIsolated(t *testing.T) {
	service := newPaginationFakeService()
	codec := newContactsCursorCodec()
	deps := paginationDeps(t, service, codec)

	queries := []string{"Person 0001", "Person 0002", "Person 0003", "Person 0004"}
	results := make([][]contacts.ContactSummary, len(queries))
	pagesSeen := make([]int, len(queries))
	errs := make([]error, len(queries))
	var wait sync.WaitGroup
	for index, query := range queries {
		wait.Add(1)
		go func(index int, query string) {
			defer wait.Done()
			handler := searchContactsHandler(deps)
			cursor := "start"
			for {
				res, err := handler(context.Background(), newReq(map[string]any{"cursor": cursor, "limit": 50, "query": query}))
				if err != nil {
					errs[index] = err
					return
				}
				if res.IsError {
					errs[index] = fmt.Errorf("page %d: %s", pagesSeen[index], resultText(t, res))
					return
				}
				var page contacts.SearchResult
				decodeResult(t, res, &page)
				results[index] = append(results[index], page.Contacts...)
				pagesSeen[index]++
				if page.NextCursor == "" {
					return
				}
				cursor = page.NextCursor
				if pagesSeen[index] > 100 {
					errs[index] = fmt.Errorf("pagination did not terminate")
					return
				}
			}
		}(index, query)
	}
	wait.Wait()

	// The fake ignores the query text, so every session must see the same
	// complete result set with no row repeated inside its own walk.
	total := paginationFixtureBooks * paginationFixturePerBook
	for index := range queries {
		if errs[index] != nil {
			t.Fatalf("session %q: %v", queries[index], errs[index])
		}
		if len(results[index]) != total {
			t.Fatalf("session %q rows = %d, want %d", queries[index], len(results[index]), total)
		}
		seen := make(map[string]bool, total)
		for _, summary := range results[index] {
			if seen[paginationRowKey(summary)] {
				t.Fatalf("session %q repeated row %q", queries[index], paginationRowKey(summary))
			}
			seen[paginationRowKey(summary)] = true
		}
	}
}

// TestContactsPaginationMaximalScanFitsSnapshotBudget proves that a result set
// at the 2,000-card scan cap still paginates. The byte budget equals the REPORT
// budget, so pagination never fails for size alone.
func TestContactsPaginationMaximalScanFitsSnapshotBudget(t *testing.T) {
	rows := make([]contacts.ContactSummary, contacts.MaxCardsScanned)
	for index := range rows {
		rows[index] = contacts.ContactSummary{
			AddressBook:  fmt.Sprintf("book-%02d", index%3),
			UID:          fmt.Sprintf("u-%04d", index),
			ETag:         `"etag-0123456789abcdef"`,
			DisplayName:  fmt.Sprintf("Person %04d", index),
			Organization: "Pagination Org",
			Emails: []contacts.TypedValue{
				{Type: "work", Value: fmt.Sprintf("contact-%04d@example.test", index)},
			},
		}
	}
	sortPaginationRows(rows)
	codec := newContactsCursorCodec()
	if _, _, ok := codec.storeSnapshot(rows); !ok {
		t.Fatalf("a %d-card result set at the scan cap was refused by the snapshot budget", len(rows))
	}
	if got := codec.snapshotBytes(); got > maxContactsSnapshotBytes {
		t.Fatalf("retained bytes = %d, want at most %d", got, maxContactsSnapshotBytes)
	}
	t.Logf("maximal_snapshot rows=%d bytes=%d budget=%d", len(rows), codec.snapshotBytes(), maxContactsSnapshotBytes)
}

// TestContactsPaginationRestartDropsSnapshots defines the restart semantics: a
// new process keeps no snapshot, so a token from the former process fails
// closed. The key is copied here so the rejection proves the snapshot is gone,
// not merely that the signature differs.
func TestContactsPaginationRestartDropsSnapshots(t *testing.T) {
	service := newPaginationFakeService()
	codec := newContactsCursorCodec()
	deps := paginationDeps(t, service, codec)
	first, err := searchContactsHandler(deps)(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 10}))
	if err != nil || first.IsError {
		t.Fatalf("first page: %v %s", err, resultText(t, first))
	}
	var page contacts.SearchResult
	decodeResult(t, first, &page)
	if page.NextCursor == "" {
		t.Fatal("no cursor")
	}

	// A restarted process signs with the same key it loaded again, but it holds
	// no snapshots and no entries.
	restarted := newContactsCursorCodec()
	restarted.key = codec.key
	searchesBefore := service.searchCount()
	res, err := searchContactsHandler(paginationDeps(t, service, restarted))(
		context.Background(), newReq(map[string]any{"cursor": page.NextCursor, "limit": 10}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("a pre-restart cursor was accepted: %s", resultText(t, res))
	}
	payload := contactsErrorPayloadFromResult(t, res)
	if payload.Code != string(contacts.CodeValidation) {
		t.Fatalf("pre-restart cursor error code = %q", payload.Code)
	}
	if got := service.searchCount(); got != searchesBefore {
		t.Fatalf("pre-restart cursor reached the backend: searches %d -> %d", searchesBefore, got)
	}
}

// TestContactsPaginationDesignComparison measures both designs on the same
// fixture bytes. It simulates the former key-position cursor with the same
// rescan and the same sort-key filter, then counts REPORTs and page anomalies
// for a walk that spans a concurrent edit. Every number below comes from the
// fixture, so the trade-off is measured, not asserted.
func TestContactsPaginationDesignComparison(t *testing.T) {
	const limit = 100
	const renamed = "Aaa Renamed"
	counters := &paginationCounters{}
	client, doer := newPaginationClient(counters)

	// Pre-edit ordered rows, as the fixture defines them.
	preRows := paginationExpectedRows(t, client)

	// Snapshot design: page 1 emits the first page, the edit lands, and the
	// remaining pages come from the retained snapshot.
	deps := paginationDeps(t, client, newContactsCursorCodec())
	counters.reports.Store(0)
	first, err := searchContactsHandler(deps)(context.Background(), newReq(map[string]any{"cursor": "start", "limit": limit}))
	if err != nil || first.IsError {
		t.Fatalf("first page: %v %s", err, resultText(t, first))
	}
	var firstPage contacts.SearchResult
	decodeResult(t, first, &firstPage)
	doer.renameCard(0, paginationFixturePerBook-1, renamed)
	snapshotRows := append([]contacts.ContactSummary(nil), firstPage.Contacts...)
	cursor := firstPage.NextCursor
	for pages := 1; cursor != ""; pages++ {
		if pages > 20 {
			t.Fatal("snapshot walk did not terminate")
		}
		res, callErr := searchContactsHandler(deps)(context.Background(), newReq(map[string]any{"cursor": cursor, "limit": limit}))
		if callErr != nil || res.IsError {
			t.Fatalf("snapshot page %d: %v %s", pages, callErr, resultText(t, res))
		}
		var page contacts.SearchResult
		decodeResult(t, res, &page)
		snapshotRows = append(snapshotRows, page.Contacts...)
		cursor = page.NextCursor
	}
	snapshotReports, snapshotBytes := counters.snapshot()

	// Former design: every page rescans all books and keeps rows whose sort key
	// is after the last emitted key.
	counters.reports.Store(0)
	counters.reportBytes.Store(0)
	pageCount := (len(preRows) + limit - 1) / limit
	keyRows := append([]contacts.ContactSummary(nil), preRows[:limit]...)
	position, _ := decodeContactsSortKey(encodeContactsSortKey(
		keyRows[len(keyRows)-1].DisplayName, keyRows[len(keyRows)-1].UID, keyRows[len(keyRows)-1].AddressBook))
	for page := 1; page < pageCount; page++ {
		result, searchErr := client.SearchContacts(context.Background(), contacts.SearchOptions{Limit: limit, PageAll: true})
		if searchErr != nil {
			t.Fatalf("key page %d: %v", page, searchErr)
		}
		var pageRows []contacts.ContactSummary
		for _, summary := range result.Contacts {
			if contactsSortKeyAfter(summary.DisplayName, summary.UID, summary.AddressBook, position) {
				pageRows = append(pageRows, summary)
			}
		}
		if len(pageRows) > limit {
			pageRows = pageRows[:limit]
		}
		if len(pageRows) == 0 {
			break
		}
		keyRows = append(keyRows, pageRows...)
		last := pageRows[len(pageRows)-1]
		position, _ = decodeContactsSortKey(encodeContactsSortKey(last.DisplayName, last.UID, last.AddressBook))
	}
	keyReports, keyBytes := counters.snapshot()

	// Count anomalies against the pre-edit set both designs started from.
	anomalies := func(rows []contacts.ContactSummary) (skipped, duplicated int) {
		expected := make(map[string]bool, len(preRows))
		for _, row := range preRows {
			expected[paginationRowKey(row)] = true
		}
		seen := make(map[string]int, len(rows))
		for _, row := range rows {
			seen[paginationRowKey(row)]++
		}
		for key := range expected {
			if seen[key] == 0 {
				skipped++
			}
		}
		for _, count := range seen {
			if count > 1 {
				duplicated += count - 1
			}
		}
		return skipped, duplicated
	}
	snapshotSkipped, snapshotDuplicated := anomalies(snapshotRows)
	keySkipped, keyDuplicated := anomalies(keyRows)

	t.Logf("design_comparison seed=%s books=%d rows=%d pages=%d limit=%d",
		paginationFixtureSeed, paginationFixtureBooks, len(preRows), pageCount, limit)
	t.Logf("design=snapshot reports=%d report_bytes=%d rows=%d skipped=%d duplicated=%d",
		snapshotReports, snapshotBytes, len(snapshotRows), snapshotSkipped, snapshotDuplicated)
	t.Logf("design=key_cursor reports=%d report_bytes=%d rows=%d skipped=%d duplicated=%d",
		keyReports, keyBytes, len(keyRows), keySkipped, keyDuplicated)

	if snapshotSkipped != 0 || snapshotDuplicated != 0 {
		t.Fatalf("snapshot walk anomalies: skipped=%d duplicated=%d", snapshotSkipped, snapshotDuplicated)
	}
	if snapshotReports >= keyReports {
		t.Fatalf("snapshot walk issued %d REPORTs, key-cursor walk issued %d; the snapshot design did not reduce transfer",
			snapshotReports, keyReports)
	}
	if keySkipped+keyDuplicated == 0 {
		t.Log("note: the fixture edit did not produce a key-cursor anomaly")
	}
}

// TestContactsPaginationZeroMatchesMintsNoToken covers an empty result set.
func TestContactsPaginationZeroMatchesMintsNoToken(t *testing.T) {
	service := newPaginationFakeService()
	service.rows = nil
	codec := newContactsCursorCodec()
	deps := paginationDeps(t, service, codec)
	handler := searchContactsHandler(deps)

	res, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 100}))
	if err != nil || res.IsError {
		t.Fatalf("zero match search: %v %s", err, resultText(t, res))
	}
	var page contacts.SearchResult
	decodeResult(t, res, &page)
	if len(page.Contacts) != 0 || page.NextCursor != "" || page.Truncated || page.ScanLimitReached {
		t.Fatalf("zero match page = %+v", page)
	}
	if got := codec.snapshotCount(); got != 0 {
		t.Fatalf("zero match search retained %d snapshots", got)
	}
}

// TestContactsPaginationPartialScanIssuesNoToken covers a truncated backend scan.
func TestContactsPaginationPartialScanIssuesNoToken(t *testing.T) {
	summaries := make([]contacts.ContactSummary, 3)
	for index := range summaries {
		summaries[index] = contacts.ContactSummary{
			AddressBook: contactsTestBook, UID: fmt.Sprintf("uid-%d", index),
			DisplayName: fmt.Sprintf("Person %d", index),
		}
	}
	service := &fakeContactsService{searchResult: contacts.SearchResult{
		Contacts: summaries, ScanLimitReached: true,
	}}
	codec := newContactsCursorCodec()
	handler := searchContactsHandler(ContactsDeps{
		Service: service, Redactor: security.NewRedactor("unused"), ContactsCursor: codec,
	})
	res, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 2}))
	if err != nil || res.IsError {
		t.Fatalf("partial scan search: %v %s", err, resultText(t, res))
	}
	var page contacts.SearchResult
	decodeResult(t, res, &page)
	if page.NextCursor != "" {
		t.Fatal("a scan-limited result issued a continuation token")
	}
	if !page.ScanLimitReached {
		t.Fatalf("scan-limited page = %+v", page)
	}
	if len(page.Contacts) != 2 {
		t.Fatalf("scan-limited page rows = %d, want 2", len(page.Contacts))
	}
	if got := codec.snapshotCount(); got != 0 {
		t.Fatalf("scan-limited search retained %d snapshots", got)
	}
}

// TestContactsPaginationBookFailureIsSafe covers a failing book at the start of
// a walk: the error is reported, nothing is retained, and a retry works.
func TestContactsPaginationBookFailureIsSafe(t *testing.T) {
	counters := &paginationCounters{}
	client, doer := newPaginationClient(counters)
	codec := newContactsCursorCodec()
	deps := paginationDeps(t, client, codec)
	handler := searchContactsHandler(deps)

	doer.failBook(paginationFixturePaths[1])
	res, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 100}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("failing book was masked: %s", resultText(t, res))
	}
	payload := contactsErrorPayloadFromResult(t, res)
	if payload.Code == "" || payload.Message == "" {
		t.Fatalf("failure payload = %+v", payload)
	}
	for _, secret := range []string{"Person", "u-0001", "contact-0001"} {
		if strings.Contains(payload.Message, secret) {
			t.Fatalf("failure message leaked %q: %s", secret, payload.Message)
		}
	}
	if got := codec.snapshotCount(); got != 0 {
		t.Fatalf("failed walk retained %d snapshots", got)
	}

	doer.healBook(paginationFixturePaths[1])
	retry, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 100}))
	if err != nil || retry.IsError {
		t.Fatalf("retry after a failed walk: %v %s", err, resultText(t, retry))
	}
}

// TestContactsPaginationCancelledRequestStoresNothing covers cancellation.
func TestContactsPaginationCancelledRequestStoresNothing(t *testing.T) {
	counters := &paginationCounters{}
	client, _ := newPaginationClient(counters)
	codec := newContactsCursorCodec()
	deps := paginationDeps(t, client, codec)
	handler := searchContactsHandler(deps)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := handler(ctx, newReq(map[string]any{"cursor": "start", "limit": 100}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("cancelled search returned a page: %s", resultText(t, res))
	}
	if got := codec.snapshotCount(); got != 0 {
		t.Fatalf("cancelled search retained %d snapshots", got)
	}
}

// TestContactsPaginationScopeChangeRejectsCursor keeps the read-scope binding on
// the new token format.
func TestContactsPaginationScopeChangeRejectsCursor(t *testing.T) {
	service := newPaginationFakeService()
	codec := newContactsCursorCodec()
	deps := paginationDeps(t, service, codec)
	deps.ResourceScopes = mustScopes(t, "contacts.read=*")
	handler := searchContactsHandler(deps)

	first, err := handler(context.Background(), newReq(map[string]any{"cursor": "start", "limit": 10}))
	if err != nil || first.IsError {
		t.Fatalf("first page: %v %s", err, resultText(t, first))
	}
	var page contacts.SearchResult
	decodeResult(t, first, &page)

	scoped := deps
	scoped.ResourceScopes = mustScopes(t, "contacts.read=book-00")
	res, err := searchContactsHandler(scoped)(context.Background(), newReq(map[string]any{"cursor": page.NextCursor, "limit": 10}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("cursor from a different read scope was accepted: %s", resultText(t, res))
	}
}
