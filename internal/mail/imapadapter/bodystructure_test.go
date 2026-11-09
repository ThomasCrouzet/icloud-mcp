package imapadapter

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func singlePart(subtype string) *imap.BodyStructureSinglePart {
	return &imap.BodyStructureSinglePart{Type: "text", Subtype: subtype, Params: map[string]string{"charset": "utf-8"}}
}

func TestFlattenBodyStructureTracksAlternativeGroups(t *testing.T) {
	mixed := &imap.BodyStructureMultiPart{Subtype: "mixed", Children: []imap.BodyStructure{
		singlePart("plain"),
		&imap.BodyStructureMultiPart{Subtype: "alternative", Children: []imap.BodyStructure{
			singlePart("plain"),
			singlePart("plain"),
		}},
		singlePart("plain"),
	}}
	parts, err := flattenBodyStructure(mixed)
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
		t.Fatalf("inline plain parts = %d", len(plain))
	}
	if plain[0].AlternativeGroup != "" || plain[3].AlternativeGroup != "" {
		t.Errorf("independent mixed groups = %q / %q", plain[0].AlternativeGroup, plain[3].AlternativeGroup)
	}
	if plain[1].AlternativeGroup == "" || plain[1].AlternativeGroup != plain[2].AlternativeGroup {
		t.Errorf("alternative groups = %q / %q", plain[1].AlternativeGroup, plain[2].AlternativeGroup)
	}
	if plain[1].AlternativeGroup == plain[0].AlternativeGroup {
		t.Error("independent mixed part shares an alternative group")
	}
}
