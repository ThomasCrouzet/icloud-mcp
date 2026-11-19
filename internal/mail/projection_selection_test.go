package mail

import (
	"context"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

func (f *fakeIMAP) recordedProjections() []imapadapter.Projection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]imapadapter.Projection(nil), f.projections...)
}

// TestSearchMessagesRequestsSummaryProjection proves that search_messages asks
// the adapter for summary metadata, so its FETCH cannot request a MIME tree.
func TestSearchMessagesRequestsSummaryProjection(t *testing.T) {
	t.Parallel()
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 8, UIDNext: 3},
		searchFn: func(imapadapter.SearchRequest) ([]uint32, error) { return []uint32{2}, nil },
		metadata: []imapadapter.Message{{
			UID: 2, Envelope: imapadapter.Envelope{Subject: "Summary only"},
		}},
	}
	service := testService(t, imap, nil, false, false)
	if _, err := service.SearchMessages(context.Background(), SearchInput{Mailbox: "INBOX", Limit: 5}); err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	got := imap.recordedProjections()
	if len(got) != 1 || got[0] != imapadapter.ProjectionSummary {
		t.Fatalf("search projections = %v, want [%v]", got, imapadapter.ProjectionSummary)
	}
}

// TestGetReplyMetadataRequestsReplyHeadersProjection proves that a reply asks
// for curated threading headers and never for the MIME tree.
func TestGetReplyMetadataRequestsReplyHeadersProjection(t *testing.T) {
	t.Parallel()
	var log []string
	fakeSMTP := &fakeSMTP{log: &log}
	imap := replyFixture(imapadapter.CuratedHeaders{MessageID: "<source@example.com>"}, imapadapter.Envelope{
		Subject: "Subject",
		From:    []imapadapter.Address{{Address: "author@example.com"}},
	})
	service := testService(t, imap, fakeSMTP, false, true)
	if _, err := service.ReplyMessage(context.Background(), ReplyInput{
		Mailbox: "INBOX", UIDValidity: 12, UID: 4, Body: "reply",
	}); err != nil {
		t.Fatalf("ReplyMessage() error = %v", err)
	}
	got := imap.recordedProjections()
	if len(got) != 1 || got[0] != imapadapter.ProjectionReplyHeaders {
		t.Fatalf("reply projections = %v, want [%v]", got, imapadapter.ProjectionReplyHeaders)
	}
}

// TestGetMessageRequestsFullMessageProjection proves that only get_message asks
// for the MIME tree.
func TestGetMessageRequestsFullMessageProjection(t *testing.T) {
	t.Parallel()
	imap := plainMetadata(imapadapter.BodyPart{Path: []int{1}, ContentType: "text/plain", InlinePlain: true})
	imap.bodyByPath = map[string]imapadapter.BodyData{
		"1": {MIMEHeader: []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n"), Body: []byte("body")},
	}
	service := testService(t, imap, nil, false, false)
	if _, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1}); err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	got := imap.recordedProjections()
	if len(got) != 1 || got[0] != imapadapter.ProjectionFullMessage {
		t.Fatalf("get_message projections = %v, want [%v]", got, imapadapter.ProjectionFullMessage)
	}
}

// TestMailReadProjectionsAreDistinct keeps the three read paths pinned to three
// different projections in one place.
func TestMailReadProjectionsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := map[imapadapter.Projection]string{
		imapadapter.ProjectionSummary:      "search_messages",
		imapadapter.ProjectionReplyHeaders: "reply_message",
		imapadapter.ProjectionFullMessage:  "get_message",
	}
	if len(seen) != 3 {
		t.Fatalf("projection constants are not distinct: %v", seen)
	}
	for projection, tool := range seen {
		if !projection.Valid() {
			t.Fatalf("%s projection %d is not valid", tool, projection)
		}
	}
}
