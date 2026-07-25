package contacts

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"
)

const (
	davNamespace     = "DAV:"
	carddavNamespace = "urn:ietf:params:xml:ns:carddav"

	maxMultiStatusXMLDepth      = 32
	maxMultiStatusXMLTokens     = 100000
	maxMultiStatusPropstats     = 8192
	maxMultiStatusPropertyCount = 16384
)

var errAddressDataTooLarge = errors.New("address-data exceeds card limit")

type multiStatusResponse struct {
	Href      string        `xml:"href"`
	Propstats []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}

type davProp struct {
	CurrentUserPrincipal   *hrefProperty      `xml:"current-user-principal"`
	AddressBookHomeSet     *hrefProperty      `xml:"urn:ietf:params:xml:ns:carddav addressbook-home-set"`
	DisplayName            string             `xml:"displayname"`
	AddressBookDescription string             `xml:"urn:ietf:params:xml:ns:carddav addressbook-description"`
	ResourceType           *resourceType      `xml:"resourcetype"`
	SupportedAddressData   *supportedData     `xml:"urn:ietf:params:xml:ns:carddav supported-address-data"`
	MaxResourceSize        string             `xml:"urn:ietf:params:xml:ns:carddav max-resource-size"`
	GetETags               []string           `xml:"getetag"`
	AddressData            limitedAddressData `xml:"urn:ietf:params:xml:ns:carddav address-data"`
}

type hrefProperty struct {
	Hrefs []string `xml:"href"`
}

type resourceType struct {
	AddressBook *struct{} `xml:"urn:ietf:params:xml:ns:carddav addressbook"`
}

type supportedData struct {
	Types []addressDataType `xml:"urn:ietf:params:xml:ns:carddav address-data-type"`
}

type addressDataType struct {
	ContentType string `xml:"content-type,attr"`
	Version     string `xml:"version,attr"`
}

type limitedAddressData struct {
	Value   string
	Present bool
}

func (value *limitedAddressData) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	value.Present = true
	var data bytes.Buffer
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token := token.(type) {
		case xml.CharData:
			if data.Len()+len(token) > maxCardBytes {
				return errAddressDataTooLarge
			}
			_, _ = data.Write(token)
		case xml.EndElement:
			if token.Name == start.Name {
				value.Value = data.String()
				return nil
			}
		}
	}
}

func decodeMultiStatus(data []byte, responseLimit int) ([]multiStatusResponse, bool, error) {
	if err := validateMultiStatusXML(data, responseLimit); err != nil {
		return nil, false, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var responses []multiStatusResponse
	inRoot := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if !inRoot {
				return nil, false, newError(CodeProtocolError, 0, "Contacts response was not a DAV multistatus document")
			}
			return responses, false, nil
		}
		if err != nil {
			if errors.Is(err, errAddressDataTooLarge) || strings.Contains(err.Error(), errAddressDataTooLarge.Error()) {
				return nil, false, newError(CodePayloadTooLarge, 0, "one vCard exceeded its byte limit")
			}
			return nil, false, newError(CodeProtocolError, 0, "Contacts returned malformed DAV XML")
		}
		switch element := token.(type) {
		case xml.StartElement:
			if !inRoot {
				if element.Name.Space != davNamespace || element.Name.Local != "multistatus" {
					return nil, false, newError(CodeProtocolError, 0, "Contacts response was not a DAV multistatus document")
				}
				inRoot = true
				continue
			}
			if element.Name.Space == davNamespace && element.Name.Local == "response" {
				if len(responses) >= responseLimit {
					return responses, true, nil
				}
				var response multiStatusResponse
				if err := decoder.DecodeElement(&response, &element); err != nil {
					if errors.Is(err, errAddressDataTooLarge) || strings.Contains(err.Error(), errAddressDataTooLarge.Error()) {
						return nil, false, newError(CodePayloadTooLarge, 0, "one vCard exceeded its byte limit")
					}
					return nil, false, newError(CodeProtocolError, 0, "Contacts returned malformed DAV XML")
				}
				responses = append(responses, response)
			}
		}
	}
}

// validateMultiStatusXML bounds recursive and repeated XML structure before
// DecodeElement can allocate slices for one response. It deliberately stops at
// responseLimit+1 because decodeMultiStatus ignores the overflowing response.
func validateMultiStatusXML(data []byte, responseLimit int) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	tokens := 0
	responses := 0
	propstats := 0
	properties := 0
	propDepth := 0
	rootSeen := false
	rootClosed := false

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if !rootSeen || !rootClosed || depth != 0 {
				return newError(CodeProtocolError, 0, "Contacts response was not a DAV multistatus document")
			}
			return nil
		}
		if err != nil {
			return newError(CodeProtocolError, 0, "Contacts returned malformed DAV XML")
		}
		tokens++
		if tokens > maxMultiStatusXMLTokens {
			return newError(CodePayloadTooLarge, 0, "Contacts DAV XML exceeded its token limit")
		}

		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if depth > maxMultiStatusXMLDepth {
				return newError(CodePayloadTooLarge, 0, "Contacts DAV XML exceeded its depth limit")
			}
			if rootClosed {
				return newError(CodeProtocolError, 0, "Contacts returned malformed DAV XML")
			}
			if !rootSeen {
				if value.Name.Space != davNamespace || value.Name.Local != "multistatus" {
					return newError(CodeProtocolError, 0, "Contacts response was not a DAV multistatus document")
				}
				rootSeen = true
				continue
			}
			if value.Name.Space == davNamespace && value.Name.Local == "response" {
				responses++
				if responses > responseLimit {
					return nil
				}
			}
			if value.Name.Space == davNamespace && value.Name.Local == "propstat" {
				propstats++
				if propstats > maxMultiStatusPropstats {
					return newError(CodePayloadTooLarge, 0, "Contacts DAV XML exceeded its propstat limit")
				}
			}
			if propDepth != 0 && depth == propDepth+1 {
				properties++
				if properties > maxMultiStatusPropertyCount {
					return newError(CodePayloadTooLarge, 0, "Contacts DAV XML exceeded its property limit")
				}
			}
			if value.Name.Space == davNamespace && value.Name.Local == "prop" && propDepth == 0 {
				propDepth = depth
			}
		case xml.EndElement:
			if propDepth == depth && value.Name.Space == davNamespace && value.Name.Local == "prop" {
				propDepth = 0
			}
			if depth == 1 && value.Name.Space == davNamespace && value.Name.Local == "multistatus" {
				rootClosed = true
			}
			depth--
		case xml.CharData:
			if (!rootSeen || rootClosed) && len(bytes.TrimSpace(value)) != 0 {
				return newError(CodeProtocolError, 0, "Contacts returned malformed DAV XML")
			}
		}
	}
}

func successfulProp(response multiStatusResponse) *davProp {
	var merged davProp
	found := false
	for _, propstat := range response.Propstats {
		if !statusIsOK(propstat.Status) {
			continue
		}
		found = true
		prop := propstat.Prop
		if prop.CurrentUserPrincipal != nil {
			if merged.CurrentUserPrincipal == nil {
				merged.CurrentUserPrincipal = &hrefProperty{}
			}
			merged.CurrentUserPrincipal.Hrefs = append(merged.CurrentUserPrincipal.Hrefs, prop.CurrentUserPrincipal.Hrefs...)
		}
		if prop.AddressBookHomeSet != nil {
			if merged.AddressBookHomeSet == nil {
				merged.AddressBookHomeSet = &hrefProperty{}
			}
			merged.AddressBookHomeSet.Hrefs = append(merged.AddressBookHomeSet.Hrefs, prop.AddressBookHomeSet.Hrefs...)
		}
		if prop.DisplayName != "" {
			merged.DisplayName = prop.DisplayName
		}
		if prop.AddressBookDescription != "" {
			merged.AddressBookDescription = prop.AddressBookDescription
		}
		if prop.ResourceType != nil {
			merged.ResourceType = prop.ResourceType
		}
		if prop.SupportedAddressData != nil {
			merged.SupportedAddressData = prop.SupportedAddressData
		}
		if prop.MaxResourceSize != "" {
			merged.MaxResourceSize = prop.MaxResourceSize
		}
		if len(prop.GetETags) > 0 {
			merged.GetETags = append(merged.GetETags, prop.GetETags...)
		}
		if prop.AddressData.Present {
			merged.AddressData = prop.AddressData
		}
	}
	if !found {
		return nil
	}
	return &merged
}

func statusIsOK(status string) bool {
	if strings.ContainsAny(status, "\r\n") {
		return false
	}
	fields := strings.Fields(status)
	if len(fields) < 2 || fields[1] != "200" {
		return false
	}
	major, _, ok := http.ParseHTTPVersion(fields[0])
	return ok && major == 1
}
