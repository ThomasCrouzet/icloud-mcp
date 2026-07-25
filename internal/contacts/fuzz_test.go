package contacts

import (
	"strings"
	"testing"
)

func FuzzContactsDAVXML(f *testing.F) {
	f.Add([]byte(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/book/contact.vcf</D:href><D:propstat><D:prop><D:getetag>&quot;v1&quot;</D:getetag><C:address-data>BEGIN:VCARD&#13;&#10;VERSION:3.0&#13;&#10;UID:u1&#13;&#10;FN:Name&#13;&#10;N:Name;;;;&#13;&#10;END:VCARD&#13;&#10;</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`))
	f.Add([]byte(`<D:multistatus xmlns:D="DAV:"><D:response/><D:response/></D:multistatus>`))
	f.Add([]byte(`<D:multistatus xmlns:D="DAV:"><x><x><x></D:multistatus>`))
	f.Add([]byte(`<!DOCTYPE x [<!ENTITY y "expanded">]><D:multistatus xmlns:D="DAV:">&y;</D:multistatus>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		responses, overflow, err := decodeMultiStatus(data, 16)
		if err != nil {
			return
		}
		if len(responses) > 16 {
			t.Fatalf("decoded %d responses above the cap", len(responses))
		}
		if overflow && len(responses) != 16 {
			t.Fatalf("overflow with %d decoded responses", len(responses))
		}
	})
}

func FuzzStrictVCardDecode(f *testing.F) {
	f.Add([]byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:contact-1\r\nFN:Example Person\r\nN:Person;Example;;;\r\nEMAIL;TYPE=HOME:person@example.com\r\nEND:VCARD\r\n"))
	f.Add([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:group-1\r\nFN:Example Group\r\nN:Group;Example;;;\r\nKIND:group\r\nEND:VCARD\r\n"))
	f.Add([]byte("BEGIN:VCARD\nVERSION:3.0\nUID:folded\nFN:Folded\n Name\nN:Name;Folded;;;\nEND:VCARD\n"))
	f.Add([]byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:one\r\nUID:two\r\nFN:Duplicate\r\nN:Duplicate;;;;\r\nEND:VCARD\r\ntrailing"))
	f.Add([]byte{'B', 'E', 'G', 'I', 'N', ':', 'V', 'C', 'A', 'R', 'D', 0, '\n'})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxCardBytes+1 {
			return
		}
		card, version, err := decodeCard(data)
		if err != nil {
			return
		}
		if card == nil || version != "3.0" && version != "4.0" {
			t.Fatalf("successful strict decode returned card=%v version=%q", card != nil, version)
		}
	})
}

func TestMultiStatusStreamingLimits(t *testing.T) {
	t.Run("response cap preserves truncation", func(t *testing.T) {
		data := []byte(`<D:multistatus xmlns:D="DAV:"><D:response/><D:response/></D:multistatus>`)
		responses, overflow, err := decodeMultiStatus(data, 1)
		if err != nil || !overflow || len(responses) != 1 {
			t.Fatalf("decodeMultiStatus() = %d, %v, %v", len(responses), overflow, err)
		}
	})

	t.Run("depth cap plus one", func(t *testing.T) {
		data := `<D:multistatus xmlns:D="DAV:" xmlns:X="urn:test">` + strings.Repeat(`<X:n>`, maxMultiStatusXMLDepth) + strings.Repeat(`</X:n>`, maxMultiStatusXMLDepth) + `</D:multistatus>`
		_, _, err := decodeMultiStatus([]byte(data), 1)
		requireCode(t, err, CodePayloadTooLarge)
	})

	t.Run("property cap plus one", func(t *testing.T) {
		var data strings.Builder
		data.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:X="urn:test"><D:response><D:propstat><D:prop>`)
		for range maxMultiStatusPropertyCount + 1 {
			data.WriteString(`<X:p/>`)
		}
		data.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
		_, _, err := decodeMultiStatus([]byte(data.String()), 1)
		requireCode(t, err, CodePayloadTooLarge)
	})

	t.Run("token cap plus one", func(t *testing.T) {
		var data strings.Builder
		data.WriteString(`<D:multistatus xmlns:D="DAV:">`)
		for range maxMultiStatusXMLTokens + 1 {
			data.WriteString(`<!--x-->`)
		}
		data.WriteString(`</D:multistatus>`)
		_, _, err := decodeMultiStatus([]byte(data.String()), 1)
		requireCode(t, err, CodePayloadTooLarge)
	})

	t.Run("address data remains byte bounded", func(t *testing.T) {
		data := `<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:propstat><D:prop><C:address-data>` + strings.Repeat("x", maxCardBytes+1) + `</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
		_, _, err := decodeMultiStatus([]byte(data), 1)
		requireCode(t, err, CodePayloadTooLarge)
	})
}
