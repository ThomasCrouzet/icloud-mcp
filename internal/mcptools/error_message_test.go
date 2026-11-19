package mcptools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// TestErrorMessageDoesNotRepeatTypedCode pins the NEW-2 helper contract. A typed
// *icloud.Error already prints its code in Error(), so a matching context must
// not add a second copy. Every other case keeps the context prefix.
func TestErrorMessageDoesNotRepeatTypedCode(t *testing.T) {
	for _, test := range []struct {
		name    string
		context string
		err     error
		want    string
	}{
		{
			name: "typed validation with validation context", context: "validation",
			err:  icloud.NewValidationError("start does not round-trip"),
			want: "validation: start does not round-trip",
		},
		{
			name: "typed validation with other context", context: "creating event",
			err:  icloud.NewValidationError("start does not round-trip"),
			want: "creating event: validation: start does not round-trip",
		},
		{
			name: "typed conflict with matching context", context: "conflict",
			err:  icloud.NewError(icloud.CodeConflict, 409, "event already exists", nil),
			want: "conflict: event already exists",
		},
		{
			name: "plain error keeps the context", context: "validation",
			err:  fmt.Errorf("plain failure"),
			want: "validation: plain failure",
		},
		{
			name: "wrapped typed error", context: "updating event",
			err:  fmt.Errorf("updating: %w", icloud.NewValidationError("bad form")),
			want: "updating event: updating: validation: bad form",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := errorMessage(test.context, test.err)
			if got != test.want {
				t.Fatalf("message = %q, want %q", got, test.want)
			}
			if strings.Count(got, "validation: validation:") != 0 {
				t.Fatalf("message repeats the code prefix: %q", got)
			}
		})
	}
}
