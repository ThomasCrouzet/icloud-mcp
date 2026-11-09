package mcptools

import (
	"testing"
)

// The pinned go-imap beta.8 cannot expose the tagged MODIFIED code. A guarded
// STORE therefore cannot be proven, even when an unsolicited FETCH happens to
// show the desired flags. The adapter and this contract fail closed before any
// mutation. #205 owns this guarantee; #150's postcondition check remains for
// the direct validator and the non-CONDSTORE path (#208).
func TestContractRealConditionalStore(t *testing.T) {
	for _, name := range []string{"with-precondition", "missing-precondition", "malformed-return"} {
		t.Run(name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
				f.mail.condstore = true
				f.mail.malformedStore = name == "malformed-return"
				f.mail.modseq = 42
			})
			args := f.arguments("set_message_flags", "")
			if name != "missing-precondition" {
				args["expected_modseq"] = 42
			}
			result := f.call(t, "set_message_flags", args)
			f.mail.mu.Lock()
			stores := f.mail.stores
			f.mail.mu.Unlock()
			if stores != 0 {
				t.Fatalf("unprovable conditional STORE was dispatched %d time(s)", stores)
			}
			if !result.IsError {
				t.Fatal("unprovable conditional STORE reported success")
			}
		})
	}
}

func TestContractFullRangeConditionalStore(t *testing.T) {
	for _, token := range []string{"18446744073709551615", "184467440737095516150e-1", "18446744073709551614"} {
		t.Run(token, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)}, func(f *contractRuntime) {
				f.mail.condstore = true
				f.mail.modseq = 42
			})
			args := f.arguments("set_message_flags", "")
			args["expected_modseq"] = token
			result := f.call(t, "set_message_flags", args)
			f.mail.mu.Lock()
			stores := f.mail.stores
			f.mail.mu.Unlock()
			if stores != 0 || !result.IsError {
				t.Fatalf("unprovable conditional STORE was not refused: stores=%d error=%v", stores, result.IsError)
			}
		})
	}
}
