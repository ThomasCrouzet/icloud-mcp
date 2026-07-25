package contacts

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-vcard"
)

const appleGroupField = "X-ADDRESSBOOKSERVER-KIND"

func decodeCard(data []byte) (vcard.Card, string, error) {
	card, version, _, err := decodeRawCard(data)
	return card, version, err
}

type rawCard struct {
	properties []rawProperty
}

type rawProperty struct {
	group     string
	name      string
	value     string
	hasParams bool
	raw       []byte
}

func decodeRawCard(data []byte) (vcard.Card, string, *rawCard, error) {
	if len(data) > maxCardBytes {
		return nil, "", nil, newError(CodePayloadTooLarge, 0, "one vCard exceeded its byte limit")
	}
	raw, err := parseRawCard(data)
	if err != nil {
		return nil, "", nil, err
	}
	decoder := vcard.NewDecoder(bytes.NewReader(data))
	card, err := decoder.Decode()
	if err != nil {
		return nil, "", nil, invalidCardError()
	}
	decodedProperties := 0
	for _, fields := range card {
		decodedProperties += len(fields)
	}
	if decodedProperties != len(raw.properties)-2 {
		return nil, "", nil, invalidCardError()
	}
	if _, trailingErr := decoder.Decode(); trailingErr != io.EOF {
		return nil, "", nil, invalidCardError()
	}
	version := strings.TrimSpace(card.Value(vcard.FieldVersion))
	if version != raw.properties[1].value || version != "3.0" && version != "4.0" {
		return nil, "", nil, newError(CodeProtocolError, 0, "iCloud Contacts returned an unsupported vCard version")
	}
	if strings.TrimSpace(card.Value(vcard.FieldUID)) == "" {
		return nil, "", nil, newError(CodeProtocolError, 0, "iCloud Contacts returned a vCard without a UID")
	}
	if err := validateUID(card.Value(vcard.FieldUID)); err != nil {
		return nil, "", nil, newError(CodeProtocolError, 0, "iCloud Contacts returned an invalid vCard UID")
	}
	return card, version, raw, nil
}

func parseRawCard(data []byte) (*rawCard, error) {
	if len(data) == 0 {
		return nil, invalidCardError()
	}
	raw := &rawCard{}
	var chunks, logical []byte
	flush := func() error {
		if len(chunks) == 0 {
			return nil
		}
		property, err := parseRawProperty(logical, chunks)
		if err != nil {
			return err
		}
		raw.properties = append(raw.properties, property)
		if len(raw.properties) > maxCardProperties+2 {
			return newError(CodePayloadTooLarge, 0, "one vCard exceeded its property limit")
		}
		chunks = nil
		logical = nil
		return nil
	}

	for offset := 0; offset < len(data); {
		lineEnd := bytes.IndexByte(data[offset:], '\n')
		if lineEnd < 0 {
			return nil, invalidCardError()
		}
		lineEnd += offset
		next := lineEnd + 1
		line := data[offset:lineEnd]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) == 0 || bytes.IndexByte(line, '\r') >= 0 || !validPhysicalLine(line) {
			return nil, invalidCardError()
		}
		chunk := data[offset:next]
		if line[0] == ' ' || line[0] == '\t' {
			if len(chunks) == 0 {
				return nil, invalidCardError()
			}
			chunks = append(chunks, chunk...)
			logical = append(logical, line[1:]...)
		} else {
			if err := flush(); err != nil {
				return nil, err
			}
			chunks = append(chunks, chunk...)
			logical = append(logical, line...)
		}
		offset = next
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := validateRawCard(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func validPhysicalLine(line []byte) bool {
	for _, value := range line {
		if value == 0 || value == 0x7f || value < 0x20 && value != '\t' {
			return false
		}
	}
	return true
}

func parseRawProperty(logical, chunks []byte) (rawProperty, error) {
	colon, err := delimiterOutsideQuotes(logical, ':')
	if err != nil || colon <= 0 {
		return rawProperty{}, invalidCardError()
	}
	head := string(logical[:colon])
	parts, err := splitOutsideQuotes(head, ';')
	if err != nil || len(parts) == 0 || parts[0] == "" {
		return rawProperty{}, invalidCardError()
	}
	identity := parts[0]
	group := ""
	name := identity
	if dot := strings.IndexByte(identity, '.'); dot >= 0 {
		if strings.IndexByte(identity[dot+1:], '.') >= 0 {
			return rawProperty{}, invalidCardError()
		}
		group, name = identity[:dot], identity[dot+1:]
	}
	if !validPropertyToken(name) || group != "" && !validPropertyToken(group) {
		return rawProperty{}, invalidCardError()
	}
	for _, parameter := range parts[1:] {
		equals, equalsErr := delimiterOutsideQuotes([]byte(parameter), '=')
		if equalsErr != nil || equals <= 0 || !validPropertyToken(parameter[:equals]) || !validParameterValue(parameter[equals+1:]) {
			return rawProperty{}, invalidCardError()
		}
	}
	return rawProperty{
		group:     group,
		name:      strings.ToUpper(name),
		value:     string(logical[colon+1:]),
		hasParams: len(parts) > 1,
		raw:       append([]byte(nil), chunks...),
	}, nil
}

func delimiterOutsideQuotes(value []byte, delimiter byte) (int, error) {
	quoted := false
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\\':
			if quoted && index+1 < len(value) {
				index++
			}
		case '"':
			quoted = !quoted
		default:
			if value[index] == delimiter && !quoted {
				return index, nil
			}
		}
	}
	if quoted {
		return -1, invalidCardError()
	}
	return -1, nil
}

func splitOutsideQuotes(value string, delimiter byte) ([]string, error) {
	var parts []string
	start := 0
	quoted := false
	for index := 0; index < len(value); index++ {
		switch value[index] {
		case '\\':
			if quoted && index+1 < len(value) {
				index++
			}
		case '"':
			quoted = !quoted
		default:
			if value[index] == delimiter && !quoted {
				parts = append(parts, value[start:index])
				start = index + 1
			}
		}
	}
	if quoted {
		return nil, invalidCardError()
	}
	return append(parts, value[start:]), nil
}

func validParameterValue(value string) bool {
	if value == "" {
		return false
	}
	values, err := splitOutsideQuotes(value, ',')
	if err != nil {
		return false
	}
	for _, item := range values {
		if item == "" {
			return false
		}
		if item[0] == '"' {
			if len(item) < 2 || item[len(item)-1] != '"' {
				return false
			}
		} else if strings.ContainsRune(item, '"') {
			return false
		}
	}
	return true
}

func validPropertyToken(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		letter := character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z'
		digit := character >= '0' && character <= '9'
		if !letter && !digit && character != '-' {
			return false
		}
	}
	return true
}

func validateRawCard(raw *rawCard) error {
	if raw == nil || len(raw.properties) < 6 {
		return invalidCardError()
	}
	first := raw.properties[0]
	last := raw.properties[len(raw.properties)-1]
	if first.group != "" || first.hasParams || first.name != "BEGIN" || !strings.EqualFold(first.value, "VCARD") ||
		last.group != "" || last.hasParams || last.name != "END" || !strings.EqualFold(last.value, "VCARD") {
		return invalidCardError()
	}
	counts := make(map[string]int)
	var required = map[string]rawProperty{}
	for index, property := range raw.properties {
		if index > 0 && index < len(raw.properties)-1 && (property.name == "BEGIN" || property.name == "END") {
			return invalidCardError()
		}
		counts[property.name]++
		if property.name == vcard.FieldVersion || property.name == vcard.FieldUID || property.name == vcard.FieldFormattedName || property.name == vcard.FieldName {
			required[property.name] = property
		}
	}
	if raw.properties[1].name != vcard.FieldVersion || counts[vcard.FieldVersion] != 1 || counts[vcard.FieldUID] != 1 || counts[vcard.FieldFormattedName] != 1 || counts[vcard.FieldName] != 1 {
		return invalidCardError()
	}
	for _, name := range []string{vcard.FieldVersion, vcard.FieldUID, vcard.FieldFormattedName, vcard.FieldName} {
		if required[name].group != "" {
			return invalidCardError()
		}
	}
	version := required[vcard.FieldVersion]
	if version.hasParams || version.value != "3.0" && version.value != "4.0" {
		return invalidCardError()
	}
	if strings.TrimSpace(required[vcard.FieldUID].value) == "" || strings.TrimSpace(required[vcard.FieldFormattedName].value) == "" || countUnescaped(required[vcard.FieldName].value, ';') != 4 {
		return invalidCardError()
	}
	return nil
}

func countUnescaped(value string, wanted byte) int {
	count := 0
	escaped := false
	for index := 0; index < len(value); index++ {
		if escaped {
			escaped = false
			continue
		}
		if value[index] == '\\' {
			escaped = true
			continue
		}
		if value[index] == wanted {
			count++
		}
	}
	return count
}

func invalidCardError() *Error {
	return newError(CodeProtocolError, 0, "iCloud Contacts returned an invalid vCard")
}

func encodeCard(card vcard.Card, serverMax int64) ([]byte, error) {
	var buffer bytes.Buffer
	if err := vcard.NewEncoder(&buffer).Encode(card); err != nil {
		return nil, newError(CodeInternalError, 0, "failed to encode the contact")
	}
	limit := int64(maxCardBytes)
	if serverMax > 0 && serverMax < limit {
		limit = serverMax
	}
	if int64(buffer.Len()) > limit {
		return nil, newError(CodePayloadTooLarge, 0, "the encoded contact exceeds its resource limit")
	}
	return buffer.Bytes(), nil
}

func encodePatchedCard(raw *rawCard, card vcard.Card, patch ContactPatch, serverMax int64) ([]byte, error) {
	if raw == nil {
		return nil, newError(CodeInternalError, 0, "the contact raw representation is unavailable")
	}
	patched := cloneCard(card)
	if err := applyPatch(patched, patch); err != nil {
		return nil, err
	}
	names := patchedPropertyNames(patch)
	replacements, err := canonicalProperties(patched, names)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		selected[name] = true
	}
	emitted := make(map[string]bool, len(names))
	var output bytes.Buffer
	for index, property := range raw.properties {
		if index == len(raw.properties)-1 {
			for _, name := range names {
				if !emitted[name] {
					for _, replacement := range replacements[name] {
						_, _ = output.Write(replacement)
					}
					emitted[name] = true
				}
			}
		}
		if !selected[property.name] {
			_, _ = output.Write(property.raw)
			continue
		}
		if !emitted[property.name] {
			for _, replacement := range replacements[property.name] {
				_, _ = output.Write(replacement)
			}
			emitted[property.name] = true
		}
	}
	limit := int64(maxCardBytes)
	if serverMax > 0 && serverMax < limit {
		limit = serverMax
	}
	if int64(output.Len()) > limit {
		return nil, newError(CodePayloadTooLarge, 0, "the encoded contact exceeds its resource limit")
	}
	encoded := output.Bytes()
	if _, version, _, err := decodeRawCard(encoded); err != nil {
		return nil, err
	} else if version != "3.0" {
		return nil, validationError("only vCard 3.0 contacts can be updated")
	}
	return append([]byte(nil), encoded...), nil
}

func cloneCard(card vcard.Card) vcard.Card {
	cloned := make(vcard.Card, len(card))
	for name, fields := range card {
		cloned[name] = append([]*vcard.Field(nil), fields...)
	}
	return cloned
}

func patchedPropertyNames(patch ContactPatch) []string {
	var names []string
	fields := []struct {
		present bool
		name    string
	}{
		{patch.DisplayName != nil, vcard.FieldFormattedName},
		{patch.Name != nil, vcard.FieldName},
		{patch.Organization != nil, vcard.FieldOrganization},
		{patch.Title != nil, vcard.FieldTitle},
		{patch.Nickname != nil, vcard.FieldNickname},
		{patch.Birthday != nil, vcard.FieldBirthday},
		{patch.Notes != nil, vcard.FieldNote},
		{patch.Emails != nil, vcard.FieldEmail},
		{patch.Phones != nil, vcard.FieldTelephone},
		{patch.Addresses != nil, vcard.FieldAddress},
		{patch.URLs != nil, vcard.FieldURL},
	}
	for _, field := range fields {
		if field.present {
			names = append(names, field.name)
		}
	}
	return names
}

func canonicalProperties(card vcard.Card, names []string) (map[string][][]byte, error) {
	canonical := make(vcard.Card)
	canonical.SetValue(vcard.FieldVersion, "3.0")
	canonical.SetValue(vcard.FieldUID, "canonical-placeholder")
	canonical.SetValue(vcard.FieldFormattedName, "canonical-placeholder")
	canonical.SetValue(vcard.FieldName, "canonical-placeholder;;;;")
	for _, name := range names {
		if fields := card[name]; len(fields) > 0 {
			canonical[name] = fields
		} else {
			delete(canonical, name)
		}
	}
	encoded, err := encodeCard(canonical, 0)
	if err != nil {
		return nil, err
	}
	_, _, parsed, err := decodeRawCard(encoded)
	if err != nil {
		return nil, newError(CodeInternalError, 0, "failed to encode the contact patch")
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	properties := make(map[string][][]byte, len(names))
	for _, property := range parsed.properties {
		if wanted[property.name] {
			properties[property.name] = append(properties[property.name], property.raw)
		}
	}
	return properties, nil
}

func cardSummary(card vcard.Card, addressBook, etag string) ContactSummary {
	uid := truncateUTF8(card.Value(vcard.FieldUID), maxUIDBytes)
	display := truncateUTF8(card.Value(vcard.FieldFormattedName), maxDisplayNameBytes)
	organization := truncateUTF8(firstComponent(card.Value(vcard.FieldOrganization)), maxTextBytes)
	return ContactSummary{
		AddressBook:  addressBook,
		UID:          uid,
		ETag:         readableETag(etag),
		DisplayName:  display,
		Organization: organization,
		Emails:       modeledTypedValues(card[vcard.FieldEmail], maxEmails, maxEmailBytes),
		Phones:       modeledTypedValues(card[vcard.FieldTelephone], maxPhones, maxPhoneBytes),
		IsGroup:      cardIsGroup(card),
	}
}

func cardDetail(card vcard.Card, version, addressBook, etag string) *Contact {
	detail := &Contact{
		ContactSummary: cardSummary(card, addressBook, etag),
		Version:        version,
		Title:          truncateUTF8(card.Value(vcard.FieldTitle), maxTextBytes),
		Nickname:       truncateUTF8(card.Value(vcard.FieldNickname), maxTextBytes),
		Addresses:      modeledAddresses(card[vcard.FieldAddress]),
		URLs:           modeledTypedValues(card[vcard.FieldURL], maxURLs, maxURLBytes),
		Notes:          truncateUTF8(card.Value(vcard.FieldNote), maxNotesBytes),
		HasPhoto:       len(card[vcard.FieldPhoto]) > 0,
	}
	if name := card.Name(); name != nil {
		detail.Name = &StructuredName{
			FamilyName:      truncateUTF8(name.FamilyName, maxTextBytes),
			GivenName:       truncateUTF8(name.GivenName, maxTextBytes),
			AdditionalName:  truncateUTF8(name.AdditionalName, maxTextBytes),
			HonorificPrefix: truncateUTF8(name.HonorificPrefix, maxTextBytes),
			HonorificSuffix: truncateUTF8(name.HonorificSuffix, maxTextBytes),
		}
	}
	birthday := card.Value(vcard.FieldBirthday)
	if birthday != "" {
		if validateBirthday(birthday) == nil {
			detail.Birthday = birthday
		} else {
			detail.UnsupportedFields = append(detail.UnsupportedFields, "birthday")
		}
	}
	return detail
}

func modeledTypedValues(fields []*vcard.Field, maxCount, maxBytes int) []TypedValue {
	if len(fields) > maxCount {
		fields = fields[:maxCount]
	}
	values := make([]TypedValue, 0, len(fields))
	for _, field := range fields {
		if field == nil {
			continue
		}
		valueType := ""
		if types := field.Params.Types(); len(types) > 0 {
			valueType = truncateUTF8(types[0], maxTypeBytes)
		}
		values = append(values, TypedValue{Type: valueType, Value: truncateUTF8(field.Value, maxBytes)})
	}
	return values
}

func modeledAddresses(fields []*vcard.Field) []PostalAddress {
	if len(fields) > maxAddresses {
		fields = fields[:maxAddresses]
	}
	addresses := make([]PostalAddress, 0, len(fields))
	for _, field := range fields {
		if field == nil {
			continue
		}
		card := vcard.Card{vcard.FieldAddress: []*vcard.Field{field}}
		address := card.Address()
		if address == nil {
			continue
		}
		valueType := ""
		if types := field.Params.Types(); len(types) > 0 {
			valueType = truncateUTF8(types[0], maxTypeBytes)
		}
		addresses = append(addresses, PostalAddress{
			Type:            valueType,
			PostOfficeBox:   truncateUTF8(address.PostOfficeBox, maxTextBytes),
			ExtendedAddress: truncateUTF8(address.ExtendedAddress, maxTextBytes),
			StreetAddress:   truncateUTF8(address.StreetAddress, maxTextBytes),
			Locality:        truncateUTF8(address.Locality, maxTextBytes),
			Region:          truncateUTF8(address.Region, maxTextBytes),
			PostalCode:      truncateUTF8(address.PostalCode, maxTextBytes),
			Country:         truncateUTF8(address.Country, maxTextBytes),
		})
	}
	return addresses
}

func cardIsGroup(card vcard.Card) bool {
	return card.Kind() == vcard.KindGroup || strings.EqualFold(strings.TrimSpace(card.Value(appleGroupField)), "group")
}

func readableETag(etag string) string {
	if _, err := validateServerETag(etag); err == nil {
		return strings.TrimSpace(etag)
	}
	return ""
}

func firstComponent(value string) string {
	if index := strings.IndexByte(value, ';'); index >= 0 {
		return value[:index]
	}
	return value
}

func newV3Card(input *CreateContactInput, uid string) vcard.Card {
	card := make(vcard.Card)
	card.SetValue(vcard.FieldVersion, "3.0")
	card.SetValue(vcard.FieldProductID, "-//icloud-mcp//Contacts//EN")
	card.SetValue(vcard.FieldUID, uid)
	display := input.DisplayName
	if strings.TrimSpace(display) == "" {
		display = displayFromName(input.Name)
	}
	card.SetValue(vcard.FieldFormattedName, display)
	setName(card, input.Name)
	setSingle(card, vcard.FieldOrganization, input.Organization)
	setSingle(card, vcard.FieldTitle, input.Title)
	setSingle(card, vcard.FieldNickname, input.Nickname)
	setSingle(card, vcard.FieldBirthday, input.Birthday)
	setSingle(card, vcard.FieldNote, input.Notes)
	setTypedValues(card, vcard.FieldEmail, input.Emails)
	setTypedValues(card, vcard.FieldTelephone, input.Phones)
	setAddresses(card, input.Addresses)
	setTypedValues(card, vcard.FieldURL, input.URLs)
	return card
}

func applyPatch(card vcard.Card, patch ContactPatch) error {
	if patch.DisplayName != nil {
		setSingle(card, vcard.FieldFormattedName, *patch.DisplayName)
	}
	if patch.Name != nil {
		setName(card, *patch.Name)
	}
	if patch.Organization != nil {
		setSingle(card, vcard.FieldOrganization, *patch.Organization)
	}
	if patch.Title != nil {
		setSingle(card, vcard.FieldTitle, *patch.Title)
	}
	if patch.Nickname != nil {
		setSingle(card, vcard.FieldNickname, *patch.Nickname)
	}
	if patch.Birthday != nil {
		setSingle(card, vcard.FieldBirthday, *patch.Birthday)
	}
	if patch.Notes != nil {
		setSingle(card, vcard.FieldNote, *patch.Notes)
	}
	if patch.Emails != nil {
		setTypedValues(card, vcard.FieldEmail, *patch.Emails)
	}
	if patch.Phones != nil {
		setTypedValues(card, vcard.FieldTelephone, *patch.Phones)
	}
	if patch.Addresses != nil {
		setAddresses(card, *patch.Addresses)
	}
	if patch.URLs != nil {
		setTypedValues(card, vcard.FieldURL, *patch.URLs)
	}
	if strings.TrimSpace(card.Value(vcard.FieldFormattedName)) == "" {
		name := structuredNameFromCard(card)
		if nameEmpty(name) {
			return validationError("the updated contact must retain a display name or structured name")
		}
		card.SetValue(vcard.FieldFormattedName, displayFromName(name))
	}
	if card.Get(vcard.FieldName) == nil {
		setName(card, StructuredName{})
	}
	return nil
}

func setSingle(card vcard.Card, key, value string) {
	if value == "" {
		delete(card, key)
		return
	}
	card.SetValue(key, value)
}

func setName(card vcard.Card, name StructuredName) {
	card.SetName(&vcard.Name{
		FamilyName:      name.FamilyName,
		GivenName:       name.GivenName,
		AdditionalName:  name.AdditionalName,
		HonorificPrefix: name.HonorificPrefix,
		HonorificSuffix: name.HonorificSuffix,
	})
}

func setTypedValues(card vcard.Card, key string, values []TypedValue) {
	delete(card, key)
	for _, value := range values {
		field := &vcard.Field{Value: value.Value}
		if value.Type != "" {
			field.Params = vcard.Params{vcard.ParamType: []string{value.Type}}
		}
		card.Add(key, field)
	}
}

func setAddresses(card vcard.Card, values []PostalAddress) {
	delete(card, vcard.FieldAddress)
	for _, value := range values {
		field := &vcard.Field{}
		if value.Type != "" {
			field.Params = vcard.Params{vcard.ParamType: []string{value.Type}}
		}
		card.AddAddress(&vcard.Address{
			Field:           field,
			PostOfficeBox:   value.PostOfficeBox,
			ExtendedAddress: value.ExtendedAddress,
			StreetAddress:   value.StreetAddress,
			Locality:        value.Locality,
			Region:          value.Region,
			PostalCode:      value.PostalCode,
			Country:         value.Country,
		})
	}
}

func structuredNameFromCard(card vcard.Card) StructuredName {
	name := card.Name()
	if name == nil {
		return StructuredName{}
	}
	return StructuredName{
		FamilyName:      name.FamilyName,
		GivenName:       name.GivenName,
		AdditionalName:  name.AdditionalName,
		HonorificPrefix: name.HonorificPrefix,
		HonorificSuffix: name.HonorificSuffix,
	}
}

func displayFromName(name StructuredName) string {
	parts := []string{name.HonorificPrefix, name.GivenName, name.AdditionalName, name.FamilyName, name.HonorificSuffix}
	nonempty := parts[:0]
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			nonempty = append(nonempty, strings.TrimSpace(part))
		}
	}
	return strings.Join(nonempty, " ")
}

func uuidV4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", newError(CodeInternalError, 0, "failed to generate a contact identifier")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func normalizeForSort(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
