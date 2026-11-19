package imapadapter

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

// nestedAlternativeInsideBranch is the fixture reported against the first
// #213 attempt:
//
//	multipart/alternative                       (outer, group G1)
//	├── multipart/mixed                         (outer branch B1)
//	│   ├── text/plain: "Intro"
//	│   └── multipart/alternative               (inner, group G2)
//	│       ├── text/plain: "bonjour"
//	│       └── text/plain: "bonjour latin1 twin"
//	└── text/html
//
// The two inner leaves compete with each other. They are not independent
// segments of the outer branch, because the inner alternative is a new
// alternative level and not a mixed sibling group.
func nestedAlternativeInsideBranch() *imap.BodyStructureMultiPart {
	return &imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
		&imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
			singlePart("plain"),
			&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
				singlePart("plain"),
				singlePart("plain"),
			}},
		}},
		singlePart("html"),
	}}
}

// TestFlattenNestedAlternativeKeepsItsOwnCompetingBranches is the blocking
// finding of the review. A nested multipart/alternative inside an outer
// alternative branch must open its own scope, so its leaves compete with each
// other.
func TestFlattenNestedAlternativeKeepsItsOwnCompetingBranches(t *testing.T) {
	parts, err := flattenBodyStructure(nestedAlternativeInsideBranch())
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
		t.Fatalf("inline plain parts = %d, want 3: %+v", len(plain), plain)
	}
	intro, first, twin := plain[0], plain[1], plain[2]

	// The intro leaf is one independent segment of the outer branch.
	if intro.AlternativeGroup == "" || intro.AlternativeBranch == "" {
		t.Fatalf("intro leaf lost its outer scope: %+v", intro)
	}
	// The inner twins are in the same outer branch as the intro leaf.
	if first.AlternativeGroup != intro.AlternativeGroup || first.AlternativeBranch != intro.AlternativeBranch {
		t.Fatalf("inner leaf left the outer branch: %+v vs %+v", first, intro)
	}
	// The inner twins carry a second scope of their own.
	if len(first.AlternativeScopes) != 2 || len(twin.AlternativeScopes) != 2 {
		t.Fatalf("inner leaf scopes = %d / %d, want 2", len(first.AlternativeScopes), len(twin.AlternativeScopes))
	}
	inner := first.AlternativeScopes[1]
	if inner.Group == "" || inner.Branch == "" {
		t.Fatalf("inner scope is incomplete: %+v", inner)
	}
	// The twin must compete with the first inner leaf, not extend it.
	if inner.Group != twin.AlternativeScopes[1].Group {
		t.Fatalf("inner scopes disagree: %q / %q", inner.Group, twin.AlternativeScopes[1].Group)
	}
	if inner.Branch == twin.AlternativeScopes[1].Branch {
		t.Fatal("inner competing leaves share one branch, so they would both be kept")
	}
	// The outer scope of the twin must still match the intro leaf.
	if first.AlternativeScopes[0] != intro.AlternativeScopes[0] {
		t.Fatalf("outer scope changed for the inner leaf: %+v vs %+v", first.AlternativeScopes[0], intro.AlternativeScopes[0])
	}
}

// TestFlattenNestedAlternativeKeepsMixedSiblingSegments proves the repair
// keeps the #213 fix: independent mixed leaves of the outer branch stay
// independent when a nested alternative is also present.
func TestFlattenNestedAlternativeKeepsMixedSiblingSegments(t *testing.T) {
	root := &imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
		&imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
			singlePart("plain"),
			singlePart("plain"),
			&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
				singlePart("plain"),
				singlePart("plain"),
			}},
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
	if len(plain) != 4 {
		t.Fatalf("inline plain parts = %d, want 4", len(plain))
	}
	// Two outer mixed siblings share the outer branch.
	if plain[0].AlternativeBranch != plain[1].AlternativeBranch {
		t.Fatalf("outer mixed siblings lost independence: %q / %q", plain[0].AlternativeBranch, plain[1].AlternativeBranch)
	}
	// The inner alternative's leaves share the outer branch but compete in
	// their own scope.
	if plain[2].AlternativeScopes[0] != plain[0].AlternativeScopes[0] {
		t.Fatal("inner leaf left the outer branch")
	}
	if plain[2].AlternativeScopes[1].Branch == plain[3].AlternativeScopes[1].Branch {
		t.Fatal("inner competing leaves share one branch")
	}
}

// TestFlattenNestedAlternativeInSecondBranchOfOuterMixed covers the same
// nesting inside a later mixed sibling. The outer siblings stay independent
// and the inner pair still competes.
func TestFlattenNestedAlternativeInSecondBranchOfOuterMixed(t *testing.T) {
	root := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
		singlePart("plain"),
		&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
			&imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
				singlePart("plain"),
				&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
					singlePart("plain"),
					singlePart("plain"),
				}},
			}},
			singlePart("plain"),
		}},
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
	if plain[0].AlternativeGroup != "" {
		t.Fatal("outer mixed leaf joined an alternative scope")
	}
	// Parts 2, 3, 4 are inside the middle alternative.
	if len(plain[2].AlternativeScopes) != 2 || len(plain[3].AlternativeScopes) != 2 {
		t.Fatalf("deep leaf scope count = %d / %d, want 2", len(plain[2].AlternativeScopes), len(plain[3].AlternativeScopes))
	}
	if plain[2].AlternativeScopes[1].Branch == plain[3].AlternativeScopes[1].Branch {
		t.Fatal("innermost competing leaves share one branch")
	}
	// The middle alternative's competing branch (part 4) must differ from the
	// mixed branch holding the deep pair.
	if plain[4].AlternativeScopes[0].Branch == plain[1].AlternativeScopes[0].Branch {
		t.Fatal("middle alternative branches are not distinct")
	}
}
