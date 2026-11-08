package mail

import (
	"context"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

// A concurrent expunge between UID SEARCH and UID FETCH must not move the
// continuation cursor to the floor of the scanned window.
func TestSearchMessagesKeepsCursorWhenSelectedUIDsVanish(t *testing.T) {
	t.Parallel()
	live := []uint32{10000, 9999, 9998}
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 4, UIDNext: 10001},
		searchFn: func(request imapadapter.SearchRequest) ([]uint32, error) {
			var matches []uint32
			for _, uid := range live {
				if uid >= request.UIDMin && uid <= request.UIDMax {
					matches = append(matches, uid)
				}
			}
			return matches, nil
		},
		// 10000 and 9999 were expunged before the metadata fetch.
		metadata: []imapadapter.Message{{UID: 9998}},
	}
	service := testService(t, imap, nil, false, false)

	first, err := service.SearchMessages(context.Background(), SearchInput{Mailbox: "INBOX", Limit: 2})
	if err != nil {
		t.Fatalf("first page error = %v", err)
	}
	if len(first.Messages) != 0 {
		t.Fatalf("first page messages = %#v", first.Messages)
	}
	if first.NextBeforeUID != 9999 {
		t.Fatalf("NextBeforeUID = %d, want 9999", first.NextBeforeUID)
	}

	second, err := service.SearchMessages(context.Background(), SearchInput{
		Mailbox: "INBOX", Limit: 2, BeforeUID: first.NextBeforeUID, UIDValidity: first.UIDValidity,
	})
	if err != nil {
		t.Fatalf("second page error = %v", err)
	}
	if len(second.Messages) != 1 || second.Messages[0].UID != 9998 {
		t.Fatalf("second page = %#v, want live UID 9998", second.Messages)
	}
}

func TestSearchMessagesEmptyResultKeepsScanBoundary(t *testing.T) {
	t.Parallel()
	imap := &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 5, UIDNext: 60001},
		searchFn: func(imapadapter.SearchRequest) ([]uint32, error) { return nil, nil },
	}
	service := testService(t, imap, nil, false, false)
	result, err := service.SearchMessages(context.Background(), SearchInput{Mailbox: "INBOX", Limit: 2})
	if err != nil {
		t.Fatalf("SearchMessages() error = %v", err)
	}
	if len(result.Messages) != 0 || result.NextBeforeUID != 10001 || !result.ScanLimitReached {
		t.Fatalf("empty result = %#v", result)
	}
}
