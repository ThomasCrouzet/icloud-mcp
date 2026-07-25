package contacts

import (
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxAddressBooks      = 100
	maxSearchResults     = 100
	defaultSearchResults = 50
	maxCardsScanned      = 2000
	maxQueryBytes        = 256
	maxEmailBytes        = 320
	maxPhoneBytes        = 64
	maxDisplayNameBytes  = 500
	maxTextBytes         = 500
	maxNotesBytes        = 4000
	maxURLBytes          = 2048
	maxTypeBytes         = 64
	maxUIDBytes          = 255
	maxEmails            = 10
	maxPhones            = 10
	maxURLs              = 5
	maxAddresses         = 5
	maxCardBytes         = 1 << 20
	maxPropfindBytes     = 8 << 20
	maxReportBytes       = 32 << 20
	maxResultBytes       = 256 << 10
	maxErrorBodyBytes    = 64 << 10
	maxCardProperties    = 10000
	maxETagBytes         = 512
)

func validateText(name, value string, max int, required bool) error {
	if !utf8.ValidString(value) {
		return validationError(name + " must be valid UTF-8")
	}
	if required && strings.TrimSpace(value) == "" {
		return validationError(name + " cannot be empty")
	}
	if len(value) > max {
		return validationError(name + " exceeds its byte limit")
	}
	for _, r := range value {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return validationError(name + " contains a disallowed control character")
		}
	}
	return nil
}

func validateSingleLine(name, value string, max int, required bool) error {
	if err := validateText(name, value, max, required); err != nil {
		return err
	}
	if strings.ContainsAny(value, "\r\n") {
		return validationError(name + " must be a single line")
	}
	return nil
}

func validateUID(uid string) error {
	return validateSingleLine("uid", uid, maxUIDBytes, true)
}

func validateAddressBookID(id string) error {
	if len(id) != len("book-")+22 || !strings.HasPrefix(id, "book-") {
		return validationError("address_book must be an identifier returned by list_address_books")
	}
	for _, r := range id[len("book-"):] {
		if !validTokenRune(r) {
			return validationError("address_book must be an identifier returned by list_address_books")
		}
	}
	return nil
}

func validateSearchOptions(opts *SearchOptions) error {
	if opts.AddressBook != "" {
		if err := validateAddressBookID(opts.AddressBook); err != nil {
			return err
		}
	}
	if err := validateText("query", opts.Query, maxQueryBytes, false); err != nil {
		return err
	}
	if err := validateSingleLine("email", opts.Email, maxEmailBytes, false); err != nil {
		return err
	}
	if err := validateSingleLine("phone", opts.Phone, maxPhoneBytes, false); err != nil {
		return err
	}
	if opts.Phone != "" && normalizePhone(opts.Phone) == "" {
		return validationError("phone must contain at least one digit")
	}
	if opts.Limit == 0 {
		opts.Limit = defaultSearchResults
	}
	if opts.Limit < 1 || opts.Limit > maxSearchResults {
		return validationError("limit must be between 1 and 100")
	}
	return nil
}

func validateCreate(in *CreateContactInput) error {
	if in == nil {
		return validationError("contact cannot be nil")
	}
	if err := validateAddressBookID(in.AddressBook); err != nil {
		return err
	}
	if err := validateContactFields(in.DisplayName, &in.Name, in.Organization, in.Title, in.Nickname, in.Birthday, in.Notes, in.Emails, in.Phones, in.Addresses, in.URLs); err != nil {
		return err
	}
	if strings.TrimSpace(in.DisplayName) == "" && nameEmpty(in.Name) {
		return validationError("display_name or one structured name component is required")
	}
	if in.ClientUID != "" {
		if err := validateUID(in.ClientUID); err != nil {
			return validationError("client_uid is invalid")
		}
	}
	return nil
}

func validateUpdate(in *UpdateContactInput) error {
	if in == nil {
		return validationError("update cannot be nil")
	}
	if err := validateAddressBookID(in.AddressBook); err != nil {
		return err
	}
	if err := validateUID(in.UID); err != nil {
		return err
	}
	if _, err := normalizeCallerETag(in.ETag); err != nil {
		return err
	}
	p := &in.Patch
	if p.DisplayName == nil && p.Name == nil && p.Organization == nil && p.Title == nil && p.Nickname == nil && p.Birthday == nil && p.Notes == nil && p.Emails == nil && p.Phones == nil && p.Addresses == nil && p.URLs == nil {
		return validationError("at least one contact field must be patched")
	}
	if p.DisplayName != nil {
		if err := validateSingleLine("display_name", *p.DisplayName, maxDisplayNameBytes, false); err != nil {
			return err
		}
	}
	if p.Name != nil {
		if err := validateName(*p.Name); err != nil {
			return err
		}
	}
	for name, value := range map[string]*string{
		"organization": p.Organization,
		"title":        p.Title,
		"nickname":     p.Nickname,
	} {
		if value != nil {
			if err := validateSingleLine(name, *value, maxTextBytes, false); err != nil {
				return err
			}
		}
	}
	if p.Birthday != nil {
		if err := validateBirthday(*p.Birthday); err != nil {
			return err
		}
	}
	if p.Notes != nil {
		if err := validateText("notes", *p.Notes, maxNotesBytes, false); err != nil {
			return err
		}
	}
	if p.Emails != nil {
		if err := validateTypedValues("emails", *p.Emails, maxEmails, maxEmailBytes, validateEmail); err != nil {
			return err
		}
	}
	if p.Phones != nil {
		if err := validateTypedValues("phones", *p.Phones, maxPhones, maxPhoneBytes, validatePhone); err != nil {
			return err
		}
	}
	if p.Addresses != nil {
		if err := validateAddresses(*p.Addresses); err != nil {
			return err
		}
	}
	if p.URLs != nil {
		if err := validateTypedValues("urls", *p.URLs, maxURLs, maxURLBytes, validateURLValue); err != nil {
			return err
		}
	}
	return nil
}

func validateDelete(in *DeleteContactInput) error {
	if in == nil {
		return validationError("delete request cannot be nil")
	}
	if err := validateAddressBookID(in.AddressBook); err != nil {
		return err
	}
	if err := validateUID(in.UID); err != nil {
		return err
	}
	_, err := normalizeCallerETag(in.ETag)
	return err
}

func validateContactFields(display string, name *StructuredName, organization, title, nickname, birthday, notes string, emails, phones []TypedValue, addresses []PostalAddress, urls []TypedValue) error {
	if err := validateSingleLine("display_name", display, maxDisplayNameBytes, false); err != nil {
		return err
	}
	if name != nil {
		if err := validateName(*name); err != nil {
			return err
		}
	}
	for field, value := range map[string]string{"organization": organization, "title": title, "nickname": nickname} {
		if err := validateSingleLine(field, value, maxTextBytes, false); err != nil {
			return err
		}
	}
	if err := validateBirthday(birthday); err != nil {
		return err
	}
	if err := validateText("notes", notes, maxNotesBytes, false); err != nil {
		return err
	}
	if err := validateTypedValues("emails", emails, maxEmails, maxEmailBytes, validateEmail); err != nil {
		return err
	}
	if err := validateTypedValues("phones", phones, maxPhones, maxPhoneBytes, validatePhone); err != nil {
		return err
	}
	if err := validateAddresses(addresses); err != nil {
		return err
	}
	return validateTypedValues("urls", urls, maxURLs, maxURLBytes, validateURLValue)
}

func validateName(name StructuredName) error {
	values := []struct{ name, value string }{
		{"family_name", name.FamilyName},
		{"given_name", name.GivenName},
		{"additional_name", name.AdditionalName},
		{"honorific_prefix", name.HonorificPrefix},
		{"honorific_suffix", name.HonorificSuffix},
	}
	for _, value := range values {
		if err := validateSingleLine(value.name, value.value, maxTextBytes, false); err != nil {
			return err
		}
		if strings.Contains(value.value, ";") {
			return validationError(value.name + " cannot contain a semicolon")
		}
	}
	return nil
}

func validateBirthday(value string) error {
	if value == "" {
		return nil
	}
	if len(value) != len("2006-01-02") {
		return validationError("birthday must use YYYY-MM-DD")
	}
	t, err := time.Parse("2006-01-02", value)
	if err != nil || t.Format("2006-01-02") != value {
		return validationError("birthday must use YYYY-MM-DD")
	}
	return nil
}

func validateTypedValues(name string, values []TypedValue, maxCount, maxValue int, valueValidator func(string) error) error {
	if len(values) > maxCount {
		return validationError(name + " exceeds its item limit")
	}
	for _, value := range values {
		if err := validateSingleLine(name+" type", value.Type, maxTypeBytes, false); err != nil {
			return err
		}
		if value.Type != "" && !validTypeToken(value.Type) {
			return validationError(name + " type contains an invalid character")
		}
		if err := validateSingleLine(name+" value", value.Value, maxValue, true); err != nil {
			return err
		}
		if err := valueValidator(value.Value); err != nil {
			return err
		}
	}
	return nil
}

func validateEmail(value string) error {
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value {
		return validationError("email value must be a plain address")
	}
	return nil
}

func validatePhone(value string) error {
	if normalizePhone(value) == "" {
		return validationError("phone value must contain at least one digit")
	}
	return nil
}

func validateURLValue(value string) error {
	u, err := url.ParseRequestURI(value)
	if err != nil || u.Scheme == "" {
		return validationError("url value must be an absolute URI")
	}
	return nil
}

func validateAddresses(values []PostalAddress) error {
	if len(values) > maxAddresses {
		return validationError("addresses exceeds its item limit")
	}
	for _, address := range values {
		parts := []struct{ name, value string }{
			{"address type", address.Type},
			{"post office box", address.PostOfficeBox},
			{"extended address", address.ExtendedAddress},
			{"street address", address.StreetAddress},
			{"locality", address.Locality},
			{"region", address.Region},
			{"postal code", address.PostalCode},
			{"country", address.Country},
		}
		for _, part := range parts {
			limit := maxTextBytes
			if part.name == "address type" {
				limit = maxTypeBytes
			}
			if err := validateSingleLine(part.name, part.value, limit, false); err != nil {
				return err
			}
			if part.name == "address type" {
				if part.value != "" && !validTypeToken(part.value) {
					return validationError("address type contains an invalid character")
				}
			} else if strings.Contains(part.value, ";") {
				return validationError(part.name + " cannot contain a semicolon")
			}
		}
	}
	return nil
}

func nameEmpty(name StructuredName) bool {
	return strings.TrimSpace(name.FamilyName) == "" && strings.TrimSpace(name.GivenName) == "" && strings.TrimSpace(name.AdditionalName) == "" && strings.TrimSpace(name.HonorificPrefix) == "" && strings.TrimSpace(name.HonorificSuffix) == ""
}

func validTypeToken(value string) bool {
	for _, r := range value {
		if !validTokenRune(r) {
			return false
		}
	}
	return true
}

func validTokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

func normalizePhone(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func normalizeCallerETag(etag string) (string, error) {
	if etag == "" {
		return "", nil
	}
	parsed, err := parseStrongETag(etag)
	if err != nil {
		return "", validationError("etag must be a specific strong ETag")
	}
	return parsed, nil
}

func validateServerETag(etag string) (string, error) {
	parsed, err := parseStrongETag(etag)
	if err != nil {
		return "", newError(CodeProtocolError, 0, "the contact does not have a usable strong ETag")
	}
	return parsed, nil
}

// parseStrongETag validates one strong entity-tag and preserves its exact
// quoted wire value. Backslash is an opaque byte, not a quoting escape.
func parseStrongETag(value string) (string, error) {
	if len(value) > maxETagBytes {
		return "", fmt.Errorf("invalid strong entity-tag")
	}
	value = strings.Trim(value, " \t")
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("invalid strong entity-tag")
	}
	for index := 1; index < len(value)-1; index++ {
		valueByte := value[index]
		if valueByte < 0x21 || valueByte == '"' || valueByte == 0x7f {
			return "", fmt.Errorf("invalid strong entity-tag")
		}
	}
	return value, nil
}

func strongETagFromHeader(header http.Header) (string, error) {
	values := header.Values("ETag")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", fmt.Errorf("invalid entity-tag field count")
	}
	return parseStrongETag(values[0])
}

func strongETagFromResponse(response multiStatusResponse) (string, error) {
	var values []string
	for _, propstat := range response.Propstats {
		if statusIsOK(propstat.Status) {
			values = append(values, propstat.Prop.GetETags...)
		}
	}
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 {
		return "", fmt.Errorf("invalid entity-tag property count")
	}
	return parseStrongETag(values[0])
}
