package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/mail/imapadapter"
)

const alternativeBranchHeader = "Content-Type: text/plain; charset=utf-8\r\n\r\n"

// nestedAlternativeMetadata mirrors the body structure of the issue #213
// fixture after flattening. Both plain parts belong to one alternative branch,
// and the HTML part competes with the whole branch.
func nestedAlternativeMetadata() *fakeIMAP {
	return &fakeIMAP{
		selected: imapadapter.SelectedMailbox{UIDValidity: 3, UIDNext: 2},
		metadata: []imapadapter.Message{{
			UID: 1, Envelope: imapadapter.Envelope{Subject: "Nested alternatives"},
			Parts: []imapadapter.BodyPart{
				{Path: []int{1}, ContentType: "multipart/alternative"},
				{Path: []int{1, 1}, ContentType: "multipart/mixed", AlternativeGroup: "root", AlternativeBranch: "root.1"},
				{
					Path: []int{1, 1, 1}, ContentType: "text/plain", InlinePlain: true,
					AlternativeGroup: "root", AlternativeBranch: "root.1",
				},
				{
					Path: []int{1, 1, 2}, ContentType: "text/plain", InlinePlain: true,
					AlternativeGroup: "root", AlternativeBranch: "root.1",
				},
				{Path: []int{1, 1, 3}, ContentType: "application/pdf", Attachment: true, Filename: "terms.pdf"},
				{
					Path: []int{1, 2}, ContentType: "text/html",
					AlternativeGroup: "root", AlternativeBranch: "root.2",
				},
			},
		}},
		bodyByPath: map[string]imapadapter.BodyData{
			"1.1.1": {MIMEHeader: []byte(alternativeBranchHeader), Body: []byte("Part A")},
			"1.1.2": {MIMEHeader: []byte(alternativeBranchHeader), Body: []byte("Part B")},
			"1.2":   {MIMEHeader: []byte("Content-Type: text/html; charset=utf-8\r\n\r\n"), Body: []byte("<p>Part A Part B</p>")},
		},
	}
}

// TestGetMessageKeepsIndependentSegmentsInsideAnAlternativeBranch is the core
// correctness criterion of issue #213. A message must not lose "Part B" when a
// child multipart/mixed branch of a multipart/alternative holds two
// independent plain segments.
func TestGetMessageKeepsIndependentSegmentsInsideAnAlternativeBranch(t *testing.T) {
	t.Parallel()
	service := testService(t, nestedAlternativeMetadata(), nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if !strings.Contains(message.Body, "Part A") || !strings.Contains(message.Body, "Part B") {
		t.Fatalf("nested branch body = %q", message.Body)
	}
	if strings.Index(message.Body, "Part A") > strings.Index(message.Body, "Part B") {
		t.Fatalf("nested branch segments are out of order: %q", message.Body)
	}
	if strings.Count(message.Body, "Part A") != 1 || strings.Count(message.Body, "Part B") != 1 {
		t.Fatalf("nested branch segment repeated or lost: %q", message.Body)
	}
	if message.BodyOmitted {
		t.Fatalf("nested branch lost content silently: %#v", message.Warnings)
	}
	if strings.Contains(message.Body, "Part A Part B") {
		t.Fatalf("competing alternative branch was appended: %q", message.Body)
	}
}

// TestGetMessageNeverAppendsACompetingAlternativeBranch pins the suppression
// rule that issue #213 keeps: a chosen branch suppresses its equivalent
// siblings.
func TestGetMessageNeverAppendsACompetingAlternativeBranch(t *testing.T) {
	t.Parallel()
	imap := nestedAlternativeMetadata()
	imap.metadata[0].Parts = append(imap.metadata[0].Parts,
		imapadapter.BodyPart{
			Path: []int{1, 3}, ContentType: "text/plain", InlinePlain: true,
			AlternativeGroup: "root", AlternativeBranch: "root.3",
		})
	imap.bodyByPath["1.3"] = imapadapter.BodyData{
		MIMEHeader: []byte(alternativeBranchHeader), Body: []byte("competing plain branch"),
	}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if strings.Contains(message.Body, "competing plain branch") {
		t.Fatalf("competing alternative branch was appended: %q", message.Body)
	}
	for _, entry := range imap.log {
		if entry == "body-peek" {
			continue
		}
	}
	if got := strings.Count(strings.Join(imap.log, ","), "body-peek"); got != 2 {
		t.Fatalf("body fetches = %d, want 2 independent segments: %v", got, imap.log)
	}
}

// TestGetMessageReportsALimitOmissionInsideAnAlternativeBranch covers the
// explicit-omission criterion. Later segments of a branch that do not fit must
// set bodyOmitted with a bounded warning, never disappear silently. An
// oversized single segment reports body_too_large.
func TestGetMessageReportsALimitOmissionInsideAnAlternativeBranch(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		first, last string
		wantCode    string
	}{
		{name: "join-truncation", first: strings.Repeat("A", 60), last: strings.Repeat("B", 60), wantCode: "body_truncated"},
		{name: "oversized-segment", first: "Part A", last: strings.Repeat("B", 200), wantCode: "body_too_large"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			imap := nestedAlternativeMetadata()
			imap.bodyByPath["1.1.1"] = imapadapter.BodyData{
				MIMEHeader: []byte(alternativeBranchHeader), Body: []byte(test.first),
			}
			imap.bodyByPath["1.1.2"] = imapadapter.BodyData{
				MIMEHeader: []byte(alternativeBranchHeader), Body: []byte(test.last),
			}
			service := testService(t, imap, nil, false, false)
			message, err := service.GetMessage(context.Background(), GetMessageInput{
				Mailbox: "INBOX", UIDValidity: 3, UID: 1, MaxBodyBytes: 100,
			})
			if err != nil {
				t.Fatalf("GetMessage() error = %v", err)
			}
			if !message.BodyOmitted {
				t.Fatalf("limit omission was not reported: %#v", message)
			}
			if len(message.Body) > 100 {
				t.Fatalf("bounded body = %d bytes", len(message.Body))
			}
			found := false
			for _, warning := range message.Warnings {
				if warning.Code == test.wantCode {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s warning: %#v", test.wantCode, message.Warnings)
			}
		})
	}
}

// TestGetMessageNeverLeaksAttachmentBytesInsideAnAlternativeBranch keeps the
// attachment rule intact for the nested layout.
func TestGetMessageNeverLeaksAttachmentBytesInsideAnAlternativeBranch(t *testing.T) {
	t.Parallel()
	imap := nestedAlternativeMetadata()
	imap.bodyByPath["1.1.3"] = imapadapter.BodyData{
		MIMEHeader: []byte("Content-Type: application/pdf\r\n\r\n"), Body: []byte("PDF-1.7 secret bytes"),
	}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if strings.Contains(message.Body, "PDF-1.7") {
		t.Fatalf("attachment bytes leaked into the body: %q", message.Body)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].Filename != "terms.pdf" {
		t.Fatalf("attachment metadata = %#v", message.Attachments)
	}
}

// TestGetMessageKeepsABlankSegmentInsideAnAlternativeBranch covers the blank
// plain part case. A blank segment must not consume the branch.
func TestGetMessageKeepsABlankSegmentInsideAnAlternativeBranch(t *testing.T) {
	t.Parallel()
	imap := nestedAlternativeMetadata()
	imap.bodyByPath["1.1.1"] = imapadapter.BodyData{
		MIMEHeader: []byte(alternativeBranchHeader), Body: []byte("   \r\n\t"),
	}
	service := testService(t, imap, nil, false, false)
	message, err := service.GetMessage(context.Background(), GetMessageInput{Mailbox: "INBOX", UIDValidity: 3, UID: 1})
	if err != nil {
		t.Fatalf("GetMessage() error = %v", err)
	}
	if !strings.Contains(message.Body, "Part B") {
		t.Fatalf("blank segment consumed the branch: %q", message.Body)
	}
}
