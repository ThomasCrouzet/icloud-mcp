package icloud

import (
	"fmt"
	"net/http"
	"strings"
)

// parseStrongETag validates one RFC 9110 strong entity-tag and returns its
// exact quoted wire representation. Backslash is an ordinary opaque-tag byte,
// not an escape, so the value must never pass through strconv.Unquote.
func parseStrongETag(value string) (string, error) {
	if len(value) > maxETagBytes {
		return "", fmt.Errorf("invalid strong entity-tag")
	}
	value = strings.Trim(value, " \t")
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", fmt.Errorf("invalid strong entity-tag")
	}
	for i := 1; i < len(value)-1; i++ {
		b := value[i]
		if b < 0x21 || b == 0x22 || b == 0x7f {
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

func strongETagFromResponse(response msResponse) (string, error) {
	var values []string
	for _, propstat := range response.Propstats {
		if isOKStatus(propstat.Status) {
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
