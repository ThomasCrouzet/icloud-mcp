package imapadapter

import "testing"

// BenchmarkIMAPWireMetadataPage measures one metadata-only page of 20
// messages with large multipart envelopes. It reports the wire bytes, the
// allocation totals, and fixture sizes, so a projection regression is visible
// in the recorded baseline.
//
// Before the projection split the same page requested BODYSTRUCTURE and
// flattened a 40-part tree for every message.
func BenchmarkIMAPWireMetadataPage(b *testing.B) {
	fixture := newPageFixture(b, pageOptions{})
	uids := fixture.uids()
	messages, err := fixture.session.FetchMetadata(uids, ProjectionSummary)
	if err != nil {
		b.Fatal(err)
	}
	if len(messages) != len(uids) {
		b.Fatalf("page messages = %d, want %d", len(messages), len(uids))
	}
	base := fixture.responses.written.Load()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := fixture.session.FetchMetadata(uids, ProjectionSummary); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(fixture.responses.written.Load()-base)/float64(b.N), "wire-bytes/op")
	b.ReportMetric(float64(len(uids)), "page-messages")
	b.ReportMetric(float64(imapPageParts), "mime-parts")
	b.ReportMetric(float64(imapPageUIDBase), "sparse-uid-base")
}

// TestMetadataPageProjectionStaysBounded asserts the projection contract in
// the ordinary test run, so a regression fails without a benchmark run.
func TestMetadataPageProjectionStaysBounded(t *testing.T) {
	fixture := newPageFixture(t, pageOptions{})
	uids := fixture.uids()
	guard, ok := fixture.session.conn.(*guardedConn)
	if !ok {
		t.Fatalf("page session guard type = %T", fixture.session.conn)
	}
	before := guard.total
	if _, err := fixture.session.FetchMetadata(uids, ProjectionSummary); err != nil {
		t.Fatalf("summary page error = %v", err)
	}
	summaryBytes := guard.total - before
	if got := len(fixture.fetchCommands()); got != 1 {
		t.Fatalf("summary FETCH commands = %d, want 1", got)
	}
	if got := fixture.server.structs.Load(); got != 0 {
		t.Fatalf("summary projection asked for %d BODYSTRUCTURE values", got)
	}
	// A summary page must stay well inside the 4 MiB inbound session budget,
	// so removing BODYSTRUCTURE keeps room for the message text it does read.
	if summaryBytes > maxSessionBytes/2 {
		t.Fatalf("summary page used %d inbound bytes of %d", summaryBytes, maxSessionBytes)
	}
	// Full projection must still ask for exactly one BODYSTRUCTURE.
	if _, err := fixture.session.FetchMetadata(uids[:1], ProjectionFullMessage); err != nil {
		t.Fatalf("full projection error = %v", err)
	}
	if got := fixture.server.structs.Load(); got != 1 {
		t.Fatalf("full projection BODYSTRUCTURE count = %d, want 1", got)
	}
}
