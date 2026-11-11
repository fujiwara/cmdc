package cmdc

import (
	"sync"
	"time"
)

type entry struct {
	label    string
	chunks   []Chunk
	size     int64
	storedAt time.Time
	ttl      time.Duration
}

// Cache is an in-memory store of command outputs.
type Cache struct {
	mu       sync.Mutex
	entries  map[string]*entry
	total    int64
	maxTotal int64
}

// NewCache creates a cache which holds up to maxTotal bytes of output.
func NewCache(maxTotal int64) *Cache {
	return &Cache{
		entries:  make(map[string]*entry),
		maxTotal: maxTotal,
	}
}

// Get returns the chunks stored for key if the entry is younger than both
// expires and the expiration given when it was stored.
func (c *Cache) Get(key string, expires time.Duration) ([]Chunk, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	age := time.Since(e.storedAt)
	if age >= e.ttl {
		c.remove(key)
		return nil, false
	}
	if age >= expires {
		return nil, false
	}
	return e.chunks, true
}

// Set stores chunks for key. It reports false if the entry is not stored
// because it does not fit in the cache. When the cache is full, the oldest
// entries are evicted.
func (c *Cache) Set(key, label string, chunks []Chunk, ttl time.Duration) bool {
	var size int64
	for _, ch := range chunks {
		size += int64(len(ch.Data))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remove(key)
	if ttl <= 0 || size > c.maxTotal {
		return false
	}
	for c.total+size > c.maxTotal {
		c.remove(c.oldest())
	}
	c.entries[key] = &entry{
		label:    label,
		chunks:   chunks,
		size:     size,
		storedAt: time.Now(),
		ttl:      ttl,
	}
	c.total += size
	return true
}

// Sweep removes expired entries.
func (c *Cache) Sweep() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, e := range c.entries {
		if time.Since(e.storedAt) >= e.ttl {
			c.remove(key)
		}
	}
}

// Clear removes all entries and returns the number of removed entries.
func (c *Cache) Clear() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.entries)
	clear(c.entries)
	c.total = 0
	return n
}

// Len returns the number of entries.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Stats returns the unexpired entries, oldest first, and their total size.
func (c *Cache) Stats() ([]StatusItem, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	type aged struct {
		item StatusItem
		age  time.Duration
	}
	var total int64
	items := make([]aged, 0, len(c.entries))
	for _, e := range c.entries {
		age := time.Since(e.storedAt)
		if age >= e.ttl {
			continue
		}
		total += e.size
		items = append(items, aged{
			item: StatusItem{
				Command:   e.label,
				Size:      e.size,
				Age:       age.Round(time.Millisecond).String(),
				ExpiresIn: (e.ttl - age).Round(time.Millisecond).String(),
			},
			age: age,
		})
	}
	// oldest first
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].age > items[j-1].age; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
	result := make([]StatusItem, len(items))
	for i, it := range items {
		result[i] = it.item
	}
	return result, total
}

func (c *Cache) remove(key string) {
	if e, ok := c.entries[key]; ok {
		c.total -= e.size
		delete(c.entries, key)
	}
}

func (c *Cache) oldest() string {
	var key string
	var t time.Time
	for k, e := range c.entries {
		if key == "" || e.storedAt.Before(t) {
			key, t = k, e.storedAt
		}
	}
	return key
}
