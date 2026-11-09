package contacts

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
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
	if raw == nil || len(raw.properties) < 5 {
		return invalidCardError()
	}
	first := raw.properties[0]
	last := raw.properties[len(raw.properties)-1]
	if first.group != "" || first.hasParams || first.name != "BEGIN" || !strings.EqualFold(first.value, "VCARD") ||
		last.group != "" || last.hasParams || last.name != "END" || !strings.EqualFold(last.value, "VCARD") {
		return invalidCardError()
	}
	counts := make(map[string]int)
	var version, uid *rawProperty
	var formattedNames, names []*rawProperty
	for index, property := range raw.properties {
		if index > 0 && index < len(raw.properties)-1 && (property.name == "BEGIN" || property.name == "END") {
			return invalidCardError()
		}
		counts[property.name]++
		switch property.name {
		case vcard.FieldVersion:
			version = &raw.properties[index]
		case vcard.FieldUID:
			uid = &raw.properties[index]
		case vcard.FieldFormattedName:
			formattedNames = append(formattedNames, &raw.properties[index])
		case vcard.FieldName:
			names = append(names, &raw.properties[index])
		}
	}
	if raw.properties[1].name != vcard.FieldVersion || counts[vcard.FieldVersion] != 1 || counts[vcard.FieldUID] != 1 {
		return invalidCardError()
	}
	if version == nil || version.hasParams || version.value != "3.0" && version.value != "4.0" {
		return invalidCardError()
	}
	v4 := version.value == "4.0"
	if len(formattedNames) == 0 || !v4 && (len(formattedNames) != 1 || len(names) != 1) || v4 && len(names) > 1 {
		return invalidCardError()
	}
	for _, property := range []*rawProperty{version, uid} {
		if property.group != "" {
			return invalidCardError()
		}
	}
	for _, property := range append(formattedNames, names...) {
		if property.group != "" {
			return invalidCardError()
		}
	}
	if strings.TrimSpace(uid.value) == "" {
		return invalidCardError()
	}
	displayName := false
	for _, property := range formattedNames {
		if strings.TrimSpace(property.value) != "" {
			displayName = true
			break
		}
	}
	if !displayName {
		return invalidCardError()
	}
	for _, property := range names {
		if countUnescaped(property.value, ';') != 4 {
			return invalidCardError()
		}
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

// encodeNewCard encodes a created contact and rewrites N, ADR and ORG with
// escape-aware structured values, because go-vcard does not escape embedded
// semicolons in compound properties.
func encodeNewCard(input *CreateContactInput, uid string, serverMax int64) ([]byte, error) {
	encoded, err := encodeCard(newV3Card(input, uid), 0)
	if err != nil {
		return nil, err
	}
	nameValue := escapeStructuredComponents([]string{
		input.Name.FamilyName, input.Name.GivenName, input.Name.AdditionalName,
		input.Name.HonorificPrefix, input.Name.HonorificSuffix,
	})
	addressValues := make([]string, 0, len(input.Addresses))
	for _, address := range input.Addresses {
		addressValues = append(addressValues, escapeStructuredComponents([]string{
			address.PostOfficeBox, address.ExtendedAddress, address.StreetAddress,
			address.Locality, address.Region, address.PostalCode, address.Country,
		}))
	}
	rewritten := rewriteStructuredLines(encoded, nameValue, escapeTextValue(input.Organization), addressValues)
	limit := int64(maxCardBytes)
	if serverMax > 0 && serverMax < limit {
		limit = serverMax
	}
	if int64(len(rewritten)) > limit {
		return nil, newError(CodePayloadTooLarge, 0, "the encoded contact exceeds its resource limit")
	}
	return rewritten, nil
}

// rewriteStructuredLines replaces the value of N, ORG and ADR lines while
// preserving their property parameters and document order.
func rewriteStructuredLines(encoded []byte, nameValue, organization string, addresses []string) []byte {
	lines := strings.SplitAfter(string(encoded), "\r\n")
	addressIndex := 0
	for index, line := range lines {
		body := strings.TrimSuffix(line, "\r\n")
		colon, err := delimiterOutsideQuotes([]byte(body), ':')
		if err != nil || colon < 0 {
			continue
		}
		header := body[:colon+1]
		propertyName := body[:colon]
		if dot := strings.IndexByte(propertyName, '.'); dot >= 0 {
			propertyName = propertyName[dot+1:]
		}
		if semicolon := strings.IndexByte(propertyName, ';'); semicolon >= 0 {
			propertyName = propertyName[:semicolon]
		}
		switch strings.ToUpper(propertyName) {
		case "N":
			lines[index] = header + nameValue + "\r\n"
		case "ORG":
			lines[index] = header + organization + "\r\n"
		case "ADR":
			if addressIndex < len(addresses) {
				lines[index] = header + addresses[addressIndex] + "\r\n"
				addressIndex++
			}
		}
	}
	return []byte(strings.Join(lines, ""))
}

func escapeTextValue(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\r", "", ";", "\\;", ",", "\\,").Replace(value)
}

func escapeStructuredComponents(components []string) string {
	escaped := make([]string, len(components))
	for index, component := range components {
		escaped[index] = escapeTextValue(component)
	}
	return strings.Join(escaped, ";")
}

// structuredPatchProperties builds escape-aware raw properties for a patch
// that changes a compound property. go-vcard joins components with unescaped
// semicolons, so these replacements are constructed directly.
func structuredPatchProperties(patch ContactPatch) map[string][]rawProperty {
	properties := make(map[string][]rawProperty)
	if patch.Name != nil {
		value := escapeStructuredComponents([]string{
			patch.Name.FamilyName, patch.Name.GivenName, patch.Name.AdditionalName,
			patch.Name.HonorificPrefix, patch.Name.HonorificSuffix,
		})
		properties[vcard.FieldName] = []rawProperty{{name: vcard.FieldName, value: value, raw: []byte("N:" + value + "\r\n")}}
	}
	if patch.Organization != nil {
		value := escapeTextValue(*patch.Organization)
		properties[vcard.FieldOrganization] = []rawProperty{{name: vcard.FieldOrganization, value: value, raw: []byte("ORG:" + value + "\r\n")}}
	}
	if patch.Addresses != nil {
		var addresses []rawProperty
		for _, address := range *patch.Addresses {
			value := escapeStructuredComponents([]string{
				address.PostOfficeBox, address.ExtendedAddress, address.StreetAddress,
				address.Locality, address.Region, address.PostalCode, address.Country,
			})
			header := "ADR:"
			if address.Type != "" {
				header = "ADR;TYPE=" + address.Type + ":"
			}
			addresses = append(addresses, rawProperty{name: vcard.FieldAddress, value: value, raw: []byte(header + value + "\r\n")})
		}
		properties[vcard.FieldAddress] = addresses
	}
	return properties
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
	structured := structuredPatchProperties(patch)
	canonicalNames := make([]string, 0, len(names))
	for _, name := range names {
		if _, handled := structured[name]; !handled {
			canonicalNames = append(canonicalNames, name)
		}
	}
	replacements, err := canonicalProperties(patched, canonicalNames)
	if err != nil {
		return nil, err
	}
	for name, properties := range structured {
		replacements[name] = properties
	}
	assignedGroups, orphanGroups, err := planGroupBindings(raw, replacements, names)
	if err != nil {
		return nil, err
	}
	selected := make(map[string]bool, len(names))
	for _, name := range names {
		selected[name] = true
	}
	emitted := make(map[string]bool, len(names))
	var output bytes.Buffer
	emitReplacements := func(name string) {
		for index, replacement := range replacements[name] {
			group := assignedGroups[name][index]
			_, _ = output.Write(renderGroupedProperty(replacement, group))
		}
		emitted[name] = true
	}
	for index, property := range raw.properties {
		if index == len(raw.properties)-1 {
			for _, name := range names {
				if !emitted[name] {
					emitReplacements(name)
				}
			}
		}
		if !selected[property.name] {
			if property.group != "" && orphanGroups[property.group] {
				continue
			}
			_, _ = output.Write(property.raw)
			continue
		}
		if !emitted[property.name] {
			emitReplacements(property.name)
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

// planGroupBindings preserves Apple grouped property metadata (for example
// item1.EMAIL plus item1.X-ABLabel) when an edited value keeps its identity.
// A group whose original value is no longer present is dropped together with
// its companion metadata instead of leaving an orphan label. An ambiguous
// correspondence fails before any conditional PUT.
func planGroupBindings(raw *rawCard, replacements map[string][]rawProperty, names []string) (map[string]map[int]string, map[string]bool, error) {
	assigned := make(map[string]map[int]string, len(names))
	orphans := make(map[string]bool)
	for _, name := range names {
		type groupedValue struct {
			group string
			value string
		}
		var originals []groupedValue
		for _, property := range raw.properties {
			if property.group != "" && property.name == name {
				originals = append(originals, groupedValue{group: property.group, value: decodeTextValue(property.value)})
			}
		}
		if len(originals) == 0 {
			continue
		}
		byValue := make(map[string][]string)
		var values []string
		for _, original := range originals {
			if _, seen := byValue[original.value]; !seen {
				values = append(values, original.value)
			}
			byValue[original.value] = append(byValue[original.value], original.group)
		}
		used := make(map[int]bool)
		for _, value := range values {
			groups := byValue[value]
			var matches []int
			for index, replacement := range replacements[name] {
				if !used[index] && decodeTextValue(replacement.value) == value {
					matches = append(matches, index)
				}
			}
			if len(matches) > 0 && len(matches) < len(groups) {
				return nil, nil, validationError("the edited contact has ambiguous grouped values; keep or remove them explicitly")
			}
			if len(matches) == 0 {
				for _, group := range groups {
					orphans[group] = true
				}
				continue
			}
			for index, group := range groups {
				used[matches[index]] = true
				if assigned[name] == nil {
					assigned[name] = make(map[int]string)
				}
				assigned[name][matches[index]] = group
			}
		}
	}
	return assigned, orphans, nil
}

func renderGroupedProperty(property rawProperty, group string) []byte {
	if group == "" {
		return property.raw
	}
	rendered := make([]byte, 0, len(property.raw)+len(group)+1)
	rendered = append(rendered, group...)
	rendered = append(rendered, '.')
	return append(rendered, property.raw...)
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

func canonicalProperties(card vcard.Card, names []string) (map[string][]rawProperty, error) {
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
	properties := make(map[string][]rawProperty, len(names))
	for _, property := range parsed.properties {
		if wanted[property.name] {
			properties[property.name] = append(properties[property.name], property)
		}
	}
	return properties, nil
}

// preferredFormattedName selects one deterministic display name from the FN
// alternatives. A PREF parameter wins (lower value first), then document
// order. Empty alternatives are ignored.
func preferredFormattedName(card vcard.Card) string {
	selected := ""
	selectedPref := 101
	for _, field := range card[vcard.FieldFormattedName] {
		if field == nil || strings.TrimSpace(field.Value) == "" {
			continue
		}
		pref := 100
		if value := field.Params.Get(vcard.ParamPreferred); value != "" {
			if parsed, err := strconv.Atoi(value); err == nil {
				pref = parsed
			}
		} else if field.Params.HasType("pref") {
			pref = 1
		}
		if selected == "" || pref < selectedPref {
			selected = field.Value
			selectedPref = pref
		}
	}
	return selected
}

// cardView pairs a decoded card with its folded-line raw properties. The raw
// values are needed for structured fields because go-vcard does not decode
// escaped semicolons, and its partial decoding makes a post-decode split
// ambiguous for a literal backslash that precedes a separator.
type cardView struct {
	card vcard.Card
	raw  *rawCard
}

func newCardView(card vcard.Card, raw *rawCard) cardView {
	return cardView{card: card, raw: raw}
}

// values returns the unfolded wire values for every property with the given
// name, in document order. It falls back to the decoder output when no raw
// representation was retained.
func (v cardView) values(name string) []string {
	if v.raw != nil {
		var values []string
		for _, property := range v.raw.properties {
			if property.name == name {
				values = append(values, property.value)
			}
		}
		return values
	}
	return v.card.Values(name)
}

func (v cardView) firstValue(name string) string {
	if values := v.values(name); len(values) > 0 {
		return values[0]
	}
	return ""
}

// structuredComponents splits a compound value on unescaped semicolons and
// unescapes each component. It returns a typed error for malformed escapes.
func structuredComponents(value string) ([]string, error) {
	var components []string
	var current strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch character {
		case '\\':
			if index+1 >= len(value) {
				return nil, invalidCardError()
			}
			index++
			current.WriteByte(unescapedVCardByte(value[index]))
		case ';':
			components = append(components, current.String())
			current.Reset()
		default:
			current.WriteByte(character)
		}
	}
	components = append(components, current.String())
	return components, nil
}

func unescapedVCardByte(value byte) byte {
	switch value {
	case 'n', 'N':
		return '\n'
	default:
		return value
	}
}

// decodeTextValue unescapes a single vCard text value. Escaped semicolons and
// commas stay literal instead of acting as separators.
func decodeTextValue(value string) string {
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '\\' && index+1 < len(value) {
			index++
			decoded.WriteByte(unescapedVCardByte(value[index]))
			continue
		}
		decoded.WriteByte(value[index])
	}
	return decoded.String()
}

// searchableText returns the decoded components joined with semicolons so a
// query such as "Doe;Smith" matches the decoded structured value.
func searchableText(value string) string {
	components, err := structuredComponents(value)
	if err != nil {
		return decodeTextValue(value)
	}
	return strings.Join(components, ";")
}

func cardSummary(view cardView, addressBook, etag string) ContactSummary {
	uid := truncateUTF8(view.card.Value(vcard.FieldUID), maxUIDBytes)
	display := truncateUTF8(preferredFormattedName(view.card), maxDisplayNameBytes)
	organization := ""
	if components, err := structuredComponents(view.firstValue(vcard.FieldOrganization)); err == nil && len(components) > 0 {
		organization = truncateUTF8(components[0], maxTextBytes)
	}
	emails, emailTruncation := modeledTypedValues(view.card[vcard.FieldEmail], maxEmails, maxEmailBytes, "emails")
	phones, phoneTruncation := modeledTypedValues(view.card[vcard.FieldTelephone], maxPhones, maxPhoneBytes, "phones")
	return ContactSummary{
		AddressBook:     addressBook,
		UID:             uid,
		ETag:            readableETag(etag),
		DisplayName:     display,
		Organization:    organization,
		Emails:          emails,
		Phones:          phones,
		IsGroup:         cardIsGroup(view.card),
		TruncatedFields: appendFieldTruncation(nil, emailTruncation, phoneTruncation),
	}
}

func cardDetail(view cardView, version, addressBook, etag string) *Contact {
	detail := &Contact{
		ContactSummary: cardSummary(view, addressBook, etag),
		Version:        version,
		Title:          truncateUTF8(decodeTextValue(view.firstValue(vcard.FieldTitle)), maxTextBytes),
		Nickname:       truncateUTF8(decodeTextValue(view.firstValue(vcard.FieldNickname)), maxTextBytes),
		HasPhoto:       len(view.card[vcard.FieldPhoto]) > 0,
	}
	addresses, addressTruncation := modeledAddresses(view.card[vcard.FieldAddress])
	urls, urlTruncation := modeledTypedValues(view.card[vcard.FieldURL], maxURLs, maxURLBytes, "urls")
	detail.Addresses = addresses
	detail.URLs = urls
	detail.TruncatedFields = appendFieldTruncation(detail.TruncatedFields, addressTruncation, urlTruncation)
	detail.Notes = truncateUTF8(decodeTextValue(view.firstValue(vcard.FieldNote)), maxNotesBytes)
	if rawName := view.firstValue(vcard.FieldName); rawName != "" {
		if components, err := structuredComponents(rawName); err == nil && len(components) == 5 {
			detail.Name = &StructuredName{
				FamilyName:      truncateUTF8(components[0], maxTextBytes),
				GivenName:       truncateUTF8(components[1], maxTextBytes),
				AdditionalName:  truncateUTF8(components[2], maxTextBytes),
				HonorificPrefix: truncateUTF8(components[3], maxTextBytes),
				HonorificSuffix: truncateUTF8(components[4], maxTextBytes),
			}
		}
	}
	birthday := view.card.Value(vcard.FieldBirthday)
	if birthday != "" {
		if validateBirthday(birthday) == nil {
			detail.Birthday = birthday
		} else {
			detail.UnsupportedFields = append(detail.UnsupportedFields, "birthday")
		}
	}
	return detail
}

// FieldTruncation reports bounded, field-level completeness metadata. It never
// carries omitted private values.
type FieldTruncation struct {
	Field          string `json:"field"`
	Omitted        int    `json:"omitted,omitempty"`
	ValueTruncated bool   `json:"valueTruncated,omitempty"`
}

func appendFieldTruncation(target []FieldTruncation, items ...FieldTruncation) []FieldTruncation {
	for _, item := range items {
		if item.Omitted > 0 || item.ValueTruncated {
			target = append(target, item)
		}
	}
	return target
}

func modeledTypedValues(fields []*vcard.Field, maxCount, maxBytes int, fieldName string) ([]TypedValue, FieldTruncation) {
	truncation := FieldTruncation{Field: fieldName}
	if len(fields) > maxCount {
		truncation.Omitted = len(fields) - maxCount
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
		if len(field.Value) > maxBytes {
			truncation.ValueTruncated = true
		}
		values = append(values, TypedValue{Type: valueType, Value: truncateUTF8(field.Value, maxBytes)})
	}
	return values, truncation
}

func modeledAddresses(fields []*vcard.Field) ([]PostalAddress, FieldTruncation) {
	truncation := FieldTruncation{Field: "addresses"}
	if len(fields) > maxAddresses {
		truncation.Omitted = len(fields) - maxAddresses
		fields = fields[:maxAddresses]
	}
	addresses := make([]PostalAddress, 0, len(fields))
	for _, field := range fields {
		if field == nil {
			continue
		}
		components, err := structuredComponents(field.Value)
		if err != nil || len(components) < 7 {
			continue
		}
		valueType := ""
		if types := field.Params.Types(); len(types) > 0 {
			valueType = truncateUTF8(types[0], maxTypeBytes)
		}
		address := PostalAddress{Type: valueType}
		parts := []*string{
			&address.PostOfficeBox, &address.ExtendedAddress, &address.StreetAddress,
			&address.Locality, &address.Region, &address.PostalCode, &address.Country,
		}
		for index, part := range parts {
			if len(components[index]) > maxTextBytes {
				truncation.ValueTruncated = true
			}
			*part = truncateUTF8(components[index], maxTextBytes)
		}
		addresses = append(addresses, address)
	}
	return addresses, truncation
}

func cardIsGroup(card vcard.Card) bool {
	return card.Kind() == vcard.KindGroup || strings.EqualFold(strings.TrimSpace(card.Value(appleGroupField)), "group")
}

// contactContentFingerprint compares the modeled content of an existing
// contact. Resource identity, ETag, and truncation metadata are excluded.
func contactContentFingerprint(object *addressObject) string {
	return contactFingerprint(newCardView(object.card, object.raw), object.version)
}

func contactFingerprint(view cardView, version string) string {
	detail := cardDetail(view, version, "", "")
	detail.ETag = ""
	// Keep truncation metadata: an omitted eleventh email or a clipped value
	// changes the comparison and must not look like an identical retry.
	detail.MatchedFieldOmitted = false
	encoded, err := json.Marshal(detail)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func readableETag(etag string) string {
	if _, err := validateServerETag(etag); err == nil {
		return strings.TrimSpace(etag)
	}
	return ""
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

// NormalizeSortName is the canonical Contacts sort key. One function is used
// by the service, cross-book aggregation, and cursor continuation.
func NormalizeSortName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeForSort(value string) string {
	return NormalizeSortName(value)
}
