package imapadapter

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// alternativeWithMixed is the deterministic fixture from issue #213:
//
//	multipart/alternative
//	├── multipart/mixed
//	│   ├── text/plain: "Part A"
//	│   ├── text/plain: "Part B"
//	│   └── application/pdf attachment
//	└── text/html: equivalent rendering of A and B
//
// The two plain leaves are independent segments inside one alternative branch.
// The HTML leaf is a competing branch and must be suppressed when the mixed
// branch is chosen.
func alternativeWithMixed() *imap.BodyStructureMultiPart {
	return &imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
		&imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
			singlePart("plain"),
			singlePart("plain"),
			&imap.BodyStructureSinglePart{
				Type: "application", Subtype: "pdf", Size: 40,
				Extended: &imap.BodyStructureSinglePartExt{
					Disposition: &imap.BodyStructureDisposition{
						Value: "attachment", Params: map[string]string{"filename": "terms.pdf"},
					},
				},
			},
		}},
		singlePart("html"),
	}}
}

// TestFlattenAlternativeBranchKeepsIndependentTextSegments is the first
// acceptance criterion of issue #213. A text part inside a child multipart/mixed
// branch stays an independent segment, so a later sibling in the same branch is
// not suppressed as an equivalent alternative.
func TestFlattenAlternativeBranchKeepsIndependentTextSegments(t *testing.T) {
	parts, err := flattenBodyStructure(alternativeWithMixed())
	if err != nil {
		t.Fatalf("flattenBodyStructure() error = %v", err)
	}
	var plain []BodyPart
	var html []BodyPart
	attachment := 0
	for _, part := range parts {
		switch {
		case part.InlinePlain:
			plain = append(plain, part)
		case part.ContentType == "text/html":
			html = append(html, part)
		}
		if part.Attachment {
			attachment++
		}
	}
	if len(plain) != 2 {
		t.Fatalf("inline plain parts = %d, want 2: %+v", len(plain), plain)
	}
	if len(html) != 1 {
		t.Fatalf("html parts = %d, want 1", len(html))
	}
	if attachment != 1 {
		t.Fatalf("attachments = %d, want 1", attachment)
	}
	// One alternative ancestor, so one group.
	if plain[0].AlternativeGroup == "" || plain[0].AlternativeGroup != plain[1].AlternativeGroup {
		t.Fatalf("plain group identity = %q / %q", plain[0].AlternativeGroup, plain[1].AlternativeGroup)
	}
	// Same branch, so independent segments, never competing alternatives.
	if plain[0].AlternativeBranch == "" {
		t.Fatal("plain part lost its alternative branch identity")
	}
	if plain[0].AlternativeBranch != plain[1].AlternativeBranch {
		t.Fatalf("independent segments of one branch differ: %q / %q", plain[0].AlternativeBranch, plain[1].AlternativeBranch)
	}
	// A competing branch must not share the branch identity.
	if html[0].AlternativeBranch == "" || html[0].AlternativeBranch == plain[0].AlternativeBranch {
		t.Fatalf("competing branch identity = %q, branch = %q", html[0].AlternativeBranch, plain[0].AlternativeBranch)
	}
}

// TestFlattenAlternativeBranchNestedMixedAlternativeMixed covers the deeper
// layout named in the acceptance criteria. Only the innermost alternative
// branches compete; outer mixed parts stay independent.
func TestFlattenAlternativeBranchNestedMixedAlternativeMixed(t *testing.T) {
	root := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
		singlePart("plain"),
		&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
			&imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
				singlePart("plain"),
				singlePart("plain"),
			}},
			&imap.BodyStructureMultiPart{Subtype: "related", Children: []imap.BodyStructure{
				singlePart("html"),
				singlePart("plain"),
			}},
		}},
		singlePart("plain"),
	}}
	parts, err := flattenBodyStructure(root)
	if err != nil {
		t.Fatalf("flattenBodyStructure() error = %v", err)
	}
	var plain []BodyPart
	for _, part := range parts {
		if part.InlinePlain {
			plain = append(plain, part)
		}
	}
	if len(plain) != 5 {
		t.Fatalf("inline plain parts = %d, want 5", len(plain))
	}
	// Parts 1 and 5 are outer mixed leaves, outside every alternative.
	if plain[0].AlternativeGroup != "" || plain[4].AlternativeGroup != "" {
		t.Fatalf("outer mixed parts joined an alternative group: %q %q",
			plain[0].AlternativeGroup, plain[4].AlternativeGroup)
	}
	// Parts 2, 3, and 4 are inside the inner alternative.
	if plain[1].AlternativeGroup == "" || plain[1].AlternativeGroup != plain[2].AlternativeGroup ||
		plain[1].AlternativeGroup != plain[3].AlternativeGroup {
		t.Fatalf("inner alternative groups = %q / %q / %q",
			plain[1].AlternativeGroup, plain[2].AlternativeGroup, plain[3].AlternativeGroup)
	}
	// Parts 2 and 3 are independent segments of the mixed branch.
	if plain[1].AlternativeBranch == "" || plain[1].AlternativeBranch != plain[2].AlternativeBranch {
		t.Fatalf("nested mixed siblings lost independent segment identity: %q / %q",
			plain[1].AlternativeBranch, plain[2].AlternativeBranch)
	}
	// Part 4 is the competing related branch.
	if plain[3].AlternativeBranch == "" || plain[3].AlternativeBranch == plain[1].AlternativeBranch {
		t.Fatalf("competing related branch identity = %q", plain[3].AlternativeBranch)
	}
}

// TestFlattenAlternativeBranchBlankAndOversizedLeaves keeps the branch model
// intact across the leaf shapes the acceptance criteria name.
func TestFlattenAlternativeBranchBlankAndOversizedLeaves(t *testing.T) {
	root := &imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
		&imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 0},
			&imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: maxWireBodyBytes + 1},
			singlePart("plain"),
		}},
		singlePart("html"),
	}}
	parts, err := flattenBodyStructure(root)
	if err != nil {
		t.Fatalf("flattenBodyStructure() error = %v", err)
	}
	var plain []BodyPart
	for _, part := range parts {
		if part.InlinePlain {
			plain = append(plain, part)
		}
	}
	if len(plain) != 3 {
		t.Fatalf("inline plain parts = %d, want 3", len(plain))
	}
	for index := 1; index < len(plain); index++ {
		if plain[index].AlternativeBranch != plain[0].AlternativeBranch {
			t.Fatalf("plain part %d changed branch identity: %q vs %q", index, plain[index].AlternativeBranch, plain[0].AlternativeBranch)
		}
	}
}
