package icloud

import (
	"strings"
	"sync"
)

const maxCachedEventLocations = 1024

type eventLocation struct {
	path string
}

type eventLocationCache struct {
	mu      sync.Mutex
	entries map[string]eventLocation
	order   []string
}

func newEventLocationCache() *eventLocationCache {
	return &eventLocationCache{entries: make(map[string]eventLocation)}
}

func eventLocationKey(calendarPath, uid string) string {
	return strings.TrimSuffix(calendarPath, "/") + "\x00" + uid
}

func (c *eventLocationCache) get(calendarPath, uid string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[eventLocationKey(calendarPath, uid)]
	return entry.path, ok
}

func (c *eventLocationCache) put(calendarPath, uid, path string) {
	if c == nil || path == "" {
		return
	}
	if !strings.HasPrefix(path, strings.TrimSuffix(calendarPath, "/")+"/") {
		return
	}
	key := eventLocationKey(calendarPath, uid)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		if len(c.order) >= maxCachedEventLocations {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.entries, oldest)
		}
		c.order = append(c.order, key)
	}
	c.entries[key] = eventLocation{path: path}
}

func (c *eventLocationCache) evict(calendarPath, uid string) {
	if c == nil {
		return
	}
	key := eventLocationKey(calendarPath, uid)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		return
	}
	delete(c.entries, key)
	for index, candidate := range c.order {
		if candidate == key {
			c.order = append(c.order[:index], c.order[index+1:]...)
			break
		}
	}
}
