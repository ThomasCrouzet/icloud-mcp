// Package contacts implements bounded iCloud Contacts access over CardDAV.
package contacts

import "context"

// Aggregate Contacts search work bounds. A scoped multi-book search carries one
// budget across every authorized address book.
const (
	MaxCardsScanned = 2000
	MaxReportBytes  = 32 << 20
)

// TypedValue is a labeled contact value such as an email address or phone.
type TypedValue struct {
	Type  string `json:"type,omitempty"`
	Value string `json:"value"`
}

// StructuredName contains the five components of the vCard N property.
type StructuredName struct {
	FamilyName      string `json:"familyName,omitempty"`
	GivenName       string `json:"givenName,omitempty"`
	AdditionalName  string `json:"additionalName,omitempty"`
	HonorificPrefix string `json:"honorificPrefix,omitempty"`
	HonorificSuffix string `json:"honorificSuffix,omitempty"`
}

// PostalAddress is a labeled, structured vCard ADR value.
type PostalAddress struct {
	Type            string `json:"type,omitempty"`
	PostOfficeBox   string `json:"postOfficeBox,omitempty"`
	ExtendedAddress string `json:"extendedAddress,omitempty"`
	StreetAddress   string `json:"streetAddress,omitempty"`
	Locality        string `json:"locality,omitempty"`
	Region          string `json:"region,omitempty"`
	PostalCode      string `json:"postalCode,omitempty"`
	Country         string `json:"country,omitempty"`
}

// AddressBook is a discovered CardDAV address book. Identifier is opaque and
// is the only value callers may use to select the collection.
type AddressBook struct {
	Identifier        string   `json:"identifier"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	SupportedVersions []string `json:"supportedVersions,omitempty"`
	WriteVersion      string   `json:"writeVersion,omitempty"`
	MaxResourceSize   int64    `json:"maxResourceSize,omitempty"`
}

// ContactSummary is the bounded result model used by SearchContacts.
type ContactSummary struct {
	AddressBook     string            `json:"addressBook"`
	UID             string            `json:"uid"`
	ETag            string            `json:"etag,omitempty"`
	DisplayName     string            `json:"displayName"`
	Organization    string            `json:"organization,omitempty"`
	Emails          []TypedValue      `json:"emails,omitempty"`
	Phones          []TypedValue      `json:"phones,omitempty"`
	IsGroup         bool              `json:"isGroup,omitempty"`
	TruncatedFields []FieldTruncation `json:"truncatedFields,omitempty"`
	// MatchedFieldOmitted is true when the search filter matched a raw value
	// that the bounded output did not include.
	MatchedFieldOmitted bool `json:"matchedFieldOmitted,omitempty"`
}

// Contact is a modeled full contact. Raw vCard data and PHOTO bytes are
// deliberately not exposed (payload and PII budget). HasPhoto reports whether
// a PHOTO property is present so agents can know an avatar exists without
// receiving its bytes.
type Contact struct {
	ContactSummary
	Version           string          `json:"version"`
	Name              *StructuredName `json:"name,omitempty"`
	Title             string          `json:"title,omitempty"`
	Nickname          string          `json:"nickname,omitempty"`
	Birthday          string          `json:"birthday,omitempty"`
	Addresses         []PostalAddress `json:"addresses,omitempty"`
	URLs              []TypedValue    `json:"urls,omitempty"`
	Notes             string          `json:"notes,omitempty"`
	HasPhoto          bool            `json:"hasPhoto,omitempty"`
	UnsupportedFields []string        `json:"unsupportedFields,omitempty"`
}

// SearchOptions selects address books and applies bounded local filters.
type SearchOptions struct {
	AddressBook   string `json:"address_book,omitempty"`
	Query         string `json:"query,omitempty"`
	Email         string `json:"email,omitempty"`
	Phone         string `json:"phone,omitempty"`
	IncludeGroups bool   `json:"include_groups,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	// RemainingCards and RemainingBytes carry a caller-owned aggregate scan
	// budget across several address books. Zero means the full default.
	RemainingCards int   `json:"-"`
	RemainingBytes int64 `json:"-"`
	// PageAll keeps the complete bounded match set for a cursor page instead
	// of truncating to Limit. The caller applies the page size.
	PageAll bool `json:"-"`
}

// SearchResult reports summaries and whether either result or scan bounds
// prevented a complete response.
type SearchResult struct {
	Contacts         []ContactSummary `json:"contacts"`
	Truncated        bool             `json:"truncated,omitempty"`
	ScanLimitReached bool             `json:"scanLimitReached,omitempty"`
	// ScannedCards and UsedBytes report the work this call consumed, so a
	// caller can carry one aggregate budget across books.
	ScannedCards int    `json:"-"`
	UsedBytes    int64  `json:"-"`
	NextCursor   string `json:"nextCursor,omitempty"`
}

// CreateContactInput contains the editable vCard 3.0 fields accepted on create.
type CreateContactInput struct {
	AddressBook  string          `json:"address_book"`
	DisplayName  string          `json:"display_name,omitempty"`
	Name         StructuredName  `json:"name,omitempty"`
	Organization string          `json:"organization,omitempty"`
	Title        string          `json:"title,omitempty"`
	Nickname     string          `json:"nickname,omitempty"`
	Birthday     string          `json:"birthday,omitempty"`
	Notes        string          `json:"notes,omitempty"`
	Emails       []TypedValue    `json:"emails,omitempty"`
	Phones       []TypedValue    `json:"phones,omitempty"`
	Addresses    []PostalAddress `json:"addresses,omitempty"`
	URLs         []TypedValue    `json:"urls,omitempty"`
	ClientUID    string          `json:"client_uid,omitempty"`
}

// ContactPatch uses pointers to distinguish omitted fields from explicit
// empty values. A pointer to an empty slice clears that repeated property.
type ContactPatch struct {
	DisplayName  *string          `json:"display_name,omitempty"`
	Name         *StructuredName  `json:"name,omitempty"`
	Organization *string          `json:"organization,omitempty"`
	Title        *string          `json:"title,omitempty"`
	Nickname     *string          `json:"nickname,omitempty"`
	Birthday     *string          `json:"birthday,omitempty"`
	Notes        *string          `json:"notes,omitempty"`
	Emails       *[]TypedValue    `json:"emails,omitempty"`
	Phones       *[]TypedValue    `json:"phones,omitempty"`
	Addresses    *[]PostalAddress `json:"addresses,omitempty"`
	URLs         *[]TypedValue    `json:"urls,omitempty"`
}

// UpdateContactInput identifies a contact and supplies a field patch. ETag is
// optional, but when present it is used instead of the ETag from the full GET.
type UpdateContactInput struct {
	AddressBook string       `json:"address_book"`
	UID         string       `json:"uid"`
	ETag        string       `json:"etag,omitempty"`
	Patch       ContactPatch `json:"patch"`
}

// DeleteContactInput identifies a contact for conditional deletion.
type DeleteContactInput struct {
	AddressBook string `json:"address_book"`
	UID         string `json:"uid"`
	ETag        string `json:"etag,omitempty"`
	DryRun      bool   `json:"dry_run,omitempty"`
}

// CreateResult describes a definitively applied create.
type CreateResult struct {
	AddressBook      string `json:"addressBook"`
	UID              string `json:"uid"`
	ETag             string `json:"etag,omitempty"`
	ResultIncomplete bool   `json:"resultIncomplete,omitempty"`
	Warning          string `json:"warning,omitempty"`
	// Reconciled is true when a supplied client_uid already identified a
	// contact with identical modeled content. No second resource was created.
	Reconciled bool `json:"reconciled,omitempty"`
}

// UpdateResult describes a definitively applied update.
type UpdateResult struct {
	AddressBook      string `json:"addressBook"`
	UID              string `json:"uid"`
	ETag             string `json:"etag,omitempty"`
	ResultIncomplete bool   `json:"resultIncomplete,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

// DeleteResult describes a delete or dry-run result.
type DeleteResult struct {
	AddressBook string `json:"addressBook"`
	UID         string `json:"uid"`
	DryRun      bool   `json:"dryRun,omitempty"`
	WouldDelete bool   `json:"wouldDelete,omitempty"`
}

// Service is the Contacts API consumed by MCP integration and test doubles.
type Service interface {
	ListAddressBooks(ctx context.Context) ([]AddressBook, error)
	SearchContacts(ctx context.Context, opts SearchOptions) (SearchResult, error)
	GetContact(ctx context.Context, addressBook, uid string) (*Contact, error)
	CreateContact(ctx context.Context, input *CreateContactInput) (CreateResult, error)
	UpdateContact(ctx context.Context, input *UpdateContactInput) (UpdateResult, error)
	DeleteContact(ctx context.Context, input *DeleteContactInput) (DeleteResult, error)
}
