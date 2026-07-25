//go:build integration

// Optional integration tests against real iCloud services. Skipped by default
// (build tag `integration`), never run in CI. Calendar usage:
//
//	ICLOUD_EMAIL=... ICLOUD_PASSWORD=... go test -tags integration -count=1 -v -timeout=120s .
//
// Contacts and Mail additionally require ICLOUD_MCP_ENABLE_CONTACTS=true or
// ICLOUD_MCP_ENABLE_MAIL=true. Destructive tests also require
// ICLOUD_MCP_INTEGRATION_WRITES=true. Prefer file:// secrets (Docker-secret
// layout):
//
//	ICLOUD_EMAIL=file:///path/to/email ICLOUD_PASSWORD=file:///path/to/app-password \
//	  go test -tags integration -count=1 -v -timeout=120s .
package icloud_mcp_integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-webdav"

	"github.com/ThomasCrouzet/icloud-mcp/internal/config"
	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
	maildomain "github.com/ThomasCrouzet/icloud-mcp/internal/mail"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const (
	contactsEnabledEnv  = "ICLOUD_MCP_ENABLE_CONTACTS"
	mailEnabledEnv      = "ICLOUD_MCP_ENABLE_MAIL"
	mailWriteEnabledEnv = "ICLOUD_MCP_ENABLE_MAIL_WRITE"
	mailSendEnabledEnv  = "ICLOUD_MCP_ENABLE_MAIL_SEND"
	readOnlyEnv         = "ICLOUD_MCP_READ_ONLY"
	writesEnabledEnv    = "ICLOUD_MCP_INTEGRATION_WRITES"
	selfRecipientEnv    = "ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT"
)

func requireIntegrationEnabled(t *testing.T, envVar string) {
	t.Helper()
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envVar)), "true") {
		t.Skipf("%s=true is required for this integration test", envVar)
	}
}

func requireIntegrationDisabled(t *testing.T, envVar string) {
	t.Helper()
	if !strings.EqualFold(strings.TrimSpace(os.Getenv(envVar)), "false") {
		t.Skipf("%s=false is required for this integration test", envVar)
	}
}

func loadIntegrationConfig(t *testing.T) *config.Config {
	t.Helper()
	if strings.TrimSpace(os.Getenv("ICLOUD_EMAIL")) == "" && strings.TrimSpace(os.Getenv("ICLOUD_PASSWORD")) == "" {
		t.Skip("ICLOUD_EMAIL and ICLOUD_PASSWORD are required for integration tests")
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("invalid integration configuration: %v", err)
	}
	return cfg
}

func newIntegrationClient(t *testing.T) *icloud.Client {
	t.Helper()
	cfg := loadIntegrationConfig(t)
	httpClient := security.NewICloudHTTPClient(cfg.Timeout)
	authHTTP := webdav.HTTPClientWithBasicAuth(httpClient, cfg.Email, cfg.Password)
	doer := icloud.NewRetryClassifier(authHTTP)
	return icloud.NewClient(doer, security.ICloudBaseURL, security.IsICloudHost)
}

func newIntegrationContactsClient(t *testing.T, cfg *config.Config) *contacts.Client {
	t.Helper()
	httpClient := security.NewContactsHTTPClient(cfg.Timeout)
	authHTTP := webdav.HTTPClientWithBasicAuth(httpClient, cfg.Email, cfg.Password)
	return contacts.NewClient(authHTTP, security.ContactsBaseURL, security.IsContactsHost)
}

func newIntegrationMailClient(t *testing.T, cfg *config.Config) *maildomain.Client {
	t.Helper()
	imapDial := func(ctx context.Context) (net.Conn, error) {
		return security.DialIMAPContext(ctx, "tcp", security.IMAPAddress)
	}
	var recipientPolicy maildomain.RecipientPolicy
	var smtpDial maildomain.SMTPDialFunc
	var err error
	if cfg.EffectiveMailSend() {
		recipientPolicy, err = maildomain.ParseRecipientPolicy(strings.Join(cfg.SMTPAllowedRecipients, ","))
		if err != nil {
			t.Fatalf("parse Mail integration recipient policy: %v", err)
		}
		smtpDial = func(ctx context.Context) (net.Conn, error) {
			return security.DialSMTPContext(ctx, "tcp", security.SMTPAddress)
		}
	}
	client, err := maildomain.NewService(maildomain.Config{
		Address:         cfg.MailAddress,
		Password:        cfg.MailPassword,
		RecipientPolicy: recipientPolicy,
	}, imapDial, smtpDial, cfg.EffectiveMailWrite(), cfg.EffectiveMailSend())
	if err != nil {
		t.Fatalf("initialize Mail integration client: %v", err)
	}
	return client
}

func TestIntegration_DiscoverListSearchGet(t *testing.T) {
	client := newIntegrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := client.Discover(ctx); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	cals, err := client.ListCalendars(ctx)
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) == 0 {
		t.Fatal("no calendars returned, unexpected for a real iCloud account")
	}
	t.Logf("%d calendar(s) found", len(cals))
	for index, c := range cals {
		if c.Path == "" || !strings.HasPrefix(c.Path, "/") {
			t.Errorf("calendar[%d] path_absolute=false", index)
		}
		if strings.Contains(c.Path, "://") {
			t.Errorf("calendar[%d] absolute_url=true", index)
		}
	}

	start := time.Now().UTC().Truncate(time.Hour)
	end := start.AddDate(0, 0, 7)
	res, err := client.SearchEvents(ctx, cals[0].Path, start, end, nil)
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	t.Logf("events=%d truncated_by_expansion=%v", len(res.Events), res.TruncatedByExpansion)

	if len(res.Events) > 0 {
		uid := res.Events[0].UID
		detail, gerr := client.GetEvent(ctx, cals[0].Path, uid)
		if gerr != nil {
			t.Fatalf("GetEvent(%s): %v", uid, gerr)
		}
		if detail.UID != uid {
			t.Errorf("GetEvent UID = %q, want %q", detail.UID, uid)
		}
		t.Logf("GetEvent ok uid=%s etag_set=%v isRecurring=%v overrides=%d",
			detail.UID, detail.ETag != "", detail.IsRecurring, detail.OverrideCount)
	}
}

func TestIntegration_ContactsDiscoverListSearchGet(t *testing.T) {
	requireIntegrationEnabled(t, contactsEnabledEnv)
	cfg := loadIntegrationConfig(t)
	client := newIntegrationContactsClient(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := client.Discover(ctx); err != nil {
		t.Fatalf("Contacts Discover: %v", err)
	}
	books, err := client.ListAddressBooks(ctx)
	if err != nil {
		t.Fatalf("ListAddressBooks: %v", err)
	}
	if len(books) == 0 {
		t.Fatal("no address books returned")
	}
	t.Logf("address_books=%d", len(books))

	result, err := client.SearchContacts(ctx, contacts.SearchOptions{
		IncludeGroups: true,
		Limit:         5,
	})
	if err != nil {
		t.Fatalf("SearchContacts: %v", err)
	}
	t.Logf("contacts=%d truncated=%v scan_limit_reached=%v", len(result.Contacts), result.Truncated, result.ScanLimitReached)
	if len(result.Contacts) == 0 {
		t.Skip("GetContact requires at least one existing contact")
	}

	summary := result.Contacts[0]
	detail, err := client.GetContact(ctx, summary.AddressBook, summary.UID)
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if detail.UID != summary.UID {
		t.Errorf("GetContact returned a different opaque uid")
	}
	t.Logf("contact_get=%v etag_set=%v", detail.UID == summary.UID, detail.ETag != "")
}

func TestIntegration_ContactsCreateUpdateDelete(t *testing.T) {
	requireIntegrationEnabled(t, contactsEnabledEnv)
	requireIntegrationEnabled(t, writesEnabledEnv)
	cfg := loadIntegrationConfig(t)
	if cfg.ReadOnly {
		t.Skip("Contacts CRUD requires ICLOUD_MCP_READ_ONLY=false")
	}
	client := newIntegrationContactsClient(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	books, err := client.ListAddressBooks(ctx)
	if err != nil {
		t.Fatalf("ListAddressBooks: %v", err)
	}
	var writableBook string
	for _, book := range books {
		if book.WriteVersion == "3.0" {
			writableBook = book.Identifier
			break
		}
	}
	if writableBook == "" {
		t.Skip("Contacts CRUD requires a discovered address book with vCard 3.0 write support")
	}

	displayName := integrationNonce(t)
	structuredName := contacts.StructuredName{
		FamilyName:     integrationNonce(t),
		GivenName:      integrationNonce(t),
		AdditionalName: integrationNonce(t),
	}
	email := integrationNonce(t) + "@example.invalid"
	phoneDigits := "9" + integrationDigits(t, 17)
	phone := "+" + phoneDigits[:3] + " " + phoneDigits[3:9] + " " + phoneDigits[9:]
	organization := integrationNonce(t)
	created, err := client.CreateContact(ctx, &contacts.CreateContactInput{
		AddressBook:  writableBook,
		DisplayName:  displayName,
		Name:         structuredName,
		Organization: organization,
		Emails:       []contacts.TypedValue{{Type: "work", Value: email}},
		Phones:       []contacts.TypedValue{{Type: "work", Value: phone}},
		ClientUID:    integrationNonce(t),
	})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	cleanupNeeded := true
	defer func() {
		if !cleanupNeeded {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		deleted, cleanupErr := client.DeleteContact(cleanupCtx, &contacts.DeleteContactInput{
			AddressBook: created.AddressBook,
			UID:         created.UID,
		})
		if cleanupErr != nil {
			t.Errorf("Contact cleanup completed=false: %v", cleanupErr)
			return
		}
		cleanupNeeded = false
		if !deleted.WouldDelete {
			t.Error("Contact cleanup would_delete=false")
			return
		}
		t.Log("contact_cleanup=true")
	}()

	searchChecks := []struct {
		name    string
		options contacts.SearchOptions
	}{
		{name: "fn_query", options: contacts.SearchOptions{Query: displayName}},
		{name: "n_query", options: contacts.SearchOptions{Query: structuredName.FamilyName}},
		{name: "email_query", options: contacts.SearchOptions{Query: email}},
		{name: "org_query", options: contacts.SearchOptions{Query: organization}},
		{name: "exact_email", options: contacts.SearchOptions{Email: email}},
		{name: "normalized_phone", options: contacts.SearchOptions{Phone: phoneDigits}},
	}
	for _, check := range searchChecks {
		check.options.AddressBook = created.AddressBook
		check.options.Limit = 10
		result, searchErr := client.SearchContacts(ctx, check.options)
		if searchErr != nil {
			t.Fatalf("SearchContacts %s: %v", check.name, searchErr)
		}
		matched := contactResultContains(result, created.AddressBook, created.UID)
		t.Logf("contact_search_%s_count=%d matched=%v", check.name, len(result.Contacts), matched)
		if !matched {
			t.Fatalf("contact_search_%s matched=false", check.name)
		}
	}

	beforeUpdate, err := client.GetContact(ctx, created.AddressBook, created.UID)
	if err != nil {
		t.Fatalf("GetContact before update/delete: %v", err)
	}
	fieldsMatch := beforeUpdate.UID == created.UID &&
		beforeUpdate.DisplayName == displayName &&
		beforeUpdate.Name != nil && *beforeUpdate.Name == structuredName &&
		beforeUpdate.Organization == organization &&
		hasContactValue(beforeUpdate.Emails, email) &&
		hasContactValue(beforeUpdate.Phones, phone)
	t.Logf("contact_uid_get=%v fixture_fields_match=%v", beforeUpdate.UID == created.UID, fieldsMatch)
	if !fieldsMatch {
		t.Fatal("created contact fixture_fields_match=false")
	}

	updatedName := integrationNonce(t)
	updated, err := client.UpdateContact(ctx, &contacts.UpdateContactInput{
		AddressBook: created.AddressBook,
		UID:         created.UID,
		ETag:        created.ETag,
		Patch:       contacts.ContactPatch{DisplayName: &updatedName},
	})
	if err != nil {
		t.Fatalf("UpdateContact: %v", err)
	}
	detail, err := client.GetContact(ctx, created.AddressBook, created.UID)
	if err != nil {
		t.Fatalf("GetContact after update: %v", err)
	}
	if detail.DisplayName != updatedName {
		t.Error("updated display_name_match=false")
	}
	t.Logf("contacts_create_incomplete=%v update_incomplete=%v display_name_match=%v", created.ResultIncomplete, updated.ResultIncomplete, detail.DisplayName == updatedName)
}

func TestIntegration_MailListSearchGetMetadata(t *testing.T) {
	requireIntegrationEnabled(t, mailEnabledEnv)
	cfg := loadIntegrationConfig(t)
	client := newIntegrationMailClient(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	listed, err := client.ListMailboxes(ctx)
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	t.Logf("mailboxes=%d truncated=%v", len(listed.Mailboxes), listed.Truncated)
	candidates := selectableMailboxes(listed.Mailboxes, 4)
	if len(candidates) == 0 {
		t.Skip("Mail search requires a selectable mailbox")
	}

	var result maildomain.SearchResult
	var mailbox string
	for _, candidate := range candidates {
		result, err = client.SearchMessages(ctx, maildomain.SearchInput{Mailbox: candidate, Limit: 2})
		if err != nil {
			t.Fatalf("SearchMessages: %v", err)
		}
		if len(result.Messages) > 0 {
			mailbox = candidate
			break
		}
	}
	t.Logf("messages=%d truncated=%v scan_limit_reached=%v", len(result.Messages), result.Truncated, result.ScanLimitReached)
	if mailbox == "" {
		t.Skip("GetMessage requires at least one message in the checked mailboxes")
	}

	summary := result.Messages[0]
	seenBefore := hasMailFlag(summary.Flags, maildomain.FlagSeen)
	detail, err := client.GetMessage(ctx, maildomain.GetMessageInput{
		Mailbox:     mailbox,
		UIDValidity: summary.UIDValidity,
		UID:         summary.UID,
	})
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	after, err := client.GetMessage(ctx, maildomain.GetMessageInput{
		Mailbox:     mailbox,
		UIDValidity: summary.UIDValidity,
		UID:         summary.UID,
	})
	if err != nil {
		t.Fatalf("GetMessage verification: %v", err)
	}
	seenUnchanged := seenBefore == hasMailFlag(detail.Flags, maildomain.FlagSeen) &&
		seenBefore == hasMailFlag(after.Flags, maildomain.FlagSeen)
	if !seenUnchanged {
		t.Error("GetMessage changed Seen state")
	}
	t.Logf("mail_get=true uidvalidity_set=%v seen_unchanged=%v body_omitted=%v", summary.UIDValidity != 0, seenUnchanged, detail.BodyOmitted)
}

func TestIntegration_MailMutationAndSend(t *testing.T) {
	requireIntegrationEnabled(t, mailEnabledEnv)
	requireIntegrationEnabled(t, mailWriteEnabledEnv)
	requireIntegrationEnabled(t, mailSendEnabledEnv)
	requireIntegrationDisabled(t, readOnlyEnv)
	requireIntegrationEnabled(t, writesEnabledEnv)
	selfRecipient := os.Getenv(selfRecipientEnv)
	if selfRecipient == "" {
		t.Skipf("%s must explicitly name the self recipient; SMTP is never attempted without it", selfRecipientEnv)
	}
	cfg := loadIntegrationConfig(t)
	if selfRecipient != cfg.MailAddress {
		t.Skipf("%s must exactly match the configured Mail address", selfRecipientEnv)
	}
	if cfg.SMTPRecipientPolicy.AllowAll() {
		t.Skip("Mail mutation/send requires an exact self-recipient allowlist, not wildcard policy")
	}
	if !cfg.EffectiveMailWrite() || !cfg.EffectiveMailSend() || !cfg.SMTPRecipientPolicy.Allows(selfRecipient) {
		t.Skip("Mail mutation/send requires enabled Mail write and send configuration with the self recipient allowed")
	}

	client := newIntegrationMailClient(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	listed, err := client.ListMailboxes(ctx)
	if err != nil {
		t.Fatalf("ListMailboxes before SMTP: %v", err)
	}
	if listed.Truncated {
		t.Skip("SMTP is not attempted when the mailbox list is truncated")
	}
	mailboxes := selectableMailboxes(listed.Mailboxes, maildomain.MaxMailboxes)
	if len(mailboxes) == 0 {
		t.Skip("SMTP is not attempted without a selectable mailbox")
	}
	trashMailboxes := specialUseMailboxes(listed.Mailboxes, "\\Trash")
	if len(trashMailboxes) != 1 {
		t.Skip("SMTP is not attempted without exactly one selectable SPECIAL-USE Trash mailbox")
	}
	trashMailbox := trashMailboxes[0]
	mailboxes = mailboxLast(mailboxes, trashMailbox)

	subject := integrationNonce(t)
	query := integrationNonce(t)
	body := integrationNonce(t) + "\n" + query
	sentAfter := time.Now().UTC().Add(-5 * time.Minute)
	var lastMutation time.Time
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cleanupCancel()
		found, moved, cleanupErr := cleanupMailFixture(
			cleanupCtx, client, mailboxes, trashMailbox, subject, query, sentAfter, &lastMutation,
		)
		complete := cleanupErr == nil
		t.Logf("mail_cleanup_found=%d moved=%d complete=%v", found, moved, complete)
		if cleanupErr != nil {
			t.Errorf("Mail fixture cleanup complete=false: %v", cleanupErr)
		}
	}()

	sent, err := client.SendMessage(ctx, maildomain.SendInput{
		To:      []string{selfRecipient},
		Subject: subject,
		Body:    body,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	outcomeOK := sent.Status == maildomain.SendAccepted &&
		sent.MessageID != "" &&
		sent.SentCopyUnavailable &&
		sent.Reconciliation == "" &&
		len(sent.Recipients) == 1 &&
		sent.Recipients[0].Index == 0 &&
		sent.Recipients[0].Accepted &&
		sent.Recipients[0].Category == ""
	t.Logf("smtp_accepted=%v recipients=%d outcome_model=%v", sent.Status == maildomain.SendAccepted, len(sent.Recipients), outcomeOK)
	if !outcomeOK {
		t.Fatal("SMTP accepted outcome_model=false")
	}

	current, currentMailbox, matches, err := pollForMailFixture(ctx, client, mailboxes, subject, query, sentAfter, 90*time.Second)
	if err != nil {
		t.Fatalf("poll for submitted Mail fixture: %v", err)
	}
	detail, err := client.GetMessage(ctx, maildomain.GetMessageInput{
		Mailbox: currentMailbox, UIDValidity: current.UIDValidity, UID: current.UID,
	})
	if err != nil {
		t.Fatalf("GetMessage for submitted Mail fixture: %v", err)
	}
	seenState := hasMailFlag(current.Flags, maildomain.FlagSeen)
	seenUnchanged := seenState == hasMailFlag(detail.Flags, maildomain.FlagSeen)
	deliveryOK := current.UIDValidity != 0 && detail.UIDValidity == current.UIDValidity &&
		detail.UID == current.UID && detail.Subject == subject && strings.Contains(detail.Body, query)
	t.Logf("mail_delivery_found=%v matches=%d uidvalidity_set=%v seen_unchanged=%v", deliveryOK, matches, current.UIDValidity != 0, seenUnchanged)
	if !deliveryOK || !seenUnchanged {
		t.Fatal("submitted Mail fixture verification failed")
	}
	current = detail.MessageSummary

	t.Run("set_flags", func(t *testing.T) {
		flag, available := absentMutableFlag(current.Flags)
		if !available {
			t.Skip("no absent safe mutable flag is available on the fixture")
		}
		expectedModSeq := current.ModSeq
		if expectedModSeq == 0 {
			expectedModSeq = 1
		}
		if err := waitForMailMutation(ctx, &lastMutation); err != nil {
			t.Fatalf("wait for flag mutation slot: %v", err)
		}
		added, flagErr := client.SetMessageFlags(ctx, maildomain.SetFlagsInput{
			Mailbox: currentMailbox, UIDValidity: current.UIDValidity, UID: current.UID,
			Operation: maildomain.FlagOperationAdd, Flags: []maildomain.MessageFlag{flag}, ExpectedModSeq: expectedModSeq,
		})
		if flagErr != nil {
			if !deliberateCondStoreFailure(flagErr) {
				t.Fatalf("SetMessageFlags add: %v", flagErr)
			}
			after, getErr := client.GetMessage(ctx, maildomain.GetMessageInput{
				Mailbox: currentMailbox, UIDValidity: current.UIDValidity, UID: current.UID,
			})
			if getErr != nil {
				t.Fatalf("GetMessage after fail-closed flag mutation: %v", getErr)
			}
			unchanged := sameMailFlags(current.Flags, after.Flags) && seenState == hasMailFlag(after.Flags, maildomain.FlagSeen)
			t.Logf("mail_flag_fail_closed=%v", unchanged)
			if !unchanged {
				t.Fatal("CONDSTORE flag mutation fail_closed=false")
			}
			current = after.MessageSummary
			return
		}
		if added.UIDValidity != current.UIDValidity || added.UID != current.UID {
			t.Fatal("flag add UIDVALIDITY identity_match=false")
		}
		afterAdd, getErr := client.GetMessage(ctx, maildomain.GetMessageInput{
			Mailbox: currentMailbox, UIDValidity: current.UIDValidity, UID: current.UID,
		})
		if getErr != nil {
			t.Fatalf("GetMessage after flag add: %v", getErr)
		}
		addOK := hasMailFlag(afterAdd.Flags, flag) && seenState == hasMailFlag(afterAdd.Flags, maildomain.FlagSeen)
		if !addOK {
			t.Fatal("flag add or Seen preservation failed")
		}

		if err := waitForMailMutation(ctx, &lastMutation); err != nil {
			t.Fatalf("wait for flag removal slot: %v", err)
		}
		removed, removeErr := client.SetMessageFlags(ctx, maildomain.SetFlagsInput{
			Mailbox: currentMailbox, UIDValidity: current.UIDValidity, UID: current.UID,
			Operation: maildomain.FlagOperationRemove, Flags: []maildomain.MessageFlag{flag}, ExpectedModSeq: afterAdd.ModSeq,
		})
		if removeErr != nil {
			t.Fatalf("SetMessageFlags remove: %v", removeErr)
		}
		if removed.UIDValidity != current.UIDValidity || removed.UID != current.UID {
			t.Fatal("flag remove UIDVALIDITY identity_match=false")
		}
		afterRemove, getErr := client.GetMessage(ctx, maildomain.GetMessageInput{
			Mailbox: currentMailbox, UIDValidity: current.UIDValidity, UID: current.UID,
		})
		if getErr != nil {
			t.Fatalf("GetMessage after flag remove: %v", getErr)
		}
		removeOK := !hasMailFlag(afterRemove.Flags, flag) && seenState == hasMailFlag(afterRemove.Flags, maildomain.FlagSeen)
		t.Logf("mail_flag_add=%v remove=%v seen_unchanged=%v", addOK, removeOK, seenState == hasMailFlag(afterRemove.Flags, maildomain.FlagSeen))
		if !removeOK {
			t.Fatal("flag remove or Seen preservation failed")
		}
		current = afterRemove.MessageSummary
	})

	t.Run("move", func(t *testing.T) {
		destination, available := mailMoveDestination(listed.Mailboxes, currentMailbox, trashMailbox)
		if !available {
			t.Skip("no distinct selectable destination mailbox is available")
		}
		if err := waitForMailMutation(ctx, &lastMutation); err != nil {
			t.Fatalf("wait for move mutation slot: %v", err)
		}
		moved, moveErr := client.MoveMessage(ctx, maildomain.MoveInput{
			Mailbox: currentMailbox, UIDValidity: current.UIDValidity, UID: current.UID, Destination: destination,
		})
		if moveErr != nil {
			if unavailableSafeMove(moveErr) {
				t.Skip("server lacks native MOVE and the safe UIDPLUS fallback")
			}
			t.Fatalf("MoveMessage: %v", moveErr)
		}
		methodOK := validMoveMethod(moved.Method)
		identityOK := moved.Mailbox == currentMailbox && moved.UIDValidity == current.UIDValidity &&
			moved.UID == current.UID && moved.Destination == destination
		destinationIdentityOK := moved.DestinationUIDValidity == 0 && moved.DestinationUID == 0 ||
			moved.DestinationUIDValidity != 0 && moved.DestinationUID != 0
		if !methodOK || !identityOK || !destinationIdentityOK {
			t.Fatal("move method or UIDVALIDITY identity_match=false")
		}
		refound, mailbox, count, findErr := pollForMailFixture(ctx, client, []string{destination}, subject, query, sentAfter, 30*time.Second)
		if findErr != nil {
			t.Fatalf("re-find moved Mail fixture: %v", findErr)
		}
		if moved.DestinationUIDValidity != 0 && moved.DestinationUIDValidity != refound.UIDValidity ||
			moved.DestinationUID != 0 && moved.DestinationUID != refound.UID {
			t.Fatal("move destination UIDVALIDITY identity_match=false")
		}
		afterMove, getErr := client.GetMessage(ctx, maildomain.GetMessageInput{
			Mailbox: mailbox, UIDValidity: refound.UIDValidity, UID: refound.UID,
		})
		if getErr != nil {
			t.Fatalf("GetMessage after move: %v", getErr)
		}
		seenOK := seenState == hasMailFlag(afterMove.Flags, maildomain.FlagSeen)
		t.Logf("mail_move_method_valid=%v refound_count=%d uidvalidity_set=%v seen_unchanged=%v", methodOK, count, refound.UIDValidity != 0, seenOK)
		if !seenOK {
			t.Fatal("move Seen preservation failed")
		}
		current = afterMove.MessageSummary
		currentMailbox = mailbox
	})

	t.Run("trash", func(t *testing.T) {
		if currentMailbox == trashMailbox {
			t.Log("mail_already_in_trash=true")
			return
		}
		if err := waitForMailMutation(ctx, &lastMutation); err != nil {
			t.Fatalf("wait for trash mutation slot: %v", err)
		}
		trashed, trashErr := client.TrashMessage(ctx, maildomain.TrashInput{
			Mailbox: currentMailbox, UIDValidity: current.UIDValidity, UID: current.UID,
		})
		if trashErr != nil {
			if unavailableSafeMove(trashErr) {
				t.Skip("server lacks native MOVE and the safe UIDPLUS fallback")
			}
			t.Fatalf("TrashMessage: %v", trashErr)
		}
		methodOK := validMoveMethod(trashed.Method)
		identityOK := trashed.Mailbox == currentMailbox && trashed.UIDValidity == current.UIDValidity &&
			trashed.UID == current.UID && trashed.Destination == trashMailbox
		destinationIdentityOK := trashed.DestinationUIDValidity == 0 && trashed.DestinationUID == 0 ||
			trashed.DestinationUIDValidity != 0 && trashed.DestinationUID != 0
		if !methodOK || !identityOK || !destinationIdentityOK {
			t.Fatal("trash method or UIDVALIDITY identity_match=false")
		}
		refound, mailbox, count, findErr := pollForMailFixture(ctx, client, []string{trashMailbox}, subject, query, sentAfter, 30*time.Second)
		if findErr != nil {
			t.Fatalf("re-find trashed Mail fixture: %v", findErr)
		}
		if trashed.DestinationUIDValidity != 0 && trashed.DestinationUIDValidity != refound.UIDValidity ||
			trashed.DestinationUID != 0 && trashed.DestinationUID != refound.UID {
			t.Fatal("trash destination UIDVALIDITY identity_match=false")
		}
		afterTrash, getErr := client.GetMessage(ctx, maildomain.GetMessageInput{
			Mailbox: mailbox, UIDValidity: refound.UIDValidity, UID: refound.UID,
		})
		if getErr != nil {
			t.Fatalf("GetMessage after trash: %v", getErr)
		}
		seenOK := seenState == hasMailFlag(afterTrash.Flags, maildomain.FlagSeen)
		t.Logf("mail_trash_method_valid=%v refound_count=%d uidvalidity_set=%v seen_unchanged=%v", methodOK, count, refound.UIDValidity != 0, seenOK)
		if !seenOK {
			t.Fatal("trash Seen preservation failed")
		}
		current = afterTrash.MessageSummary
		currentMailbox = mailbox
	})
}

func integrationNonce(t *testing.T) string {
	t.Helper()
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate opaque integration fixture id: %v", err)
	}
	return hex.EncodeToString(value[:])
}

func integrationDigits(t *testing.T, count int) string {
	t.Helper()
	value := make([]byte, count)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("generate opaque integration digits: %v", err)
	}
	for index := range value {
		value[index] = '0' + value[index]%10
	}
	return string(value)
}

func contactResultContains(result contacts.SearchResult, addressBook, uid string) bool {
	for _, contact := range result.Contacts {
		if contact.AddressBook == addressBook && contact.UID == uid {
			return true
		}
	}
	return false
}

func hasContactValue(values []contacts.TypedValue, wanted string) bool {
	for _, value := range values {
		if value.Value == wanted {
			return true
		}
	}
	return false
}

func pollForMailFixture(
	ctx context.Context,
	client *maildomain.Client,
	mailboxes []string,
	subject, query string,
	since time.Time,
	maxWait time.Duration,
) (maildomain.MessageSummary, string, int, error) {
	pollCtx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	for {
		for _, mailbox := range mailboxes {
			result, err := searchMailFixture(pollCtx, client, mailbox, subject, query, since)
			if err != nil {
				return maildomain.MessageSummary{}, "", 0, err
			}
			matches := 0
			var selected maildomain.MessageSummary
			for _, message := range result.Messages {
				if message.Subject != subject {
					continue
				}
				matches++
				if selected.UID == 0 {
					selected = message
				}
			}
			if selected.UID != 0 {
				return selected, mailbox, matches, nil
			}
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			return maildomain.MessageSummary{}, "", 0, errors.New("mail fixture was not found before the bounded poll deadline")
		case <-timer.C:
		}
	}
}

func searchMailFixture(
	ctx context.Context,
	client *maildomain.Client,
	mailbox, subject, query string,
	since time.Time,
) (maildomain.SearchResult, error) {
	for {
		result, err := client.SearchMessages(ctx, maildomain.SearchInput{
			Mailbox: mailbox,
			Subject: subject,
			Query:   query,
			Since:   since,
			Limit:   20,
		})
		if err != nil {
			mailErr := maildomain.AsError(err)
			if mailErr == nil || mailErr.Code != maildomain.CodeRateLimited || mailErr.Message != "local mail read rate limit reached" {
				return maildomain.SearchResult{}, err
			}
			timer := time.NewTimer(1100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return maildomain.SearchResult{}, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if result.UIDValidity == 0 {
			return maildomain.SearchResult{}, errors.New("mail fixture search returned zero UIDVALIDITY")
		}
		for _, message := range result.Messages {
			if message.UID == 0 || message.UIDValidity != result.UIDValidity {
				return maildomain.SearchResult{}, errors.New("mail fixture search returned an invalid UIDVALIDITY identity")
			}
		}
		return result, nil
	}
}

func cleanupMailFixture(
	ctx context.Context,
	client *maildomain.Client,
	mailboxes []string,
	trashMailbox, subject, query string,
	since time.Time,
	lastMutation *time.Time,
) (int, int, error) {
	found := 0
	moved := 0
	quietRounds := 0
	for quietRounds < 2 {
		outsideTrash := 0
		for _, mailbox := range mailboxes {
			result, err := searchMailFixture(ctx, client, mailbox, subject, query, since)
			if err != nil {
				return found, moved, err
			}
			if result.Truncated {
				return found, moved, errors.New("mail fixture cleanup search was truncated")
			}
			for _, message := range result.Messages {
				if message.Subject != subject {
					continue
				}
				found++
				if mailbox == trashMailbox {
					continue
				}
				outsideTrash++
				if err := waitForMailMutation(ctx, lastMutation); err != nil {
					return found, moved, err
				}
				result, err := client.TrashMessage(ctx, maildomain.TrashInput{
					Mailbox: mailbox, UIDValidity: message.UIDValidity, UID: message.UID,
				})
				if err != nil {
					return found, moved, err
				}
				if result.UIDValidity != message.UIDValidity || result.UID != message.UID ||
					result.Destination != trashMailbox || !validMoveMethod(result.Method) {
					return found, moved, errors.New("mail fixture cleanup returned an invalid move identity")
				}
				moved++
			}
		}
		if outsideTrash == 0 {
			quietRounds++
		} else {
			quietRounds = 0
		}
		if quietRounds == 2 {
			break
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return found, moved, ctx.Err()
		case <-timer.C:
		}
	}
	return found, moved, nil
}

func waitForMailMutation(ctx context.Context, lastMutation *time.Time) error {
	if !lastMutation.IsZero() {
		wait := time.Until(lastMutation.Add(3200 * time.Millisecond))
		if wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	*lastMutation = time.Now()
	return nil
}

func absentMutableFlag(flags []maildomain.MessageFlag) (maildomain.MessageFlag, bool) {
	for _, flag := range []maildomain.MessageFlag{maildomain.FlagFlagged, maildomain.FlagAnswered} {
		if !hasMailFlag(flags, flag) {
			return flag, true
		}
	}
	return "", false
}

func sameMailFlags(left, right []maildomain.MessageFlag) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func deliberateCondStoreFailure(err error) bool {
	mailErr := maildomain.AsError(err)
	if mailErr == nil || mailErr.Code != maildomain.CodeProtocolError {
		return false
	}
	return mailErr.Message == "conditional flag mutation is unavailable because MODIFIED cannot be safely detected" ||
		mailErr.Message == "conditional flag mutation is not implemented by this adapter"
}

func unavailableSafeMove(err error) bool {
	mailErr := maildomain.AsError(err)
	return mailErr != nil && mailErr.Code == maildomain.CodeProtocolError &&
		mailErr.Message == "server supports neither native MOVE nor the safe UIDPLUS fallback"
}

func validMoveMethod(method string) bool {
	return method == "move" || method == "uidplus_fallback"
}

func specialUseMailboxes(mailboxes []maildomain.Mailbox, attribute string) []string {
	var result []string
	for _, mailbox := range mailboxes {
		if mailboxSelectable(mailbox) && mailboxHasAttribute(mailbox, attribute) {
			result = append(result, mailbox.Name)
		}
	}
	return result
}

func mailMoveDestination(mailboxes []maildomain.Mailbox, source, trash string) (string, bool) {
	counts := make(map[string]int)
	for _, mailbox := range mailboxes {
		if mailboxSelectable(mailbox) {
			counts[mailbox.Name]++
		}
	}
	for _, preferArchive := range []bool{true, false} {
		for _, mailbox := range mailboxes {
			if !mailboxSelectable(mailbox) || mailbox.Name == source || mailbox.Name == trash || counts[mailbox.Name] != 1 {
				continue
			}
			if mailboxHasAttribute(mailbox, "\\Archive") == preferArchive {
				return mailbox.Name, true
			}
		}
	}
	return "", false
}

func mailboxLast(mailboxes []string, wanted string) []string {
	result := make([]string, 0, len(mailboxes))
	found := false
	for _, mailbox := range mailboxes {
		if mailbox == wanted {
			found = true
			continue
		}
		result = append(result, mailbox)
	}
	if found {
		result = append(result, wanted)
	}
	return result
}

func selectableMailboxes(mailboxes []maildomain.Mailbox, limit int) []string {
	result := make([]string, 0, limit)
	seen := make(map[string]struct{})
	for _, preferInbox := range []bool{true, false} {
		for _, mailbox := range mailboxes {
			if len(result) == limit {
				return result
			}
			isInbox := strings.EqualFold(mailbox.Name, "INBOX")
			if isInbox != preferInbox || !mailboxSelectable(mailbox) {
				continue
			}
			if _, duplicate := seen[mailbox.Name]; duplicate {
				continue
			}
			seen[mailbox.Name] = struct{}{}
			result = append(result, mailbox.Name)
		}
	}
	return result
}

func mailboxSelectable(mailbox maildomain.Mailbox) bool {
	for _, attribute := range mailbox.Attributes {
		if strings.EqualFold(attribute, "\\Noselect") || strings.EqualFold(attribute, "\\NonExistent") {
			return false
		}
	}
	return true
}

func mailboxHasAttribute(mailbox maildomain.Mailbox, wanted string) bool {
	for _, attribute := range mailbox.Attributes {
		if strings.EqualFold(attribute, wanted) {
			return true
		}
	}
	return false
}

func hasMailFlag(flags []maildomain.MessageFlag, wanted maildomain.MessageFlag) bool {
	for _, flag := range flags {
		if flag == wanted {
			return true
		}
	}
	return false
}

func TestIntegration_ValidateAndFreeSlotsLocal(t *testing.T) {
	start := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	res := icloud.ValidateEventInput(&icloud.EventInput{
		Title:     "integration-validate",
		StartTime: start,
		EndTime:   end,
		Status:    "CONFIRMED",
	}, time.UTC)
	if !res.OK {
		t.Fatalf("ValidateEventInput: %+v", res.Errors)
	}
	busy := []icloud.Interval{{Start: start.Add(2 * time.Hour), End: start.Add(3 * time.Hour)}}
	slots, err := icloud.FindFreeSlots(busy, icloud.FreeSlotOptions{
		RangeStart: start,
		RangeEnd:   start.Add(8 * time.Hour),
		Duration:   time.Hour,
	})
	if err != nil {
		t.Fatalf("FindFreeSlots: %v", err)
	}
	if len(slots) == 0 {
		t.Fatal("expected at least one free slot")
	}
}
