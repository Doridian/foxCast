package transmux

import (
	"context"
	"sync"
)

// segmentCache loads each segment once, shares it between the concurrent
// video and audio requests for it, and keeps the most recently used few.
type segmentCache struct {
	load func(int) (*segmentData, error)
	max  int

	mu      sync.Mutex
	entries map[int]*cacheEntry
	lru     []int // oldest first
}

type cacheEntry struct {
	ready chan struct{}
	data  *segmentData
	err   error
}

func newSegmentCache(max int, load func(int) (*segmentData, error)) *segmentCache {
	return &segmentCache{load: load, max: max, entries: map[int]*cacheEntry{}}
}

// get returns segment i, loading it if needed. The load itself is not
// cancelled by ctx, so an abandoned request still fills the cache.
func (c *segmentCache) get(ctx context.Context, i int) (*segmentData, error) {
	e := c.entry(i)
	select {
	case <-e.ready:
		return e.data, e.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// prefetch starts loading segment i in the background.
func (c *segmentCache) prefetch(i int) { c.entry(i) }

func (c *segmentCache) entry(i int) *cacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[i]; ok {
		c.touch(i)
		return e
	}
	e := &cacheEntry{ready: make(chan struct{})}
	c.entries[i] = e
	c.lru = append(c.lru, i)
	c.evict()
	go func() {
		e.data, e.err = c.load(i)
		close(e.ready)
		if e.err != nil {
			c.mu.Lock()
			if c.entries[i] == e {
				c.remove(i)
			}
			c.mu.Unlock()
		}
	}()
	return e
}

func (c *segmentCache) touch(i int) {
	for k, v := range c.lru {
		if v == i {
			c.lru = append(append(c.lru[:k:k], c.lru[k+1:]...), i)
			return
		}
	}
}

func (c *segmentCache) remove(i int) {
	delete(c.entries, i)
	for k, v := range c.lru {
		if v == i {
			c.lru = append(c.lru[:k:k], c.lru[k+1:]...)
			return
		}
	}
}

// evict drops the oldest completed entries beyond max. In-flight loads are
// kept so their waiters are not orphaned.
func (c *segmentCache) evict() {
	for k := 0; len(c.entries) > c.max && k < len(c.lru); {
		i := c.lru[k]
		select {
		case <-c.entries[i].ready:
			c.remove(i)
		default:
			k++
		}
	}
}
