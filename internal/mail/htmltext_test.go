package mail

import (
	"strings"
	"testing"
	"time"
)

func TestHTMLToTextRejectsCloseTagPrefixes(t *testing.T) {
	for _, name := range []string{"script", "style", "template", "head"} {
		input := "<" + name + ">discarded</" + name + "ure>EXPOSED</" + name + ">kept"
		text, _, err := htmlToText(input, 4096)
		if err != nil {
			t.Fatalf("%s: htmlToText() error = %v", name, err)
		}
		if strings.Contains(text, "EXPOSED") || strings.Contains(text, "discarded") {
			t.Fatalf("%s: blocked content leaked: %q", name, text)
		}
		if !strings.Contains(text, "kept") {
			t.Fatalf("%s: readable text lost: %q", name, text)
		}
	}
}

func TestHTMLToTextHandlesCaseAndQuotedDelimiters(t *testing.T) {
	input := `<SCRIPT data-x="a>b">hidden</ScRiPt >shown`
	text, _, err := htmlToText(input, 4096)
	if err != nil {
		t.Fatalf("htmlToText() error = %v", err)
	}
	if strings.Contains(text, "hidden") || !strings.Contains(text, "shown") {
		t.Fatalf("case or quoted delimiter handling = %q", text)
	}
	if _, _, err := htmlToText("<script>unterminated", 4096); err == nil {
		t.Fatal("unterminated blocked element was accepted")
	}
	if _, _, err := htmlToText("<script>a</scripture", 4096); err == nil {
		t.Fatal("malformed close tag was accepted")
	}
}

func TestHTMLToTextBlockedScanIsBounded(t *testing.T) {
	var builder strings.Builder
	for range 5_000 {
		builder.WriteString("<script>x</script>")
	}
	builder.WriteString("visible")
	started := time.Now()
	text, _, err := htmlToText(builder.String(), 4096)
	if err != nil {
		t.Fatalf("htmlToText() error = %v", err)
	}
	if !strings.Contains(text, "visible") || strings.Contains(text, "x") {
		t.Fatalf("blocked scan result = %q", text)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("blocked scan took %v", elapsed)
	}
}

func BenchmarkHTMLToTextBlockedTags(b *testing.B) {
	var builder strings.Builder
	for range 5_000 {
		builder.WriteString("<style>x</style>")
	}
	builder.WriteString("visible")
	input := builder.String()
	b.ResetTimer()
	for range b.N {
		if _, _, err := htmlToText(input, 4096); err != nil {
			b.Fatal(err)
		}
	}
}
