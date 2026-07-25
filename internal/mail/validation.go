package mail

import (
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// RecipientPolicy is an immutable exact-address SMTP allowlist.
type RecipientPolicy struct {
	allowAll bool
	allowed  map[string]struct{}
}

// ParseRecipientPolicy parses literal "*" or a comma-separated list of
// unique plain addr-specs. Matching is ASCII case-insensitive.
func ParseRecipientPolicy(raw string) (RecipientPolicy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "*" {
		return RecipientPolicy{allowAll: true}, nil
	}
	if raw == "" {
		return RecipientPolicy{}, fmt.Errorf("recipient policy cannot be empty")
	}
	policy := RecipientPolicy{allowed: make(map[string]struct{})}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" || item == "*" {
			return RecipientPolicy{}, fmt.Errorf("recipient policy contains an invalid item")
		}
		addr, err := validateAddrSpec(item)
		if err != nil {
			return RecipientPolicy{}, fmt.Errorf("recipient policy contains an invalid address")
		}
		key := asciiLower(addr)
		if _, duplicate := policy.allowed[key]; duplicate {
			return RecipientPolicy{}, fmt.Errorf("recipient policy contains a duplicate address")
		}
		policy.allowed[key] = struct{}{}
	}
	return policy, nil
}

func (p RecipientPolicy) valid() bool {
	return p.allowAll || len(p.allowed) > 0
}

func (p RecipientPolicy) clone() RecipientPolicy {
	out := RecipientPolicy{allowAll: p.allowAll}
	if len(p.allowed) > 0 {
		out.allowed = make(map[string]struct{}, len(p.allowed))
		for address := range p.allowed {
			out.allowed[address] = struct{}{}
		}
	}
	return out
}

func (p RecipientPolicy) allows(address string) bool {
	if p.allowAll {
		return true
	}
	_, ok := p.allowed[asciiLower(address)]
	return ok
}

func asciiLower(value string) string {
	b := []byte(value)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func validateAddrSpec(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") || !utf8.ValidString(value) {
		return "", fmt.Errorf("invalid addr-spec")
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value {
		return "", fmt.Errorf("invalid addr-spec")
	}
	if !strings.Contains(value, "@") || len(value) > 320 {
		return "", fmt.Errorf("invalid addr-spec")
	}
	return parsed.Address, nil
}

func validateMailbox(value string) error {
	if value == "" {
		return validationError("mailbox cannot be empty")
	}
	if len(value) > MaxMetadataString || !utf8.ValidString(value) || hasDisallowedControl(value, true) {
		return validationError("mailbox contains invalid or excessive data")
	}
	return nil
}

func validateSearchValue(name, value string) error {
	if len(value) > MaxSearchValue || !utf8.ValidString(value) || hasDisallowedControl(value, false) {
		return validationError(name + " contains invalid or excessive data")
	}
	return nil
}

func hasDisallowedControl(value string, allowTab bool) bool {
	for _, r := range value {
		if r == 0x7f || r < 0x20 && (r != '\t' || !allowTab) {
			return true
		}
	}
	return false
}

func validateIdentity(mailbox string, uidValidity, uid uint32) error {
	if err := validateMailbox(mailbox); err != nil {
		return err
	}
	if uidValidity == 0 {
		return validationError("uid_validity must be non-zero")
	}
	if uid == 0 {
		return validationError("uid must be non-zero")
	}
	return nil
}

func validateFlags(input SetFlagsInput) error {
	if err := validateIdentity(input.Mailbox, input.UIDValidity, input.UID); err != nil {
		return err
	}
	if input.Operation != FlagOperationAdd && input.Operation != FlagOperationRemove {
		return validationError("operation must be add or remove")
	}
	if len(input.Flags) == 0 || len(input.Flags) > 3 {
		return validationError("flags must contain one or more supported flags")
	}
	seen := make(map[MessageFlag]struct{}, len(input.Flags))
	for _, flag := range input.Flags {
		if flag != FlagSeen && flag != FlagFlagged && flag != FlagAnswered {
			return validationError("flags contain an unsupported value")
		}
		if _, duplicate := seen[flag]; duplicate {
			return validationError("flags contain a duplicate value")
		}
		seen[flag] = struct{}{}
	}
	return nil
}

func truncateUTF8(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
