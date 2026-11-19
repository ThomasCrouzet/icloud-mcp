package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

// nestedAlternativeMetadata mirrors the body structure of the review repro
// after flattening:
//
//	multipart/alternative
//	├── multipart/mixed
//	│   ├── text/plain: "Intro text"
//	│   └── multipart/alternative
//	│       ├── text/plain: "bonjour"
//	│       └── text/plain: "bonjour latin1 twin"
//	└── text/html
func nestedAlternativeScopesMetadata() *fakeIMAP {
	outer := imapadapter.AlternativeScope{Group: "root", Branch: "1"}
	innerFirst := imapadapter.AlternativeScope{Group: "1.2", Branch: "1.2.1"}
	innerTwin := imapadapter.AlternativeScope{Group: "1.2", Branch: "1.2.2"}
	return &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 3, UIDNext: 2},
		metadata: []imapadapter.Message{{
			UID: 1, Envelope: imapadapter.Envelope{Subject: "Nested alternatives"},
			Parts: []imapadapter.BodyPart{
				{Path: []int{1}, ContentType: "multipart/alternative"},
				{
					Path: []int{1, 1}, ContentType: "multipart/mixed",
					AlternativeGroup: outer.Group, AlternativeBranch: outer.Branch,
					AlternativeScopes: []imapadapter.AlternativeScope{outer},
				},
				{
					Path: []int{1, 1, 1}, ContentType: "text/plain", InlinePlain: true,
					AlternativeGroup: outer.Group, AlternativeBranch: outer.Branch,
					AlternativeScopes: []imapadapter.AlternativeScope{outer},
				},
				{
					Path: []int{1, 1, 2}, ContentType: "multipart/alternative",
					AlternativeGroup: outer.Group, AlternativeBranch: outer.Branch,
					AlternativeScopes: []imapadapter.AlternativeScope{outer},
				},
				{
					Path: []int{1, 1, 2, 1}, ContentType: "text/plain", InlinePlain: true,
					AlternativeGroup: outer.Group, AlternativeBranch: outer.Branch,
					AlternativeScopes: []imapadapter.AlternativeScope{outer, innerFirst},
				},
				{
					Path: []int{1, 1, 2, 2}, ContentType: "text/plain", InlinePlain: true,
					AlternativeGroup: outer.Group, AlternativeBranch: outer.Branch,
					AlternativeScopes: []imapadapter.AlternativeScope{outer, innerTwin},
				},
				{
					Path: []int{1, 2}, ContentType: "text/html",
					AlternativeGroup: outer.Group, AlternativeBranch: "2",
					AlternativeScopes: []imapadapter.AlternativeScope{{Group: outer.Group, Branch: "2"}},
				},
			},
		}},
		bodyByPath: map[string]imapadapter.BodyData{
			"1.1.1":   {MIMEHeader: []byte(alternativeBranchHeader), Body: []byte("Intro text")},
			"1.1.2.1": {MIMEHeader: []byte(alternativeBranchHeader), Body: []byte("bonjour")},
			"1.1.2.2": {MIMEHeader: []byte(alternativeBranchHeader), Body: []byte("bonjour latin1 twin")},
			"1.2":     {MIMEHeader: []byte("Content-Type: text/html; charset=utf-8\r\n\r\n"), Body: []byte("<p>Intro text bonjour</p>")},
		},
	}
}

// TestGetMessageInnerAlternativeSuppressesCompetingPlainTwin is the blocking
// finding of the review. A nested multipart/alternative inside an outer
// alternative branch must still suppress its own competing branch, while the
// independent mixed sibling of the outer branch stays.
func TestGetMessageInnerAlternativeSuppressesCompetingPlainTwin(t *testing.T) {
	t.Parallel()
	service := testService(t, nestedAlternativeScopesMetadata(), nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if !strings.Contains(message.Body, "Intro text") {
		t.Fatalf("independent outer segment was lost: %q", message.Body)
	}
	if !strings.Contains(message.Body, "bonjour") {
		t.Fatalf("inner preferred branch was lost: %q", message.Body)
	}
	if strings.Contains(message.Body, "bonjour latin1 twin") {
		t.Fatalf("competing inner branch was appended: %q", message.Body)
	}
	if strings.Count(message.Body, "bonjour") != 1 {
		t.Fatalf("inner branch repeated: %q", message.Body)
	}
	if message.BodyOmitted {
		t.Fatalf("nested alternative reported a silent omission: %#v", message.Warnings)
	}
}

// TestGetMessageNeverFetchesCompetingInnerBranch keeps the suppressed branch
// off the wire, which is what the first #213 fix already did for one level.
func TestGetMessageNeverFetchesCompetingInnerBranch(t *testing.T) {
	t.Parallel()
	imap := nestedAlternativeScopesMetadata()
	service := testService(t, imap, nil, false, false)
	if _, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1}); err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	// Two segments are kept: the outer mixed leaf and the inner preferred
	// leaf. The competing inner twin is never fetched.
	if got := strings.Count(strings.Join(imap.log, ","), "body-peek"); got != 2 {
		t.Fatalf("body fetches = %d, want 2: %v", got, imap.log)
	}
}

// TestGetMessageNestedAlternativeKeepsOuterMixedSiblings proves the repair
// keeps the #213 behaviour at the outer level.
func TestGetMessageNestedAlternativeKeepsOuterMixedSiblings(t *testing.T) {
	t.Parallel()
	imap := nestedAlternativeScopesMetadata()
	imap.metadata[0].Parts = append([]imapadapter.BodyPart{{
		Path: []int{1, 1, 0}, ContentType: "text/plain", InlinePlain: true,
		AlternativeGroup: "root", AlternativeBranch: "1",
		AlternativeScopes: []imapadapter.AlternativeScope{{Group: "root", Branch: "1"}},
	}}, imap.metadata[0].Parts...)
	imap.bodyByPath["1.1.0"] = imapadapter.BodyData{
		MIMEHeader: []byte(alternativeBranchHeader), Body: []byte("Second outer segment"),
	}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	for _, want := range []string{"Second outer segment", "Intro text", "bonjour"} {
		if !strings.Contains(message.Body, want) {
			t.Fatalf("missing %q in %q", want, message.Body)
		}
	}
	if strings.Contains(message.Body, "bonjour latin1 twin") {
		t.Fatalf("competing inner branch was appended: %q", message.Body)
	}
}

// TestGetMessageNestedAlternativeReportsLimitOmission keeps the explicit
// omission rule for a nested level.
func TestGetMessageNestedAlternativeReportsLimitOmission(t *testing.T) {
	t.Parallel()
	imap := nestedAlternativeScopesMetadata()
	imap.bodyByPath["1.1.2.1"] = imapadapter.BodyData{
		MIMEHeader: []byte(alternativeBranchHeader), Body: []byte(strings.Repeat("A", 60)),
	}
	imap.bodyByPath["1.1.1"] = imapadapter.BodyData{
		MIMEHeader: []byte(alternativeBranchHeader), Body: []byte(strings.Repeat("I", 60)),
	}
	imap.bodyByPath["1.1.0"] = imapadapter.BodyData{}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{
		Mailbox: "INBOX", UIDValidity: 3, UID: 1, MaxBodyBytes: 100,
	})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if !message.BodyOmitted || len(message.Body) > 100 {
		t.Fatalf("bounded body = %d bytes omitted=%v", len(message.Body), message.BodyOmitted)
	}
	found := false
	for _, warning := range message.Warnings {
		if warning.Code == "body_truncated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing body_truncated warning: %#v", message.Warnings)
	}
}

// TestGetMessageNestedAlternativeKeepsPreferredInnerLeaf pins the exact
// expected body for the review fixture.
//
// The outer alternative selects its mixed branch, and the mixed siblings are
// independent: the "Intro text" leaf and the nested alternative both count.
// The nested alternative then selects one of its two competing leaves. The
// result is therefore "Intro text" plus one twin.
//
// This differs from the pre-#213 baseline ff572b8, which returned "Intro text"
// alone because every descendant of the outer alternative shared one group. That
// older result dropped an independent mixed part, which is the silent omission
// that #213 fixes, so the repair keeps the preferred inner leaf instead.
func TestGetMessageNestedAlternativeKeepsPreferredInnerLeaf(t *testing.T) {
	t.Parallel()
	service := testService(t, nestedAlternativeScopesMetadata(), nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if message.Body != "Intro text\n\nbonjour" {
		t.Fatalf("body = %q, want %q", message.Body, "Intro text\n\nbonjour")
	}
	if message.BodySource != "plain_text" || message.BodyOmitted {
		t.Fatalf("body metadata = %+v", message)
	}
}
