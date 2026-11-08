package imapadapter

import (
	"reflect"
	"strings"
	"testing"
)

func TestCuratedHeadersCanonicalThreadIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, headers, id     string
		inReplyTo, references []string
	}{
		{"source", "Message-ID: <source@example.invalid>\r\n", "<source@example.invalid>", nil, nil},
		{"thread", "Message-ID: <source@example.invalid>\r\nIn-Reply-To: <first@example.invalid> <second@example.invalid>\r\nReferences: <first@example.invalid>\r\n <second@example.invalid>\r\n", "<source@example.invalid>", []string{"<first@example.invalid>", "<second@example.invalid>"}, []string{"<first@example.invalid>", "<second@example.invalid>"}},
		{"absent", "Subject: Synthetic\r\n", "", nil, nil},
		{"empty", "Message-ID:\r\nIn-Reply-To:\r\nReferences:\r\n", "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCuratedHeaders([]byte(tc.headers + "\r\n"))
			t.Logf("curated=%+v error=%v", got, err)
			if err != nil || got.MessageID != tc.id || !reflect.DeepEqual(got.InReplyTo, tc.inReplyTo) || !reflect.DeepEqual(got.References, tc.references) {
				t.Fatalf("canonical thread identity lost: %+v, %v", got, err)
			}
		})
	}
}

func TestCuratedHeadersCanonicalBoundsAndRefusal(t *testing.T) {
	for _, field := range []string{"Message-ID", "In-Reply-To", "References"} {
		for _, length := range []int{maxMetadataString, maxMetadataString + 1} {
			t.Run(field+"/"+strings.Repeat("x", length-maxMetadataString+1), func(t *testing.T) {
				id := "<" + strings.Repeat("x", length-len("<@example.invalid>")) + "@example.invalid>"
				got, err := parseCuratedHeaders([]byte(field + ": " + id + "\r\n\r\n"))
				if length > maxMetadataString {
					if !isAdapterKind(err, ErrorTooLarge) {
						t.Fatalf("canonical value escaped existing bound: %+v %v", got, err)
					}
				} else if err != nil || field == "Message-ID" && got.MessageID != id || field == "In-Reply-To" && !reflect.DeepEqual(got.InReplyTo, []string{id}) || field == "References" && !reflect.DeepEqual(got.References, []string{id}) {
					t.Fatalf("exact canonical bound rejected or altered: %+v %v", got, err)
				}
			})
		}
		for _, bad := range []string{"not-an-id", "<unterminated@example.invalid"} {
			t.Run(field+"/malformed/"+bad, func(t *testing.T) {
				if got, err := parseCuratedHeaders([]byte(field + ": " + bad + "\r\n\r\n")); !isAdapterKind(err, ErrorProtocol) {
					t.Fatalf("malformed header accepted: %+v %v", got, err)
				}
			})
		}
	}
	if got, err := parseCuratedHeaders([]byte("X-Synthetic: " + strings.Repeat("x", maxHeaderBytes) + "\r\n\r\n")); !isAdapterKind(err, ErrorProtocol) {
		t.Fatalf("oversized header accepted: %+v %v", got, err)
	}
}
