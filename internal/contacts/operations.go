package contacts

import (
	"context"
	"encoding/xml"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/emersion/go-vcard"
)

type addressObject struct {
	url     *url.URL
	card    vcard.Card
	raw     *rawCard
	version string
	etag    string
}

func searchQuery(opts SearchOptions, limit int) string {
	var filter string
	textMatch := func(property, value string) string {
		return `<C:prop-filter name="` + property + `"><C:text-match collation="i;unicode-casemap" match-type="contains">` +
			xmlEscape(value) + `</C:text-match></C:prop-filter>`
	}
	switch {
	case opts.Query != "":
		var matches strings.Builder
		for _, property := range []string{"FN", "N", "EMAIL", "TEL", "ORG"} {
			matches.WriteString(textMatch(property, opts.Query))
		}
		filter = `<C:filter test="anyof">` + matches.String() + `</C:filter>`
	case opts.Email != "":
		filter = `<C:filter>` + textMatch("EMAIL", opts.Email) + `</C:filter>`
	default:
		filter = `<C:filter><C:prop-filter name="VERSION"/></C:filter>`
	}
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` +
		`<D:prop><D:getetag/><C:address-data/></D:prop>` + filter +
		`<C:limit><C:nresults>` + strconv.Itoa(limit) + `</C:nresults></C:limit>` +
		`</C:addressbook-query>`
}

func uidQuery(uid string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">` +
		`<D:prop><D:getetag/><C:address-data/></D:prop>` +
		`<C:filter><C:prop-filter name="UID"><C:text-match collation="i;unicode-casemap" match-type="equals">` +
		xmlEscape(uid) + `</C:text-match></C:prop-filter></C:filter>` +
		`<C:limit><C:nresults>2</C:nresults></C:limit>` +
		`</C:addressbook-query>`
}

func xmlEscape(value string) string {
	var builder strings.Builder
	_ = xml.EscapeText(&builder, []byte(value))
	return builder.String()
}

func (c *Client) report(ctx context.Context, book bookRecord, body string, rowLimit int, byteCap int64) ([]addressObject, bool, int64, error) {
	header := make(http.Header)
	header.Set("Content-Type", `application/xml; charset="utf-8"`)
	header.Set("Depth", "1")
	response, err := c.doDAV(ctx, "REPORT", book.url, header, []byte(body), byteCap, book.url)
	if err != nil {
		return nil, false, 0, err
	}
	if response.Status != http.StatusMultiStatus {
		return nil, false, int64(len(response.Body)), classifyDAVError(response, false)
	}
	rows, overflow, err := decodeMultiStatus(response.Body, rowLimit)
	if err != nil {
		return nil, false, int64(len(response.Body)), err
	}
	objects := make([]addressObject, 0, len(rows))
	for _, row := range rows {
		prop := successfulProp(row)
		if prop == nil || !prop.AddressData.Present || strings.TrimSpace(prop.AddressData.Value) == "" {
			continue
		}
		resourceURL, resolveErr := resolveAndValidate(c, response.URL, row.Href)
		if resolveErr != nil {
			return nil, false, int64(len(response.Body)), resolveErr
		}
		if !urlWithin(resourceURL, book.url, false) {
			return nil, false, int64(len(response.Body)), newError(CodeProtocolError, 0, "Contacts REPORT returned a resource outside its address book")
		}
		card, version, decodeErr := decodeCard([]byte(prop.AddressData.Value))
		if decodeErr != nil {
			return nil, false, int64(len(response.Body)), decodeErr
		}
		etag, etagErr := strongETagFromResponse(row)
		if etagErr != nil {
			return nil, false, int64(len(response.Body)), newError(CodeProtocolError, response.Status, "Contacts REPORT response has an invalid ETag")
		}
		objects = append(objects, addressObject{url: resourceURL, card: card, version: version, etag: etag})
	}
	return objects, overflow, int64(len(response.Body)), nil
}

// SearchContacts queries every selected book within the 2,000-card and 32 MiB
// aggregate scan bounds, then applies deterministic local filters.
func (c *Client) SearchContacts(ctx context.Context, opts SearchOptions) (SearchResult, error) {
	if err := validateSearchOptions(&opts); err != nil {
		return SearchResult{}, err
	}
	var books []bookRecord
	if opts.AddressBook != "" {
		book, err := c.book(ctx, opts.AddressBook)
		if err != nil {
			return SearchResult{}, err
		}
		books = []bookRecord{book}
	} else {
		if err := c.discover(ctx); err != nil {
			return SearchResult{}, err
		}
		books = append([]bookRecord(nil), c.state.books...)
	}

	remainingCards := maxCardsScanned
	remainingBytes := int64(maxReportBytes)
	var matches []ContactSummary
	scanLimit := false
	for index, book := range books {
		if remainingCards == 0 || remainingBytes == 0 {
			scanLimit = true
			break
		}
		requestLimit := remainingCards + 1
		objects, overflow, used, err := c.report(ctx, book, searchQuery(opts, requestLimit), requestLimit, remainingBytes)
		if err != nil {
			return SearchResult{}, err
		}
		remainingBytes -= used
		complete := len(objects)
		if complete > remainingCards {
			complete = remainingCards
		}
		for _, object := range objects[:complete] {
			remainingCards--
			if contactMatches(object.card, opts) {
				matches = append(matches, cardSummary(object.card, book.public.Identifier, object.etag))
			}
		}
		if overflow || len(objects) > complete || remainingCards == 0 && index < len(books)-1 {
			scanLimit = true
			break
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		left := normalizeForSort(matches[i].DisplayName)
		right := normalizeForSort(matches[j].DisplayName)
		if left != right {
			return left < right
		}
		if matches[i].UID != matches[j].UID {
			return matches[i].UID < matches[j].UID
		}
		return matches[i].AddressBook < matches[j].AddressBook
	})
	result := SearchResult{ScanLimitReached: scanLimit, Truncated: len(matches) > opts.Limit}
	count := len(matches)
	if count > opts.Limit {
		count = opts.Limit
	}
	for _, summary := range matches[:count] {
		result.Contacts = append(result.Contacts, summary)
		if !resultFits(result) {
			result.Contacts = result.Contacts[:len(result.Contacts)-1]
			result.Truncated = true
			break
		}
	}
	return result, nil
}

func contactMatches(card vcard.Card, opts SearchOptions) bool {
	if !opts.IncludeGroups && cardIsGroup(card) {
		return false
	}
	if opts.Query != "" {
		needle := strings.ToLower(opts.Query)
		fields := []string{
			card.Value(vcard.FieldFormattedName),
			card.Value(vcard.FieldName),
			card.Value(vcard.FieldOrganization),
		}
		fields = append(fields, card.Values(vcard.FieldEmail)...)
		fields = append(fields, card.Values(vcard.FieldTelephone)...)
		matched := false
		for _, value := range fields {
			if strings.Contains(strings.ToLower(value), needle) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if opts.Email != "" {
		needle := strings.ToLower(opts.Email)
		matched := false
		for _, value := range card.Values(vcard.FieldEmail) {
			if strings.Contains(strings.ToLower(value), needle) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if opts.Phone != "" {
		needle := normalizePhone(opts.Phone)
		matched := false
		for _, value := range card.Values(vcard.FieldTelephone) {
			if strings.Contains(normalizePhone(value), needle) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func (c *Client) findByUID(ctx context.Context, book bookRecord, uid string) (*addressObject, error) {
	objects, overflow, _, err := c.report(ctx, book, uidQuery(uid), 3, maxReportBytes)
	if err != nil {
		return nil, err
	}
	if overflow || len(objects) > 1 {
		return nil, newError(CodeProtocolError, 0, "UID lookup returned more than one contact")
	}
	if len(objects) == 0 {
		return nil, newError(CodeNotFound, http.StatusNotFound, "contact UID was not found in the address book")
	}
	if objects[0].card.Value(vcard.FieldUID) != uid {
		return nil, newError(CodeProtocolError, 0, "UID lookup returned a different contact")
	}
	return c.getObject(ctx, book, objects[0].url, uid)
}

func (c *Client) getObject(ctx context.Context, book bookRecord, target *url.URL, expectedUID string) (*addressObject, error) {
	if !urlWithin(target, book.url, false) {
		return nil, newError(CodeProtocolError, 0, "contact resource is outside its address book")
	}
	header := make(http.Header)
	header.Set("Accept", "text/vcard")
	response, err := c.doDAV(ctx, http.MethodGet, target, header, nil, maxCardBytes, book.url)
	if err != nil {
		return nil, err
	}
	if response.Status/100 != 2 {
		return nil, classifyDAVError(response, false)
	}
	if !urlWithin(response.URL, book.url, false) {
		return nil, newError(CodeProtocolError, 0, "contact GET resolved outside its address book")
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || !acceptableVCardContentType(mediaType) {
		return nil, newError(CodeProtocolError, 0, "contact GET returned an invalid Content-Type")
	}
	card, version, raw, err := decodeRawCard(response.Body)
	if err != nil {
		return nil, err
	}
	if card.Value(vcard.FieldUID) != expectedUID {
		return nil, newError(CodeProtocolError, 0, "contact GET returned a different UID")
	}
	etag, etagErr := strongETagFromHeader(response.Header)
	if etagErr != nil {
		return nil, newError(CodeProtocolError, response.Status, "contact GET returned an invalid ETag")
	}
	return &addressObject{url: cloneURL(response.URL), card: card, raw: raw, version: version, etag: etag}, nil
}

// acceptableVCardContentType reports whether a CardDAV GET Content-Type is a
// known vCard media type. iCloud production responses often use text/plain
// with a vCard body; the body is still validated by decodeRawCard.
func acceptableVCardContentType(mediaType string) bool {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/vcard", "text/x-vcard", "text/directory", "text/plain":
		return true
	default:
		return false
	}
}

// GetContact returns one modeled contact after UID query and full GET.
func (c *Client) GetContact(ctx context.Context, addressBook, uid string) (*Contact, error) {
	if err := validateUID(uid); err != nil {
		return nil, err
	}
	book, err := c.book(ctx, addressBook)
	if err != nil {
		return nil, err
	}
	object, err := c.findByUID(ctx, book, uid)
	if err != nil {
		return nil, err
	}
	detail := cardDetail(object.card, object.version, addressBook, object.etag)
	if !resultFits(detail) {
		return nil, newError(CodePayloadTooLarge, 0, "contact detail exceeds the result limit")
	}
	return detail, nil
}

// CreateContact writes vCard 3.0 with If-None-Match and re-GETs normalization.
func (c *Client) CreateContact(ctx context.Context, input *CreateContactInput) (CreateResult, error) {
	if err := validateCreate(input); err != nil {
		return CreateResult{}, err
	}
	book, err := c.book(ctx, input.AddressBook)
	if err != nil {
		return CreateResult{}, err
	}
	if book.public.WriteVersion != "3.0" {
		return CreateResult{}, validationError("the address book does not advertise vCard 3.0 writes")
	}
	uid := input.ClientUID
	if uid == "" {
		uid, err = uuidV4()
		if err != nil {
			return CreateResult{}, err
		}
	}
	resourceID, err := uuidV4()
	if err != nil {
		return CreateResult{}, err
	}
	target := childResourceURL(book.url, resourceID+".vcf")
	if err := c.validateURL(target); err != nil {
		return CreateResult{}, err
	}
	encoded, err := encodeCard(newV3Card(input, uid), book.public.MaxResourceSize)
	if err != nil {
		return CreateResult{}, err
	}
	header := make(http.Header)
	header.Set("Content-Type", "text/vcard; charset=utf-8")
	header.Set("If-None-Match", "*")
	response, err := c.doDAV(ctx, http.MethodPut, target, header, encoded, maxErrorBodyBytes, book.url)
	if err != nil {
		return CreateResult{}, err
	}
	if response.Status/100 != 2 {
		return CreateResult{}, classifyDAVError(response, true)
	}
	createdURL := target
	if location := response.Header.Get("Location"); location != "" {
		createdURL, err = resolveAndValidate(c, response.URL, location)
		if err != nil || !urlWithin(createdURL, book.url, false) {
			return CreateResult{AddressBook: input.AddressBook, UID: uid, ResultIncomplete: true, Warning: "contact was created, but its normalized resource location was invalid"}, nil
		}
	}
	result := CreateResult{AddressBook: input.AddressBook, UID: uid}
	created, getErr := c.getObject(ctx, book, createdURL, uid)
	if getErr != nil {
		result.ResultIncomplete = true
		result.Warning = "contact was created, but normalized metadata could not be read"
		return result, nil
	}
	result.ETag = readableETag(created.etag)
	return result, nil
}

// UpdateContact patches a full vCard 3.0 object and preserves unknown fields.
func (c *Client) UpdateContact(ctx context.Context, input *UpdateContactInput) (UpdateResult, error) {
	if err := validateUpdate(input); err != nil {
		return UpdateResult{}, err
	}
	callerETag, _ := normalizeCallerETag(input.ETag)
	book, err := c.book(ctx, input.AddressBook)
	if err != nil {
		return UpdateResult{}, err
	}
	object, err := c.findByUID(ctx, book, input.UID)
	if err != nil {
		return UpdateResult{}, err
	}
	serverETag, err := validateServerETag(object.etag)
	if err != nil {
		return UpdateResult{}, err
	}
	if object.version != "3.0" {
		return UpdateResult{}, validationError("vCard 4.0 contacts are read-only")
	}
	if cardIsGroup(object.card) {
		return UpdateResult{}, validationError("contact groups are read-only")
	}
	encoded, err := encodePatchedCard(object.raw, object.card, input.Patch, book.public.MaxResourceSize)
	if err != nil {
		return UpdateResult{}, err
	}
	if callerETag == "" {
		callerETag = serverETag
	}
	header := make(http.Header)
	header.Set("Content-Type", "text/vcard; charset=utf-8")
	header.Set("If-Match", callerETag)
	response, err := c.doDAV(ctx, http.MethodPut, object.url, header, encoded, maxErrorBodyBytes, book.url)
	if err != nil {
		return UpdateResult{}, err
	}
	if response.Status/100 != 2 {
		return UpdateResult{}, classifyDAVError(response, false)
	}
	result := UpdateResult{AddressBook: input.AddressBook, UID: input.UID}
	updated, getErr := c.getObject(ctx, book, object.url, input.UID)
	if getErr != nil {
		result.ResultIncomplete = true
		result.Warning = "contact was updated, but normalized metadata could not be read"
		return result, nil
	}
	result.ETag = readableETag(updated.etag)
	return result, nil
}

// DeleteContact performs a full GET and then a strong conditional DELETE.
func (c *Client) DeleteContact(ctx context.Context, input *DeleteContactInput) (DeleteResult, error) {
	if err := validateDelete(input); err != nil {
		return DeleteResult{}, err
	}
	callerETag, _ := normalizeCallerETag(input.ETag)
	book, err := c.book(ctx, input.AddressBook)
	if err != nil {
		return DeleteResult{}, err
	}
	object, err := c.findByUID(ctx, book, input.UID)
	if err != nil {
		return DeleteResult{}, err
	}
	if object.version == "4.0" || cardIsGroup(object.card) {
		return DeleteResult{}, validationError("this contact object is read-only")
	}
	result := DeleteResult{AddressBook: input.AddressBook, UID: input.UID, WouldDelete: true}
	serverETag, err := validateServerETag(object.etag)
	if err != nil {
		return DeleteResult{}, err
	}
	if input.DryRun {
		if callerETag != "" && callerETag != serverETag {
			return DeleteResult{}, newError(CodeConcurrentModification, http.StatusPreconditionFailed, "the contact changed since it was read")
		}
		result.DryRun = true
		return result, nil
	}
	if callerETag == "" {
		callerETag = serverETag
	}
	header := make(http.Header)
	header.Set("If-Match", callerETag)
	response, err := c.doDAV(ctx, http.MethodDelete, object.url, header, nil, maxErrorBodyBytes, book.url)
	if err != nil {
		return DeleteResult{}, err
	}
	if response.Status/100 != 2 {
		return DeleteResult{}, classifyDAVError(response, false)
	}
	return result, nil
}

func childResourceURL(collection *url.URL, name string) *url.URL {
	base := cloneURL(collection)
	basePath := strings.TrimSuffix(base.Path, "/")
	escapedBase := strings.TrimSuffix(base.EscapedPath(), "/")
	base.Path = basePath + "/"
	escapedBase += "/"
	if escapedBase != base.Path {
		base.RawPath = escapedBase
	} else {
		base.RawPath = ""
	}
	base.RawQuery = ""
	base.Fragment = ""
	return base.ResolveReference(&url.URL{Path: name})
}
