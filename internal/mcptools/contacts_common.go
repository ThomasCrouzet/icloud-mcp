package mcptools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
	"github.com/ThomasCrouzet/icloud-mcp/internal/security"
)

const (
	contactsMaxAddressBooks    = 100
	contactsDefaultSearchLimit = 50
	contactsMaxSearchLimit     = 100
	contactsMaxQueryBytes      = 256
	contactsMaxEmailBytes      = 320
	contactsMaxPhoneBytes      = 64
	contactsMaxDisplayBytes    = 500
	contactsMaxTextBytes       = 500
	contactsMaxNotesBytes      = 4000
	contactsMaxURLBytes        = 2048
	contactsMaxTypeBytes       = 64
	contactsMaxUIDBytes        = 255
	contactsMaxEmails          = 10
	contactsMaxPhones          = 10
	contactsMaxURLs            = 5
	contactsMaxAddresses       = 5
	contactsMaxResultBytes     = 256 << 10
)

// ContactsDeps groups the dependencies used by Contacts tool handlers.
type ContactsDeps struct {
	Service  contacts.Service
	Audit    *security.AuditLogger
	Redactor *security.Redactor
}

type contactsRawArgs map[string]json.RawMessage

func parseContactsArgs(req mcp.CallToolRequest, allowed ...string) (contactsRawArgs, error) {
	raw := req.GetRawArguments()
	if raw == nil {
		return contactsRawArgs{}, nil
	}

	var data []byte
	switch value := raw.(type) {
	case json.RawMessage:
		data = value
	case []byte:
		data = value
	default:
		var err error
		data, err = json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("arguments must be a JSON object")
		}
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}

	var args contactsRawArgs
	if err := json.Unmarshal(trimmed, &args); err != nil {
		return nil, fmt.Errorf("arguments must be a valid JSON object")
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name := range args {
		if _, ok := allowedSet[name]; !ok {
			return nil, fmt.Errorf("unknown parameter %q", name)
		}
	}
	return args, nil
}

func parseContactsObject(raw json.RawMessage, path string, allowed ...string) (contactsRawArgs, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%s must be an object", path)
	}
	var object contactsRawArgs
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, fmt.Errorf("%s must be a valid object", path)
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name := range object {
		if _, ok := allowedSet[name]; !ok {
			return nil, fmt.Errorf("unknown parameter %q in %s", name, path)
		}
	}
	return object, nil
}

func contactsString(args contactsRawArgs, key string, required bool, maxBytes int, singleLine bool) (string, bool, error) {
	raw, exists := args[key]
	if !exists {
		if required {
			return "", false, fmt.Errorf("required parameter %q not found", key)
		}
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, fmt.Errorf("%s must be a string", key)
	}
	if !utf8.ValidString(value) {
		return "", false, fmt.Errorf("%s must be valid UTF-8", key)
	}
	if maxBytes > 0 && len(value) > maxBytes {
		return "", false, fmt.Errorf("%s exceeds its %d-byte limit", key, maxBytes)
	}
	for _, r := range value {
		if r == 0 || (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return "", false, fmt.Errorf("%s contains a disallowed control character", key)
		}
	}
	if singleLine && strings.ContainsAny(value, "\r\n") {
		return "", false, fmt.Errorf("%s must be a single line", key)
	}
	return value, true, nil
}

func contactsRequiredIdentifier(args contactsRawArgs, key string, addressBook bool) (string, error) {
	value, _, err := contactsString(args, key, true, contactsMaxUIDBytes, true)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s cannot be empty", key)
	}
	if addressBook && !validContactsAddressBook(value) {
		return "", fmt.Errorf("address_book must be an identifier returned by list_address_books")
	}
	return value, nil
}

func validContactsAddressBook(value string) bool {
	if len(value) != len("book-")+22 || !strings.HasPrefix(value, "book-") {
		return false
	}
	for _, r := range value[len("book-"):] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func contactsOptionalBool(args contactsRawArgs, key string, fallback bool) (bool, error) {
	raw, exists := args[key]
	if !exists {
		return fallback, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}

func contactsOptionalInt(args contactsRawArgs, key string, fallback, min, max int) (int, error) {
	raw, exists := args[key]
	if !exists {
		return fallback, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	if value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return value, nil
}

func contactsArray(args contactsRawArgs, key string, maxItems int) ([]json.RawMessage, bool, error) {
	raw, exists := args[key]
	if !exists {
		return nil, false, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false, fmt.Errorf("%s must be an array", key)
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, false, fmt.Errorf("%s must be a valid array", key)
	}
	if len(values) > maxItems {
		return nil, false, fmt.Errorf("%s may contain at most %d items", key, maxItems)
	}
	return values, true, nil
}

func parseContactsName(raw json.RawMessage, path string) (contacts.StructuredName, error) {
	object, err := parseContactsObject(raw, path,
		"familyName", "givenName", "additionalName", "honorificPrefix", "honorificSuffix")
	if err != nil {
		return contacts.StructuredName{}, err
	}
	values := make(map[string]string, len(object))
	for _, key := range []string{"familyName", "givenName", "additionalName", "honorificPrefix", "honorificSuffix"} {
		value, exists, parseErr := contactsString(object, key, false, contactsMaxTextBytes, true)
		if parseErr != nil {
			return contacts.StructuredName{}, fmt.Errorf("%s.%w", path, parseErr)
		}
		if exists {
			values[key] = value
		}
	}
	return contacts.StructuredName{
		FamilyName:      values["familyName"],
		GivenName:       values["givenName"],
		AdditionalName:  values["additionalName"],
		HonorificPrefix: values["honorificPrefix"],
		HonorificSuffix: values["honorificSuffix"],
	}, nil
}

func parseContactsTypedValues(args contactsRawArgs, key string, maxItems, maxValueBytes int) ([]contacts.TypedValue, bool, error) {
	items, exists, err := contactsArray(args, key, maxItems)
	if err != nil || !exists {
		return nil, exists, err
	}
	values := make([]contacts.TypedValue, 0, len(items))
	for index, item := range items {
		path := fmt.Sprintf("%s[%d]", key, index)
		object, parseErr := parseContactsObject(item, path, "type", "value")
		if parseErr != nil {
			return nil, true, parseErr
		}
		kind, _, parseErr := contactsString(object, "type", false, contactsMaxTypeBytes, true)
		if parseErr != nil {
			return nil, true, fmt.Errorf("%s.%w", path, parseErr)
		}
		if kind != "" && !validContactsType(kind) {
			return nil, true, fmt.Errorf("%s.type contains an invalid character", path)
		}
		value, _, parseErr := contactsString(object, "value", true, maxValueBytes, true)
		if parseErr != nil {
			return nil, true, fmt.Errorf("%s.%w", path, parseErr)
		}
		if strings.TrimSpace(value) == "" {
			return nil, true, fmt.Errorf("%s.value cannot be empty", path)
		}
		values = append(values, contacts.TypedValue{Type: kind, Value: value})
	}
	return values, true, nil
}

func parseContactsAddresses(args contactsRawArgs, key string) ([]contacts.PostalAddress, bool, error) {
	items, exists, err := contactsArray(args, key, contactsMaxAddresses)
	if err != nil || !exists {
		return nil, exists, err
	}
	values := make([]contacts.PostalAddress, 0, len(items))
	keys := []string{"type", "postOfficeBox", "extendedAddress", "streetAddress", "locality", "region", "postalCode", "country"}
	for index, item := range items {
		path := fmt.Sprintf("%s[%d]", key, index)
		object, parseErr := parseContactsObject(item, path, keys...)
		if parseErr != nil {
			return nil, true, parseErr
		}
		parts := make(map[string]string, len(object))
		for _, field := range keys {
			limit := contactsMaxTextBytes
			if field == "type" {
				limit = contactsMaxTypeBytes
			}
			value, present, fieldErr := contactsString(object, field, false, limit, true)
			if fieldErr != nil {
				return nil, true, fmt.Errorf("%s.%w", path, fieldErr)
			}
			if present {
				parts[field] = value
			}
		}
		if parts["type"] != "" && !validContactsType(parts["type"]) {
			return nil, true, fmt.Errorf("%s.type contains an invalid character", path)
		}
		values = append(values, contacts.PostalAddress{
			Type:            parts["type"],
			PostOfficeBox:   parts["postOfficeBox"],
			ExtendedAddress: parts["extendedAddress"],
			StreetAddress:   parts["streetAddress"],
			Locality:        parts["locality"],
			Region:          parts["region"],
			PostalCode:      parts["postalCode"],
			Country:         parts["country"],
		})
	}
	return values, true, nil
}

func validContactsType(value string) bool {
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func contactsNameEmpty(name contacts.StructuredName) bool {
	return strings.TrimSpace(name.FamilyName) == "" &&
		strings.TrimSpace(name.GivenName) == "" &&
		strings.TrimSpace(name.AdditionalName) == "" &&
		strings.TrimSpace(name.HonorificPrefix) == "" &&
		strings.TrimSpace(name.HonorificSuffix) == ""
}

func contactsValidateETag(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 512 {
		return fmt.Errorf("etag must be a specific strong ETag")
	}
	trimmed := strings.Trim(value, " \t")
	if len(trimmed) < 2 || trimmed[0] != '"' || trimmed[len(trimmed)-1] != '"' {
		return fmt.Errorf("etag must be a specific strong ETag")
	}
	for index := 1; index < len(trimmed)-1; index++ {
		valueByte := trimmed[index]
		if valueByte < 0x21 || valueByte == '"' || valueByte == 0x7f {
			return fmt.Errorf("etag must be a specific strong ETag")
		}
	}
	return nil
}

func contactsValidationResult(deps ContactsDeps, err error) *mcp.CallToolResult {
	return contactsErrorResult(deps.Redactor, "validation", &contacts.Error{
		Code:    contacts.CodeValidation,
		Message: err.Error(),
	})
}

func contactsErrorResult(redactor *security.Redactor, context string, err error) *mcp.CallToolResult {
	if redactor == nil {
		redactor = security.NewRedactor()
	}
	payload := toolErrorPayload{Code: string(contacts.CodeInternalError)}
	typed := contacts.AsError(err)
	if typed == nil {
		payload.Message = context + ": Contacts operation failed unexpectedly"
	} else {
		payload.Code = string(typed.Code)
		message := typed.Message
		if message == "" {
			message = "Contacts operation failed"
		}
		payload.Message = context + ": " + message
		payload.Retryable = typed.Retryable
		if typed.Code == contacts.CodeOutcomeUnknown {
			payload.Reconciliation = typed.Reconciliation
		}
		if typed.RetryAfter > 0 {
			seconds := int(typed.RetryAfter / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			if seconds > 60 {
				seconds = 60
			}
			payload.RetryAfter = seconds
		}
	}
	payload.Message = boundedUTF8(redactor.Redact(payload.Message), maxErrorMessageBytes)
	payload.Reconciliation = boundedUTF8(redactor.Redact(payload.Reconciliation), maxErrorDetailBytes)
	encoded, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		return mcp.NewToolResultError(redactor.Redact("Contacts operation failed"))
	}
	return mcp.NewToolResultError(string(encoded))
}

func contactsWriteJSON(deps ContactsDeps, payload any) *mcp.CallToolResult {
	redactor := deps.Redactor
	if redactor == nil {
		redactor = security.NewRedactor()
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return contactsErrorResult(redactor, "formatting response", nil)
	}
	if len(redactor.Redact(string(encoded))) > contactsMaxResultBytes {
		return contactsErrorResult(redactor, "formatting response", &contacts.Error{
			Code:    contacts.CodePayloadTooLarge,
			Message: "Contacts result exceeds its byte limit",
		})
	}
	return writeJSON(redactor, payload)
}

func contactsAudit(deps ContactsDeps, tool, resource, status string) {
	if deps.Audit != nil {
		deps.Audit.LogDomainMutation(tool, "contacts", "contact", resource, status)
	}
}

func contactsAuditErrorStatus(err error) string {
	typed := contacts.AsError(err)
	if typed == nil {
		return "error"
	}
	switch typed.Code {
	case contacts.CodeValidation:
		return "denied"
	case contacts.CodeOutcomeUnknown:
		return "outcome_unknown"
	default:
		return "error"
	}
}

func contactsRawString(args contactsRawArgs, key string) string {
	value, _, _ := contactsString(args, key, false, 0, false)
	return value
}

func contactsStringProperty(description string, maxLength int) map[string]any {
	property := map[string]any{"type": "string", "description": description}
	if maxLength > 0 {
		property["maxLength"] = maxLength
	}
	return property
}

func contactsNameSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"familyName":      contactsStringProperty("Family name", contactsMaxTextBytes),
			"givenName":       contactsStringProperty("Given name", contactsMaxTextBytes),
			"additionalName":  contactsStringProperty("Additional or middle name", contactsMaxTextBytes),
			"honorificPrefix": contactsStringProperty("Honorific prefix", contactsMaxTextBytes),
			"honorificSuffix": contactsStringProperty("Honorific suffix", contactsMaxTextBytes),
		},
	}
}

func contactsTypedValueSchema(description string, maxValueLength int) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"value"},
		"properties": map[string]any{
			"type": map[string]any{
				"type":        "string",
				"maxLength":   contactsMaxTypeBytes,
				"pattern":     `^[A-Za-z0-9_-]*$`,
				"description": "Optional vCard type label such as home, work, or cell",
			},
			"value": map[string]any{
				"type":        "string",
				"minLength":   1,
				"maxLength":   maxValueLength,
				"description": description,
			},
		},
	}
}

func contactsAddressSchema() map[string]any {
	properties := map[string]any{
		"type": contactsStringProperty("Optional vCard type label such as home or work", contactsMaxTypeBytes),
	}
	for name, description := range map[string]string{
		"postOfficeBox":   "Post office box",
		"extendedAddress": "Extended address",
		"streetAddress":   "Street address",
		"locality":        "City or locality",
		"region":          "State, province, or region",
		"postalCode":      "Postal code",
		"country":         "Country",
	} {
		properties[name] = contactsStringProperty(description, contactsMaxTextBytes)
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
}
