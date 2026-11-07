package mcptools

import (
	"reflect"
	"testing"
)

func TestContractMailIdentityPreconditions(t *testing.T) {
	for _, name := range []string{"get_message", "search_messages", "set_message_flags", "move_message", "trash_message", "reply_message"} {
		t.Run(name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
			args := f.arguments(name, "")
			args["uid_validity"] = 8
			if name == "search_messages" {
				args["before_uid"] = 3
			}
			res := f.call(t, name, args)
			if !res.IsError || contractPayload(t, res)["code"] != "concurrent_modification" {
				t.Fatalf("stale UIDVALIDITY accepted or misclassified: %s", resultText(t, res))
			}
			f.mail.mu.Lock()
			defer f.mail.mu.Unlock()
			if f.mail.imapDials != 1 || f.mail.stores+f.mail.moves+f.mail.data+f.mail.smtpDials != 0 {
				t.Fatal("identity precondition crossed mutation/send boundary")
			}
		})
	}
}

func TestContractCallerETagPreconditions(t *testing.T) {
	for _, name := range []string{"update_event", "delete_event", "update_contact", "delete_contact"} {
		t.Run(name, func(t *testing.T) {
			for _, etag := range []string{"*", `W/"v1"`, `"caller-stale"`} {
				t.Run(etag, func(t *testing.T) {
					f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
					args := f.arguments(name, "")
					args["etag"] = etag
					before := f.counts()
					res := f.call(t, name, args)
					if !res.IsError {
						t.Fatal("invalid/stale caller ETag was ignored")
					}
					if f.calendar.mutations != 0 || f.contacts.mutations != 0 {
						t.Fatal("stale ETag applied a write")
					}
					if etag != `"caller-stale"` {
						if !reflect.DeepEqual(before, f.counts()) {
							t.Fatal("illegal ETag caused a backend request")
						}
						return
					}
					attempts := 0
					for _, backend := range []*contractDAVBackend{f.calendar, f.contacts} {
						for _, req := range backend.requests {
							if req.Method == "PUT" || req.Method == "DELETE" {
								attempts++
								if req.IfMatch != etag {
									t.Fatal("caller strong ETag was not used")
								}
							}
						}
					}
					if attempts != 1 {
						t.Fatalf("conditional attempt count %d != 1", attempts)
					}
				})
			}
		})
	}
}
