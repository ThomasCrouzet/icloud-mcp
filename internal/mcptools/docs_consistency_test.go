package mcptools

import (
	"os"
	"strings"
	"testing"
)

// TestDocumentedToolManifest keeps the public tool counts and names aligned
// with the exact registration manifest. The documentation is read from the
// repository, so drift fails the normal package test run.
func TestDocumentedToolManifest(t *testing.T) {
	readme := readDocFile(t, "../../README.md")
	architecture := readDocFile(t, "../../docs/architecture.md")

	all := NewCapabilityPlan(false, true, true, true, true)
	defaultPlan := NewCapabilityPlan(false, false, false, false, false)
	readOnly := NewCapabilityPlan(true, false, false, false, false)

	if got := defaultPlan.ToolCount(); got != 10 {
		t.Fatalf("default tool count = %d, want 10", got)
	}
	if got := readOnly.ToolCount(); got != 7 {
		t.Fatalf("read-only tool count = %d, want 7", got)
	}
	if got := all.ToolCount(); got != 24 {
		t.Fatalf("all-on tool count = %d, want 24", got)
	}

	readmeCounts := []string{
		"**10 tools by default**",
		"**7 with global read-only",
		"**24 with all gates on**",
	}
	for _, claim := range readmeCounts {
		if !strings.Contains(readme, claim) {
			t.Errorf("README.md omits tool count claim %q", claim)
		}
	}
	architectureCounts := []string{
		"Default registration has 10 tools",
		"Global read-only with optional domains disabled has seven tools",
		"complete surface has 24 tools",
		"at most **24** tools",
		"The default surface has 10 tools",
		"has 7 tools",
	}
	for _, claim := range architectureCounts {
		if !strings.Contains(architecture, claim) {
			t.Errorf("docs/architecture.md omits tool count claim %q", claim)
		}
	}

	// README keeps a representative table and points to the complete
	// inventory. The architecture inventory must name every registered tool.
	for _, name := range all.RegisteredTools() {
		if quoted := "`" + name + "`"; !strings.Contains(architecture, quoted) {
			t.Errorf("docs/architecture.md omits tool %q", name)
		}
	}
}

func readDocFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
