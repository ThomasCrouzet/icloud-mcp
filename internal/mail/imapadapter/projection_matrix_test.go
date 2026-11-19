package imapadapter

import (
	"strings"
	"testing"
)

// TestProjectionPerReadMatchesTheBetaFETCHMatrix pins one projection to one
// FETCH item set. A read cannot ask for more than its projection names.
func TestProjectionPerReadMatchesTheBetaFETCHMatrix(t *testing.T) {
	for _, test := range []struct {
		name        string
		projection  Projection
		wantItems   []string
		deniedItems []string
	}{
		{
			name:        "summary",
			projection:  ProjectionSummary,
			wantItems:   []string{"UID", "FLAGS", "ENVELOPE", "INTERNALDATE", "RFC822.SIZE"},
			deniedItems: []string{"BODYSTRUCTURE", "BODY.PEEK", "HEADER.FIELDS"},
		},
		{
			name:        "reply-headers",
			projection:  ProjectionReplyHeaders,
			wantItems:   []string{"UID", "FLAGS", "ENVELOPE", "INTERNALDATE", "RFC822.SIZE", "HEADER.FIELDS"},
			deniedItems: []string{"BODYSTRUCTURE"},
		},
		{
			name:        "full-message",
			projection:  ProjectionFullMessage,
			wantItems:   []string{"UID", "FLAGS", "ENVELOPE", "INTERNALDATE", "RFC822.SIZE", "BODYSTRUCTURE", "HEADER.FIELDS"},
			deniedItems: nil,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPageFixture(t, pageOptions{})
			if _, err := fixture.session.FetchMetadata(fixture.uids()[:1], test.projection); err != nil {
				t.Fatalf("%s projection error = %v", test.projection, err)
			}
			fetches := fixture.fetchCommands()
			if len(fetches) != 1 {
				t.Fatalf("FETCH commands = %d, want 1: %v", len(fetches), fetches)
			}
			upper := strings.ToUpper(fetches[0])
			for _, item := range test.wantItems {
				if !strings.Contains(upper, item) {
					t.Fatalf("%s projection lost %s: %s", test.projection, item, fetches[0])
				}
			}
			for _, item := range test.deniedItems {
				if strings.Contains(upper, item) {
					t.Fatalf("%s projection requested %s: %s", test.projection, item, fetches[0])
				}
			}
		})
	}
}

// TestInvalidProjectionIsRejectedBeforeAnyCommand keeps the selector closed.
func TestInvalidProjectionIsRejectedBeforeAnyCommand(t *testing.T) {
	fixture := newPageFixture(t, pageOptions{})
	before := fixture.server.fetches.Load()
	for _, projection := range []Projection{0, 4, 255} {
		if _, err := fixture.session.FetchMetadata(fixture.uids()[:1], projection); !isAdapterKind(err, ErrorProtocol) {
			t.Fatalf("projection %d error = %v, want protocol", projection, err)
		}
	}
	if got := fixture.server.fetches.Load() - before; got != 0 {
		t.Fatalf("invalid projections sent %d FETCH commands", got)
	}
}
