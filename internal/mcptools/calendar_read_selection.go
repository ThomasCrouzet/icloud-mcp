package mcptools

import (
	"context"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// Explicit selections reject the entire request if any path is forbidden.
// Discovery filters before event queries and preserves the permitted input order.
func selectCalendarReadPaths(ctx context.Context, deps Deps, paths []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, restricted := calendarReadScope(deps.ResourceScopes)
	if len(paths) > 0 {
		for _, path := range paths {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if restricted && !scopeContains(scope, path) {
				return nil, icloud.NewError(icloud.CodeAuthorization, 0, "calendar selection is outside the configured resource scope", nil)
			}
		}
		return paths, nil
	}

	calendars, err := deps.Service.ListCalendars(ctx)
	if err != nil {
		return nil, err
	}
	for _, calendar := range calendars {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if calendar.Path == "" || (restricted && !scopeContains(scope, calendar.Path)) {
			continue
		}
		paths = append(paths, calendar.Path)
	}
	if restricted && len(paths) == 0 {
		return nil, icloud.NewError(icloud.CodeAuthorization, 0, "no calendars are within the configured resource scope", nil)
	}
	return paths, nil
}
