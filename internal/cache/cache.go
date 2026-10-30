// Package cache is a disk-backed response cache for proxy_cache.
// The index lives in memory, so the cache starts cold after a restart.
package cache

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Cache struct {
	dir        string
	mu         sync.Mutex
	entries    map[string]*entry
	varyOf     map[string][]string
	variants   map[string]map[string]bool
	maxEntries int
	closed     bool
}
type entry struct {
	file, base, index string
	status            int
	header            http.Header
	expires, lastUse  time.Time
	readers           int
	retired           bool
}

func Open(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir, entries: map[string]*entry{}, varyOf: map[string][]string{}, variants: map[string]map[string]bool{}, maxEntries: 4096}, nil
}

func (c *Cache) Serve(w http.ResponseWriter, r *http.Request, base string) bool {
	return c.ServeVary(w, r, base)
}

// ServeVary opens an immutable generation while holding the index lock. Retired
// files stay alive until their last reader closes, including on Windows.
func (c *Cache) ServeVary(w http.ResponseWriter, r *http.Request, base string) bool {
	c.mu.Lock()
	e := c.entries[c.indexKeyLocked(base, r)]
	if e == nil {
		c.mu.Unlock()
		return false
	}
	if !time.Now().Before(e.expires) {
		c.dropLocked(e)
		c.mu.Unlock()
		return false
	}
	f, err := os.Open(e.file)
	if err != nil {
		c.dropLocked(e)
		c.mu.Unlock()
		return false
	}
	e.readers++
	e.lastUse = time.Now()
	c.mu.Unlock()
	defer func() {
		f.Close()
		c.mu.Lock()
		e.readers--
		if e.retired && e.readers == 0 {
			os.Remove(e.file)
		}
		c.mu.Unlock()
	}()
	for k, vv := range e.header {
		w.Header()[k] = append([]string(nil), vv...)
	}
	w.Header().Set("X-Cache-Status", "HIT")
	w.WriteHeader(e.status)
	if r.Method != http.MethodHead {
		io.Copy(w, f)
	}
	return true
}

// Put publishes only a complete, closed file. Concurrent replacements never
// truncate a file referenced by an earlier response or overwrite its metadata.
func (c *Cache) Put(base string, vary []string, r *http.Request, status int, header http.Header, body []byte, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	index := base
	if len(vary) > 0 {
		index += "\x00" + varyKey(r, vary)
	}
	f, err := os.CreateTemp(c.dir, "entry-")
	if err != nil {
		return
	}
	_, err = f.Write(body)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		os.Remove(f.Name())
		return
	}
	h := header.Clone()
	h.Del("X-Cache-Status")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		os.Remove(f.Name())
		return
	}
	if !sameNames(c.varyOf[base], vary) {
		c.purgeLocked(base)
	}
	if old := c.entries[index]; old != nil {
		c.dropLocked(old)
	} else {
		c.evict()
	}
	if len(vary) > 0 {
		c.varyOf[base] = append([]string(nil), vary...)
	} else {
		delete(c.varyOf, base)
	}
	now := time.Now()
	c.entries[index] = &entry{file: f.Name(), base: base, index: index, status: status, header: h, expires: now.Add(ttl), lastUse: now}
	if c.variants[base] == nil {
		c.variants[base] = map[string]bool{}
	}
	c.variants[base][index] = true
}

// Close retires this instance's files without interrupting existing readers.
// The index is not persisted, so keeping these files after shutdown has no use.
func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for _, e := range c.entries {
		c.dropLocked(e)
	}
}

func (c *Cache) indexKeyLocked(base string, r *http.Request) string {
	if names := c.varyOf[base]; len(names) > 0 {
		return base + "\x00" + varyKey(r, names)
	}
	return base
}

func (c *Cache) dropLocked(e *entry) {
	if c.entries[e.index] != e {
		return
	}
	delete(c.entries, e.index)
	if set := c.variants[e.base]; set != nil {
		delete(set, e.index)
		if len(set) == 0 {
			delete(c.variants, e.base)
			delete(c.varyOf, e.base)
		}
	}
	e.retired = true
	if e.readers == 0 {
		os.Remove(e.file)
	}
}

func (c *Cache) purgeLocked(base string) {
	for k := range c.variants[base] {
		c.dropLocked(c.entries[k])
	}
}

func varyKey(r *http.Request, names []string) string {
	var b strings.Builder
	field := func(s string) {
		b.WriteString(strconv.Itoa(len(s)))
		b.WriteByte(':')
		b.WriteString(s)
	}
	for _, name := range names {
		field(strings.ToLower(name))
		values := r.Header.Values(name)
		if strings.EqualFold(name, "Host") {
			values = []string{r.Host}
		}
		b.WriteString(strconv.Itoa(len(values)))
		b.WriteByte(':')
		for _, value := range values {
			field(value)
		}
	}
	return b.String()
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *Cache) evict() {
	if len(c.entries) < c.maxEntries {
		return
	}
	var old *entry
	for _, e := range c.entries {
		if old == nil || e.lastUse.Before(old.lastUse) {
			old = e
		}
	}
	if old != nil {
		c.dropLocked(old)
	}
}
