package contacts

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const principalPropfind = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D="DAV:"><D:prop><D:current-user-principal/></D:prop></D:propfind>`

const homeSetPropfind = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><C:addressbook-home-set/></D:prop></D:propfind>`

const addressBooksPropfind = `<?xml version="1.0" encoding="UTF-8"?>
<D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><D:resourcetype/><D:displayname/><C:addressbook-description/><C:supported-address-data/><C:max-resource-size/></D:prop></D:propfind>`

func (c *Client) discover(ctx context.Context) error {
	for {
		c.discoverMu.Lock()
		if c.state != nil {
			c.discoverMu.Unlock()
			return nil
		}
		if waiting := c.discovering; waiting != nil {
			c.discoverMu.Unlock()
			select {
			case <-ctx.Done():
				return newError(CodeTimeout, 0, "Contacts discovery was canceled while waiting")
			case <-waiting:
				continue
			}
		}
		attemptDone := make(chan struct{})
		c.discovering = attemptDone
		c.discoverMu.Unlock()

		attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		state, err := c.runDiscovery(attemptCtx)
		cancel()

		c.discoverMu.Lock()
		if err == nil {
			c.state = state
		}
		c.discovering = nil
		close(attemptDone)
		c.discoverMu.Unlock()
		return err
	}
}

func (c *Client) runDiscovery(ctx context.Context) (*discoveryState, error) {
	entry, err := url.Parse(c.baseURL)
	if err != nil || entry.Path == "" && entry.Host == "" {
		return nil, newError(CodeValidation, 0, "Contacts base URL is invalid")
	}
	if entry.Path == "" {
		entry.Path = "/"
	}
	if err := c.validateURL(entry); err != nil {
		return nil, err
	}

	principalResponse, err := c.propfind(ctx, entry, "0", principalPropfind, 4)
	if err != nil {
		return nil, err
	}
	if principalResponse.overflow {
		return nil, newError(CodeProtocolError, 0, "Contacts discovery returned too many principal responses")
	}
	principalHrefs := propertyHrefs(principalResponse.rows, func(prop *davProp) *hrefProperty { return prop.CurrentUserPrincipal })
	if len(principalHrefs) != 1 {
		return nil, newError(CodeProtocolError, 0, "Contacts discovery did not return exactly one current-user-principal")
	}
	principal, err := resolveAndValidate(c, principalResponse.url, principalHrefs[0])
	if err != nil {
		return nil, err
	}

	homeResponse, err := c.propfind(ctx, principal, "0", homeSetPropfind, 4)
	if err != nil {
		return nil, err
	}
	if homeResponse.overflow {
		return nil, newError(CodeProtocolError, 0, "Contacts discovery returned too many home-set responses")
	}
	homeHrefs := propertyHrefs(homeResponse.rows, func(prop *davProp) *hrefProperty { return prop.AddressBookHomeSet })
	if len(homeHrefs) == 0 {
		return nil, newError(CodeNotFound, http.StatusNotFound, "Contacts discovery returned no address-book home set")
	}
	if len(homeHrefs) > maxAddressBooks {
		return nil, newError(CodePayloadTooLarge, 0, "Contacts discovery returned too many home sets")
	}

	homes := make([]*url.URL, 0, len(homeHrefs))
	seenHomes := make(map[string]bool)
	for _, href := range homeHrefs {
		home, resolveErr := resolveAndValidate(c, homeResponse.url, href)
		if resolveErr != nil {
			return nil, resolveErr
		}
		key := canonicalURL(home)
		if seenHomes[key] {
			return nil, newError(CodeProtocolError, 0, "Contacts discovery returned a duplicate home set")
		}
		seenHomes[key] = true
		homes = append(homes, home)
	}

	state := &discoveryState{homes: homes, byID: make(map[string]bookRecord)}
	seenCollections := make(map[string]bool)
	for _, home := range homes {
		remaining := maxAddressBooks - len(state.books)
		response, propErr := c.propfind(ctx, home, "1", addressBooksPropfind, remaining+2)
		if propErr != nil {
			return nil, propErr
		}
		for _, row := range response.rows {
			prop := successfulProp(row)
			if prop == nil || prop.ResourceType == nil || prop.ResourceType.AddressBook == nil {
				continue
			}
			collection, resolveErr := resolveAndValidate(c, response.url, row.Href)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if !urlWithin(collection, home, true) {
				return nil, newError(CodeProtocolError, 0, "Contacts discovery returned an address book outside its home set")
			}
			key := canonicalURL(collection)
			if seenCollections[key] {
				return nil, newError(CodeProtocolError, 0, "Contacts discovery returned a duplicate address book")
			}
			seenCollections[key] = true
			if len(state.books) >= maxAddressBooks {
				return nil, newError(CodePayloadTooLarge, 0, "Contacts discovery exceeded the address-book limit")
			}

			versions := []string(nil)
			if prop.SupportedAddressData != nil {
				versions = sortedVersions(prop.SupportedAddressData.Types)
			}
			writeVersion := ""
			if prop.SupportedAddressData == nil || len(prop.SupportedAddressData.Types) == 0 || containsString(versions, "3.0") {
				writeVersion = "3.0"
			}
			var maxSize int64
			if prop.MaxResourceSize != "" {
				maxSize, err = strconv.ParseInt(strings.TrimSpace(prop.MaxResourceSize), 10, 64)
				if err != nil || maxSize < 0 {
					return nil, newError(CodeProtocolError, 0, "Contacts returned an invalid maximum resource size")
				}
			}
			id := opaqueBookID(collection)
			if _, exists := state.byID[id]; exists {
				return nil, newError(CodeProtocolError, 0, "Contacts address-book identifier collision")
			}
			record := bookRecord{
				url: cloneURL(collection),
				public: AddressBook{
					Identifier:        id,
					Name:              truncateUTF8(prop.DisplayName, maxDisplayNameBytes),
					Description:       truncateUTF8(prop.AddressBookDescription, maxNotesBytes),
					SupportedVersions: versions,
					WriteVersion:      writeVersion,
					MaxResourceSize:   maxSize,
				},
			}
			state.books = append(state.books, record)
			state.byID[id] = record
		}
		if response.overflow {
			return nil, newError(CodePayloadTooLarge, 0, "Contacts discovery exceeded the address-book limit")
		}
	}
	sort.Slice(state.books, func(i, j int) bool {
		left := strings.ToLower(state.books[i].public.Name)
		right := strings.ToLower(state.books[j].public.Name)
		if left != right {
			return left < right
		}
		return state.books[i].public.Identifier < state.books[j].public.Identifier
	})
	return state, nil
}

type propfindResult struct {
	rows     []multiStatusResponse
	url      *url.URL
	overflow bool
}

func (c *Client) propfind(ctx context.Context, target *url.URL, depth, body string, rowLimit int) (*propfindResult, error) {
	header := make(http.Header)
	header.Set("Content-Type", `application/xml; charset="utf-8"`)
	header.Set("Depth", depth)
	response, err := c.doDAV(ctx, "PROPFIND", target, header, []byte(body), maxPropfindBytes, nil)
	if err != nil {
		return nil, err
	}
	if response.Status != http.StatusMultiStatus {
		return nil, classifyDAVError(response, false)
	}
	rows, overflow, err := decodeMultiStatus(response.Body, rowLimit)
	if err != nil {
		return nil, err
	}
	return &propfindResult{rows: rows, url: response.URL, overflow: overflow}, nil
}

func propertyHrefs(rows []multiStatusResponse, selectProperty func(*davProp) *hrefProperty) []string {
	var hrefs []string
	for _, row := range rows {
		prop := successfulProp(row)
		if prop == nil {
			continue
		}
		property := selectProperty(prop)
		if property != nil {
			hrefs = append(hrefs, property.Hrefs...)
		}
	}
	return hrefs
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// ListAddressBooks returns a copy of the validated discovery result.
func (c *Client) ListAddressBooks(ctx context.Context) ([]AddressBook, error) {
	if err := c.discover(ctx); err != nil {
		return nil, err
	}
	books := make([]AddressBook, len(c.state.books))
	for i, record := range c.state.books {
		books[i] = record.public
		books[i].SupportedVersions = append([]string(nil), record.public.SupportedVersions...)
	}
	if !resultFits(books) {
		return nil, newError(CodePayloadTooLarge, 0, "address-book metadata exceeds the result limit")
	}
	return books, nil
}
