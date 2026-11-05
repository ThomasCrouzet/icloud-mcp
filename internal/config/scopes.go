package config

import (
	"fmt"
	"strings"
)

const resourceScopesEnvVar = "ICLOUD_MCP_RESOURCE_SCOPES"

const (
	ResourceCalendarRead   = "calendar.read"
	ResourceCalendarWrite  = "calendar.write"
	ResourceCalendarDelete = "calendar.delete"
	ResourceContactsRead   = "contacts.read"
	ResourceContactsWrite  = "contacts.write"
	ResourceContactsDelete = "contacts.delete"
	ResourceMailRead       = "mail.read"
	ResourceMailMutate     = "mail.mutate"
	ResourceMailArchive    = "mail.archive"
)

var resourceScopeKeys = map[string]bool{
	ResourceCalendarRead: true, ResourceCalendarWrite: true, ResourceCalendarDelete: true,
	ResourceContactsRead: true, ResourceContactsWrite: true, ResourceContactsDelete: true,
	ResourceMailRead: true, ResourceMailMutate: true, ResourceMailArchive: true,
}

// ResourceScopes is an immutable boot-time resource allowlist. Its zero value
// imposes no restriction. A configured action with no matching resources is
// still fail-closed at the operation path.
type ResourceScopes struct {
	configured bool
	values     map[string][]string
}

// ParseResourceScopes parses ICLOUD_MCP_RESOURCE_SCOPES.
//
// Grammar: action=value[,value...][;action=value...]
// A single value "*" means no restriction for that action.
func ParseResourceScopes(value string) (ResourceScopes, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return ResourceScopes{}, nil
	}
	scopes := ResourceScopes{configured: true, values: make(map[string][]string)}
	entries := strings.Split(value, ";")
	if len(entries) > 64 {
		return ResourceScopes{}, fmt.Errorf("invalid %s: too many entries", resourceScopesEnvVar)
	}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return ResourceScopes{}, fmt.Errorf("invalid %s: empty entry", resourceScopesEnvVar)
		}
		equals := strings.IndexByte(entry, '=')
		if equals <= 0 || equals == len(entry)-1 {
			return ResourceScopes{}, fmt.Errorf("invalid %s: expected action=value", resourceScopesEnvVar)
		}
		key := strings.TrimSpace(entry[:equals])
		if !resourceScopeKeys[key] {
			return ResourceScopes{}, fmt.Errorf("invalid %s: unknown action", resourceScopesEnvVar)
		}
		if _, duplicate := scopes.values[key]; duplicate {
			return ResourceScopes{}, fmt.Errorf("invalid %s: duplicate action", resourceScopesEnvVar)
		}
		raw := strings.TrimSpace(entry[equals+1:])
		if raw == "*" {
			scopes.values[key] = []string{"*"}
			continue
		}
		if strings.Contains(raw, "*") {
			return ResourceScopes{}, fmt.Errorf("invalid %s: wildcard is valid only as the complete value", resourceScopesEnvVar)
		}
		parts := strings.Split(raw, ",")
		if len(parts) > 64 {
			return ResourceScopes{}, fmt.Errorf("invalid %s: too many resources", resourceScopesEnvVar)
		}
		seen := make(map[string]bool, len(parts))
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" || len(part) > 1024 || strings.ContainsAny(part, "\r\n\x00") {
				return ResourceScopes{}, fmt.Errorf("invalid %s: empty or invalid resource", resourceScopesEnvVar)
			}
			if strings.HasPrefix(key, "calendar.") && !validScopeCalendarPath(part) {
				return ResourceScopes{}, fmt.Errorf("invalid %s: invalid calendar path", resourceScopesEnvVar)
			}
			if seen[part] {
				return ResourceScopes{}, fmt.Errorf("invalid %s: duplicate resource", resourceScopesEnvVar)
			}
			seen[part] = true
			values = append(values, part)
		}
		scopes.values[key] = values
	}
	return scopes, nil
}

func validScopeCalendarPath(value string) bool {
	if value == "" || len(value) > 1024 || !strings.HasPrefix(value, "/") || strings.Contains(value, "://") || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return false
		}
	}
	return true
}

// Configured reports whether any resource scope was set.
func (s ResourceScopes) Configured() bool { return s.configured }

func (s ResourceScopes) forAction(action string, fallbacks ...string) ([]string, bool) {
	if !s.configured {
		return nil, false
	}
	for _, candidate := range append([]string{action}, fallbacks...) {
		if candidate == "" {
			continue
		}
		if values, ok := s.values[candidate]; ok {
			if len(values) == 1 && values[0] == "*" {
				return nil, false
			}
			return append([]string(nil), values...), true
		}
	}
	return nil, false
}

// Calendar returns the configured resources for one Calendar action. Delete
// falls back to write when no delete scope is set.
func (s ResourceScopes) Calendar(action string) ([]string, bool) {
	if action == ResourceCalendarWrite {
		return s.forAction(action, ResourceCalendarRead)
	}
	if action == ResourceCalendarDelete {
		return s.forAction(action, ResourceCalendarWrite, ResourceCalendarRead)
	}
	return s.forAction(action)
}

// Contacts returns the configured resources for one Contacts action. Delete
// falls back to write when no delete scope is set.
func (s ResourceScopes) Contacts(action string) ([]string, bool) {
	if action == ResourceContactsWrite {
		return s.forAction(action, ResourceContactsRead)
	}
	if action == ResourceContactsDelete {
		return s.forAction(action, ResourceContactsWrite, ResourceContactsRead)
	}
	return s.forAction(action)
}

// Mail returns the configured resources for one Mail action.
func (s ResourceScopes) Mail(action string) ([]string, bool) {
	switch action {
	case ResourceMailMutate:
		return s.forAction(action, ResourceMailRead)
	case ResourceMailArchive:
		return s.forAction(action, ResourceMailMutate, ResourceMailRead)
	default:
		return s.forAction(action)
	}
}
