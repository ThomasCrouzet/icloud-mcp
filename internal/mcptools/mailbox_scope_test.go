package mcptools

import (
	"testing"

	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

func mailboxNames(values []maildomain.Mailbox) map[string][]string {
	out := make(map[string][]string, len(values))
	for _, mailbox := range values {
		out[mailbox.Name] = mailbox.AccessModes
	}
	return out
}

func TestFilterMailboxesSeparatesReadFromMutationScope(t *testing.T) {
	mailboxes := []maildomain.Mailbox{{Name: "INBOX"}, {Name: "Private"}, {Name: "Sent"}}

	readOnly := mailboxNames(filterMailboxes(mailboxes, mustScopes(t, "mail.read=INBOX")))
	if len(readOnly) != 1 || len(readOnly["INBOX"]) != 3 || readOnly["INBOX"][0] != "read" {
		t.Fatalf("restricted read listing = %#v", readOnly)
	}
	if _, leaked := readOnly["Private"]; leaked {
		t.Fatal("Private mailbox leaked from a read-restricted listing")
	}

	mutationUnion := mailboxNames(filterMailboxes(mailboxes, mustScopes(t, "mail.read=INBOX;mail.mutate=*")))
	if len(mutationUnion) != 3 {
		t.Fatalf("mutation union = %#v", mutationUnion)
	}
	if modes := mutationUnion["INBOX"]; len(modes) != 3 {
		t.Fatalf("INBOX modes = %v, want read, mutate, archive", modes)
	}
	for _, name := range []string{"Private", "Sent"} {
		modes := mutationUnion[name]
		if len(modes) != 2 || modes[0] != "mutate" || modes[1] != "archive" {
			t.Fatalf("%s modes = %v, want mutate and archive only", name, modes)
		}
		for _, mode := range modes {
			if mode == "read" {
				t.Fatalf("%s was advertised as readable", name)
			}
		}
	}

	archiveOnly := mailboxNames(filterMailboxes(mailboxes, mustScopes(t, "mail.read=INBOX;mail.archive=Sent")))
	if len(archiveOnly) != 2 {
		t.Fatalf("archive union = %#v", archiveOnly)
	}
	if modes := archiveOnly["INBOX"]; len(modes) != 2 || modes[0] != "read" || modes[1] != "mutate" {
		t.Fatalf("INBOX modes = %v", modes)
	}
	if modes := archiveOnly["Sent"]; len(modes) != 1 || modes[0] != "archive" {
		t.Fatalf("Sent modes = %v, want archive only", modes)
	}

	unrestricted := mailboxNames(filterMailboxes(mailboxes, mustScopes(t, "mail.read=*")))
	if len(unrestricted) != 3 {
		t.Fatalf("unrestricted listing = %#v", unrestricted)
	}
}
