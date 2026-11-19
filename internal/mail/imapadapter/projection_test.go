package imapadapter

import (
	"strings"
	"testing"
)

// TestMetadataPageFetchOmitsBodyStructure is the first acceptance criterion of
// issue #214. A metadata-only projection must not request BODYSTRUCTURE, so a
// deep or wide MIME tree cannot fail a search or a reply.
func TestMetadataPageFetchOmitsBodyStructure(t *testing.T) {
	fixture := newPageFixture(t, pageOptions{})
	uids := fixture.uids()
	messages, err := fixture.session.FetchMetadata(uids, ProjectionSummary)
	if err != nil {
		t.Fatalf("metadata page error = %v", err)
	}
	if len(messages) != len(uids) {
		t.Fatalf("metadata page messages = %d, want %d", len(messages), len(uids))
	}
	for _, message := range messages {
		if len(message.Parts) != 0 {
			t.Fatalf("metadata-only message kept %d body parts", len(message.Parts))
		}
	}
	fetches := fixture.fetchCommands()
	if len(fetches) != 1 {
		t.Fatalf("metadata FETCH commands = %d, want 1: %v", len(fetches), fetches)
	}
	upper := strings.ToUpper(fetches[0])
	if strings.Contains(upper, "BODYSTRUCTURE") {
		t.Fatalf("metadata FETCH requested BODYSTRUCTURE: %s", fetches[0])
	}
	if strings.Contains(upper, "BODY[") || strings.Contains(upper, "BODY.PEEK") {
		t.Fatalf("metadata FETCH requested a body section: %s", fetches[0])
	}
	for _, item := range []string{"UID", "FLAGS", "ENVELOPE", "INTERNALDATE", "RFC822.SIZE"} {
		if !strings.Contains(upper, item) {
			t.Fatalf("metadata FETCH lost %s: %s", item, fetches[0])
		}
	}
}

// TestReplyProjectionFetchRequestsOnlyCuratedHeaders covers the second
// acceptance criterion: the reply projection gets envelope fields and the four
// curated headers, and no MIME structure or body.
func TestReplyProjectionFetchRequestsOnlyCuratedHeaders(t *testing.T) {
	fixture := newPageFixture(t, pageOptions{})
	uids := fixture.uids()
	messages, err := fixture.session.FetchMetadata(uids[:1], ProjectionReplyHeaders)
	if err != nil {
		t.Fatalf("reply projection error = %v", err)
	}
	if len(messages) != 1 || len(messages[0].Parts) != 0 {
		t.Fatalf("reply projection message = %+v", messages)
	}
	headers := messages[0].Headers
	if headers.MessageID != "<wire-page@example.test>" || len(headers.References) != 2 || len(headers.ReplyTo) != 1 {
		t.Fatalf("reply projection headers = %+v", headers)
	}
	fetches := fixture.fetchCommands()
	if len(fetches) != 1 {
		t.Fatalf("reply FETCH commands = %d, want 1: %v", len(fetches), fetches)
	}
	upper := strings.ToUpper(fetches[0])
	if strings.Contains(upper, "BODYSTRUCTURE") {
		t.Fatalf("reply FETCH requested BODYSTRUCTURE: %s", fetches[0])
	}
	if !strings.Contains(upper, "HEADER.FIELDS") {
		t.Fatalf("reply FETCH did not request curated headers: %s", fetches[0])
	}
	if !strings.Contains(upper, "PEEK") {
		t.Fatalf("reply FETCH must use PEEK: %s", fetches[0])
	}
}

// TestDeepMIMETreeKeepsMetadataProjectionsWorking covers the mixed acceptance
// criterion. A tree deeper than the flatten limit still projects summary and
// reply metadata over the wire, and the full projection keeps its existing
// caps. The wire guard bounds protocol nesting first, so a wide tree is checked
// on the value path instead.
func TestDeepMIMETreeKeepsMetadataProjectionsWorking(t *testing.T) {
	// maxDepth levels of multipart around two leaves put the leaves at MIME path
	// depth maxDepth+1, above the flatten limit. A deeper tree would hit the
	// inbound protocol-nesting guard first, which is a separate cap.
	fixture := newPageFixture(t, pageOptions{parts: 2, depth: maxDepth})
	uids := fixture.uids()
	summaries, err := fixture.session.FetchMetadata(uids[:2], ProjectionSummary)
	if err != nil {
		t.Fatalf("deep tree summary error = %v", err)
	}
	if len(summaries) != 2 || summaries[0].Envelope.Subject == "" {
		t.Fatalf("deep tree summaries = %+v", summaries)
	}
	if _, err := fixture.session.FetchMetadata(uids[:1], ProjectionReplyHeaders); err != nil {
		t.Fatalf("deep tree reply error = %v", err)
	}
	if _, err := fixture.session.FetchMetadata(uids[:1], ProjectionFullMessage); !isAdapterKind(err, ErrorTooLarge) {
		t.Fatalf("deep tree full projection error = %v, want too_large", err)
	}
}

// TestWideMIMETreeKeepsItsFlattenCap keeps the existing wide-tree refusal on
// the value path, where no protocol guard applies first.
func TestWideMIMETreeKeepsItsFlattenCap(t *testing.T) {
	session := &imapPageSession{messages: 1, parts: maxParts + 10}
	if _, err := flattenBodyStructure(session.bodyStructure()); !isAdapterKind(err, ErrorTooLarge) {
		t.Fatalf("wide tree flatten error = %v, want too_large", err)
	}
}
