package icloud

import (
	"bytes"
	"encoding/json"
	"testing"

	ical "github.com/emersion/go-ical"
)

func TestParseVEventTextPreservesRawProperties(t *testing.T) {
	event := ical.NewEvent()
	for _, name := range []string{ical.PropSummary, ical.PropLocation, ical.PropDescription} {
		prop := ical.NewProp(name)
		prop.Params.Set(ical.ParamValue, "TeXt")
		prop.Params.Set("LANGUAGE", "en")
		prop.Value = `Raw\, text\; \\n`
		event.Props.Set(prop)
	}
	before, err := json.Marshal(event.Component)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, _, err := parseVEvent(event, "/text/calendars/home/text-event.ics")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Title != `Raw, text; \n` || parsed.Location != parsed.Title || parsed.Notes != parsed.Title {
		t.Fatalf("decoded text mismatch: %+v", parsed)
	}
	after, err := json.Marshal(event.Component)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("read changed stored properties: before=%s after=%s", before, after)
	}
}
