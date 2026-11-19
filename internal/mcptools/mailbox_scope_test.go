package mcptools

import (
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
)

func mailboxNames(values []maildomain.Mailbox) map[string][]string {
	out := make(map[string][]string, len(values))
	for _, mailbox := range values {
		out[mailbox.Name] = mailbox.AccessModes
	}
	return out
}

// mailboxScopePlan grants every Mail capability, including the explicit
// Sent-copy archive token. The scope strings alone then decide visibility and
// modes, which is the behaviour this file pins.
func mailboxScopePlan() CapabilityPlan {
	return NewCapabilityPlanFromConfig(false, config.CapabilitySet{
		MailRead: true, MailMutate: true, MailTrash: true, MailSend: true, MailArchive: true,
	})
}

func TestFilterMailboxesSeparatesReadFromMutationScope(t *testing.T) {
	mailboxes := []maildomain.Mailbox{{Name: "INBOX"}, {Name: "Private"}, {Name: "Sent"}}
	plan := mailboxScopePlan()

	readRestricted := mailboxNames(filterMailboxes(mailboxes, mustScopes(t, "mail.read=INBOX"), plan))
	if len(readRestricted) != 1 || len(readRestricted["INBOX"]) != 3 || readRestricted["INBOX"][0] != "read" {
		t.Fatalf("restricted read listing = %#v", readRestricted)
	}
	if _, leaked := readRestricted["Private"]; leaked {
		t.Fatal("Private mailbox leaked from a read-restricted listing")
	}

	mutationUnion := mailboxNames(filterMailboxes(mailboxes, mustScopes(t, "mail.read=INBOX;mail.mutate=*"), plan))
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

	archiveUnion := mailboxNames(filterMailboxes(mailboxes, mustScopes(t, "mail.read=INBOX;mail.archive=Sent"), plan))
	if len(archiveUnion) != 2 {
		t.Fatalf("archive union = %#v", archiveUnion)
	}
	// The archive scope names only Sent. Mutate inherits the read scope because
	// no mail.mutate entry is set, so INBOX keeps read and mutate and is never
	// advertised as an archive destination.
	if modes := archiveUnion["INBOX"]; len(modes) != 2 || modes[0] != "read" || modes[1] != "mutate" {
		t.Fatalf("INBOX modes = %v, want read and mutate", modes)
	}
	if modes := archiveUnion["Sent"]; len(modes) != 1 || modes[0] != "archive" {
		t.Fatalf("Sent modes = %v, want archive only", modes)
	}

	unrestricted := mailboxNames(filterMailboxes(mailboxes, mustScopes(t, "mail.read=*"), plan))
	if len(unrestricted) != 3 {
		t.Fatalf("unrestricted listing = %#v", unrestricted)
	}
}

// TestFilterMailboxesRequiresGrantedCapability pins the effective grant half of
// the listing policy. A scope never stands in for a grant.
func TestFilterMailboxesRequiresGrantedCapability(t *testing.T) {
	mailboxes := []maildomain.Mailbox{{Name: "INBOX"}, {Name: "Private"}, {Name: "Sent"}}
	scopes := mustScopes(t, "mail.read=INBOX;mail.mutate=Private;mail.archive=Sent")

	readOnly := mailboxNames(filterMailboxes(mailboxes, scopes, mailboxAccessPlan(t, "mail.read,mail.mutate,mail.archive", true)))
	if len(readOnly) != 1 || len(readOnly["INBOX"]) != 1 || readOnly["INBOX"][0] != "read" {
		t.Fatalf("read-only listing = %#v, want only INBOX with read", readOnly)
	}

	none := mailboxNames(filterMailboxes(mailboxes, scopes, mailboxAccessPlan(t, "calendar.read", true)))
	if len(none) != 0 {
		t.Fatalf("plan without Mail grants listed %#v", none)
	}

	// A present mail.mutate scope does not inherit from mail.read, so INBOX
	// keeps read only and Private is the single mutation destination.
	writeOnly := mailboxNames(filterMailboxes(mailboxes, scopes, mailboxAccessPlan(t, "mail.read,mail.mutate", false)))
	if len(writeOnly) != 2 {
		t.Fatalf("read plus mutate listing = %#v", writeOnly)
	}
	if modes := writeOnly["INBOX"]; len(modes) != 1 || modes[0] != "read" {
		t.Fatalf("INBOX modes = %v, want read only", modes)
	}
	if modes := writeOnly["Private"]; len(modes) != 1 || modes[0] != "mutate" {
		t.Fatalf("Private modes = %v, want mutate only", modes)
	}
	if _, ok := writeOnly["Sent"]; ok {
		t.Fatalf("ungranted archive destination was listed: %#v", writeOnly)
	}
}
