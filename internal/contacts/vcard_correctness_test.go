package contacts

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type multiCardFixture struct {
	mu      sync.Mutex
	cards   map[string]string
	records []requestRecord
}

func (fixture *multiCardFixture) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.records = append(fixture.records, requestRecord{
		method: req.Method, path: req.URL.Path, escapedPath: req.URL.EscapedPath(),
		requestURI: req.URL.RequestURI(), header: req.Header.Clone(), body: string(body),
	})
	switch {
	case req.Method == "PROPFIND" && req.URL.Path == "/":
		return httpResponse(req, http.StatusMultiStatus, principalXML("/principal/"), nil), nil
	case req.Method == "PROPFIND" && req.URL.Path == "/principal/":
		return httpResponse(req, http.StatusMultiStatus, homesXML("/home/"), nil), nil
	case req.Method == "PROPFIND" && req.URL.Path == "/home/":
		return httpResponse(req, http.StatusMultiStatus, booksXML("/home/book/"), nil), nil
	case req.Method == "REPORT":
		return httpResponse(req, http.StatusMultiStatus, multiCardResponseXML(fixture.cards), nil), nil
	case req.Method == http.MethodGet:
		card, ok := fixture.cards[req.URL.Path]
		if !ok {
			return httpResponse(req, http.StatusNotFound, "", nil), nil
		}
		return httpResponse(req, http.StatusOK, card, map[string]string{"Content-Type": "text/vcard", "ETag": `"e"`}), nil
	case req.Method == http.MethodPut:
		fixture.cards[req.URL.Path] = string(body)
		return httpResponse(req, http.StatusNoContent, "", nil), nil
	case req.Method == http.MethodDelete:
		delete(fixture.cards, req.URL.Path)
		return httpResponse(req, http.StatusNoContent, "", nil), nil
	default:
		return httpResponse(req, http.StatusNotFound, "", nil), nil
	}
}

func (fixture *multiCardFixture) recordsFor(method string) []requestRecord {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	var records []requestRecord
	for _, record := range fixture.records {
		if record.method == method {
			records = append(records, record)
		}
	}
	return records
}

func escapedStructuredCard() string {
	return "BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"UID:escaped-contact\r\n" +
		"FN:Jane Doe-Smith\r\n" +
		"N:Doe\\;Smith;Jane;;;\r\n" +
		"ORG:Acme\\;Labs;Research\r\n" +
		"ADR:;;123 A\\;B Street;Paris;;;France\r\n" +
		"END:VCARD\r\n"
}

func TestEscapedStructuredValuesAreNotSplit(t *testing.T) {
	card, version, raw, err := decodeRawCard([]byte(escapedStructuredCard()))
	if err != nil {
		t.Fatalf("decodeRawCard() error = %v", err)
	}
	detail := cardDetail(newCardView(card, raw), version, "book", `"e"`)
	if detail.Name == nil {
		t.Fatal("expected a structured name")
	}
	if detail.Name.FamilyName != "Doe;Smith" || detail.Name.GivenName != "Jane" {
		t.Fatalf("name = %#v", detail.Name)
	}
	if detail.Organization != "Acme;Labs" {
		t.Fatalf("organization = %q", detail.Organization)
	}
	if len(detail.Addresses) != 1 {
		t.Fatalf("addresses = %#v", detail.Addresses)
	}
	address := detail.Addresses[0]
	if address.StreetAddress != "123 A;B Street" || address.Locality != "Paris" || address.Country != "France" {
		t.Fatalf("address = %#v", address)
	}
	if matched, _ := contactMatches(card, raw, SearchOptions{Query: "Doe;Smith"}); !matched {
		t.Fatal("decoded structured query did not match")
	}
}

func TestStructuredPatchRoundTripsEscapedSeparators(t *testing.T) {
	fixture := &crudFixture{card: escapedStructuredCard(), href: "/home/book/escaped.vcf", etag: `"e1"`}
	client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	name := StructuredName{FamilyName: "Doe;Smith", GivenName: "Jane"}
	addresses := []PostalAddress{{
		StreetAddress: "123 A;B Street",
		Locality:      "Paris",
		Country:       "France",
	}}
	if _, err := client.UpdateContact(context.Background(), &UpdateContactInput{
		AddressBook: book.Identifier,
		UID:         "escaped-contact",
		Patch:       ContactPatch{Name: &name, Addresses: &addresses},
	}); err != nil {
		t.Fatalf("UpdateContact() error = %v", err)
	}
	puts := fixture.recordsFor(http.MethodPut)
	if len(puts) != 1 {
		t.Fatalf("PUT count = %d", len(puts))
	}
	for _, required := range []string{`N:Doe\;Smith;Jane;;;`, `ADR:;;123 A\;B Street;Paris;;;France`} {
		if !strings.Contains(puts[0].body, required) {
			t.Fatalf("patched body missing %q:\n%s", required, puts[0].body)
		}
	}
	card, version, raw, err := decodeRawCard([]byte(puts[0].body))
	if err != nil {
		t.Fatalf("patched card invalid: %v", err)
	}
	detail := cardDetail(newCardView(card, raw), version, book.Identifier, `"e2"`)
	if detail.Name == nil || detail.Name.FamilyName != "Doe;Smith" {
		t.Fatalf("round-trip name = %#v", detail.Name)
	}
	if len(detail.Addresses) != 1 || detail.Addresses[0].StreetAddress != "123 A;B Street" {
		t.Fatalf("round-trip address = %#v", detail.Addresses)
	}
}

func TestGroupedLabelPreservedOnUnchangedValueAndDroppedOnRemoval(t *testing.T) {
	grouped := "BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"UID:grouped-contact\r\n" +
		"FN:Grouped\r\n" +
		"N:Grouped;;;;\r\n" +
		"item1.EMAIL;TYPE=INTERNET:person@example.com\r\n" +
		"item1.X-ABLabel:_$!<Home>!$_\r\n" +
		"item2.EMAIL;TYPE=INTERNET:work@example.com\r\n" +
		"item2.X-ABLabel:_$!<Work>!$_\r\n" +
		"END:VCARD\r\n"

	t.Run("unchanged value keeps its group and label", func(t *testing.T) {
		fixture := &crudFixture{card: grouped, href: "/home/book/grouped.vcf", etag: `"g1"`}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		emails := []TypedValue{{Type: "INTERNET", Value: "person@example.com"}, {Type: "INTERNET", Value: "work@example.com"}}
		if _, err := client.UpdateContact(context.Background(), &UpdateContactInput{
			AddressBook: book.Identifier, UID: "grouped-contact", Patch: ContactPatch{Emails: &emails},
		}); err != nil {
			t.Fatalf("UpdateContact() error = %v", err)
		}
		body := fixture.recordsFor(http.MethodPut)[0].body
		for _, required := range []string{
			"item1.EMAIL;TYPE=INTERNET:person@example.com",
			"item1.X-ABLabel:_$!<Home>!$_",
			"item2.EMAIL;TYPE=INTERNET:work@example.com",
			"item2.X-ABLabel:_$!<Work>!$_",
		} {
			if !strings.Contains(body, required) {
				t.Fatalf("grouped body missing %q:\n%s", required, body)
			}
		}
	})

	t.Run("removed value drops its label instead of orphaning it", func(t *testing.T) {
		fixture := &crudFixture{card: grouped, href: "/home/book/grouped.vcf", etag: `"g1"`}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		emails := []TypedValue{{Value: "person@example.com"}}
		if _, err := client.UpdateContact(context.Background(), &UpdateContactInput{
			AddressBook: book.Identifier, UID: "grouped-contact", Patch: ContactPatch{Emails: &emails},
		}); err != nil {
			t.Fatalf("UpdateContact() error = %v", err)
		}
		body := fixture.recordsFor(http.MethodPut)[0].body
		if strings.Contains(body, "X-ABLabel:_$!<Work>!$_") {
			t.Fatalf("orphan Work label retained:\n%s", body)
		}
		if !strings.Contains(body, "item1.X-ABLabel:_$!<Home>!$_") {
			t.Fatalf("preserved Home label missing:\n%s", body)
		}
	})

	t.Run("ambiguous value binding fails before PUT", func(t *testing.T) {
		ambiguous := "BEGIN:VCARD\r\n" +
			"VERSION:3.0\r\nUID:amb\r\nFN:Amb\r\nN:Amb;;;;\r\n" +
			"item1.EMAIL:same@example.com\r\nitem1.X-ABLabel:One\r\n" +
			"item2.EMAIL:same@example.com\r\nitem2.X-ABLabel:Two\r\n" +
			"END:VCARD\r\n"
		fixture := &crudFixture{card: ambiguous, href: "/home/book/amb.vcf", etag: `"a1"`}
		client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
		book := discoveredBook(t, client)
		emails := []TypedValue{{Value: "same@example.com"}}
		_, err := client.UpdateContact(context.Background(), &UpdateContactInput{
			AddressBook: book.Identifier, UID: "amb", Patch: ContactPatch{Emails: &emails},
		})
		if err == nil {
			t.Fatal("expected ambiguous grouped values to fail")
		}
		if len(fixture.recordsFor(http.MethodPut)) != 0 {
			t.Fatal("no PUT must occur before ambiguity is rejected")
		}
	})
}

func truncatedCard() string {
	var extras []string
	for index := 1; index <= 11; index++ {
		extras = append(extras, "EMAIL:e"+strconv.Itoa(index)+"x@example.com")
		extras = append(extras, "TEL:+1555000"+string(rune('0'+index%10))+string(rune('0'+index%10)))
	}
	for index := 1; index <= 6; index++ {
		extras = append(extras, "ADR:;;Street "+strings.Repeat("s", index)+";City;;;Country")
	}
	return v3Card("truncated", "Truncated", extras...)
}

func TestCreateEncodesEscapedSeparators(t *testing.T) {
	fixture := &crudFixture{}
	client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	_, err := client.CreateContact(context.Background(), &CreateContactInput{
		AddressBook:  book.Identifier,
		DisplayName:  "Jane Doe-Smith",
		Name:         StructuredName{FamilyName: "Doe;Smith", GivenName: "Jane"},
		Organization: "Acme;Labs",
		Addresses:    []PostalAddress{{StreetAddress: "123 A;B Street", Locality: "Paris", Country: "France"}},
	})
	if err != nil {
		t.Fatalf("CreateContact() error = %v", err)
	}
	body := fixture.recordsFor(http.MethodPut)[0].body
	for _, required := range []string{`N:Doe\;Smith;Jane;;;`, `ORG:Acme\;Labs`, `ADR:;;123 A\;B Street;Paris;;;France`} {
		if !strings.Contains(body, required) {
			t.Fatalf("create body missing %q:\n%s", required, body)
		}
	}
	if _, _, _, err := decodeRawCard([]byte(body)); err != nil {
		t.Fatalf("created card invalid: %v", err)
	}
}

func TestContactsReportRejectsIncompleteResources(t *testing.T) {
	failed := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` +
		`<D:response><D:href>/home/book/a.vcf</D:href><D:propstat><D:prop><D:getetag>&quot;e&quot;</D:getetag></D:prop><D:status>HTTP/1.1 403 Forbidden</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	client := NewClient(discoveryDoer(failed), "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	_, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier})
	requireCode(t, err, CodePartialFailure)

	missing := `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` +
		`<D:response><D:href>/home/book/a.vcf</D:href><D:propstat><D:prop><D:getetag>&quot;e&quot;</D:getetag></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>` +
		`</D:multistatus>`
	client = NewClient(discoveryDoer(missing), "https://contacts.icloud.com/", allowContactsHost)
	book = discoveredBook(t, client)
	_, err = client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier})
	requireCode(t, err, CodePartialFailure)
}

func TestPerContactTruncationMetadata(t *testing.T) {
	fixture := &multiCardFixture{cards: map[string]string{"/home/book/t.vcf": truncatedCard()}}
	client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)

	result, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier, Email: "e1", Limit: 5})
	if err != nil {
		t.Fatalf("SearchContacts() error = %v", err)
	}
	if len(result.Contacts) != 1 {
		t.Fatalf("matches = %d", len(result.Contacts))
	}
	summary := result.Contacts[0]
	if len(summary.Emails) != maxEmails {
		t.Fatalf("emails = %d", len(summary.Emails))
	}
	assertTruncation(t, summary.TruncatedFields, "emails", 1)
	assertTruncation(t, summary.TruncatedFields, "phones", 1)

	omitted, err := client.SearchContacts(context.Background(), SearchOptions{AddressBook: book.Identifier, Email: "e11", Limit: 5})
	if err != nil || len(omitted.Contacts) != 1 {
		t.Fatalf("omitted search = %#v, %v", omitted, err)
	}
	if !omitted.Contacts[0].MatchedFieldOmitted {
		t.Fatal("expected matchedFieldOmitted for an 11th email match")
	}

	detail, err := client.GetContact(context.Background(), book.Identifier, "truncated")
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	assertTruncation(t, detail.TruncatedFields, "emails", 1)
	assertTruncation(t, detail.TruncatedFields, "phones", 1)
	assertTruncation(t, detail.TruncatedFields, "addresses", 1)

	long := strings.Repeat("a", maxEmailBytes+20) + "@example.com"
	longCard := v3Card("long", "Long", "EMAIL:"+long)
	longFixture := &multiCardFixture{cards: map[string]string{"/home/book/l.vcf": longCard}}
	longClient := NewClient(longFixture, "https://contacts.icloud.com/", allowContactsHost)
	longBook := discoveredBook(t, longClient)
	longDetail, err := longClient.GetContact(context.Background(), longBook.Identifier, "long")
	if err != nil {
		t.Fatalf("GetContact(long) error = %v", err)
	}
	assertTruncation(t, longDetail.TruncatedFields, "emails", 0)
	for _, item := range longDetail.TruncatedFields {
		if item.Field == "emails" && !item.ValueTruncated {
			t.Fatal("expected valueTruncated for an overlong email")
		}
	}
	if len(longDetail.Emails) != 1 || len(longDetail.Emails[0].Value) > maxEmailBytes {
		t.Fatalf("overlong email was not bounded: %d bytes", len(longDetail.Emails[0].Value))
	}

	small := &multiCardFixture{cards: map[string]string{"/home/book/s.vcf": v3Card("small", "Small", "EMAIL:small@example.com")}}
	smallClient := NewClient(small, "https://contacts.icloud.com/", allowContactsHost)
	smallBook := discoveredBook(t, smallClient)
	smallDetail, err := smallClient.GetContact(context.Background(), smallBook.Identifier, "small")
	if err != nil {
		t.Fatalf("GetContact(small) error = %v", err)
	}
	if len(smallDetail.TruncatedFields) != 0 {
		t.Fatalf("small contact truncation = %#v", smallDetail.TruncatedFields)
	}
}

func assertTruncation(t *testing.T, items []FieldTruncation, field string, omitted int) {
	t.Helper()
	for _, item := range items {
		if item.Field != field {
			continue
		}
		if item.Omitted != omitted {
			t.Fatalf("%s omitted = %d, want %d", field, item.Omitted, omitted)
		}
		return
	}
	t.Fatalf("missing truncation metadata for %s in %#v", field, items)
}

func TestCaseDistinctUIDLookupResolvesExactContact(t *testing.T) {
	cards := map[string]string{
		"/home/book/a.vcf": v3Card("ClientA", "Upper"),
		"/home/book/b.vcf": v3Card("clienta", "Lower"),
	}
	fixture := &multiCardFixture{cards: cards}
	client := NewClient(fixture, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)

	detail, err := client.GetContact(context.Background(), book.Identifier, "ClientA")
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if detail.UID != "ClientA" || detail.DisplayName != "Upper" {
		t.Fatalf("resolved contact = %#v", detail)
	}
	gets := fixture.recordsFor(http.MethodGet)
	if len(gets) != 1 || gets[0].path != "/home/book/a.vcf" {
		t.Fatalf("GET paths = %#v", gets)
	}

	emails := []TypedValue{{Value: "clienta@example.com"}}
	result, err := client.UpdateContact(context.Background(), &UpdateContactInput{
		AddressBook: book.Identifier, UID: "clienta", Patch: ContactPatch{Emails: &emails},
	})
	if err != nil {
		t.Fatalf("UpdateContact() error = %v", err)
	}
	if result.UID != "clienta" {
		t.Fatalf("update result = %#v", result)
	}
	puts := fixture.recordsFor(http.MethodPut)
	if len(puts) != 1 || puts[0].path != "/home/book/b.vcf" {
		t.Fatalf("PUT paths = %#v", puts)
	}

	if _, err := client.DeleteContact(context.Background(), &DeleteContactInput{
		AddressBook: book.Identifier, UID: "ClientA", DryRun: true,
	}); err != nil {
		t.Fatalf("DeleteContact(dry run) error = %v", err)
	}
}

func TestDuplicateExactUIDLookupFailsClosed(t *testing.T) {
	cards := map[string]string{
		"/home/book/a.vcf": v3Card("ClientA", "One"),
		"/home/book/b.vcf": v3Card("ClientA", "Two"),
	}
	client := NewClient(&multiCardFixture{cards: cards}, "https://contacts.icloud.com/", allowContactsHost)
	book := discoveredBook(t, client)
	_, err := client.GetContact(context.Background(), book.Identifier, "ClientA")
	requireCode(t, err, CodeProtocolError)
}
