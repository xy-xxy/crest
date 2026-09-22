// Package cache is a disk-backed response cache for proxy_cache.
// The index lives in memory, so the cache starts cold after a restart.
package cache

import (
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
}
type entry struct {
	file, base, index string
	status            int
	header            http.Header
	expires, lastUse  time.Time
}

func Open(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir, entries: map[string]*entry{}, varyOf: map[string][]string{}, variants: map[string]map[string]bool{}, maxEntries: 4096}, nil
}
func (c *Cache) Serve(w http.ResponseWriter, r *http.Request, base string) bool {
	return c.serve(w, r, base)
}

// ServeVary selects the stored variant using the response's Vary policy.
func (c *Cache) ServeVary(w http.ResponseWriter, r *http.Request, base string) bool {
	return c.serve(w, r, base)
}
func (c *Cache) serve(w http.ResponseWriter, r *http.Request, base string) bool {
	c.mu.Lock()
	e := c.entries[c.indexKeyLocked(base, r)]
	if e == nil {
		c.mu.Unlock()
		return false
	}
	now := time.Now()
	if now.After(e.expires) {
		c.dropLocked(e)
		c.mu.Unlock()
		os.Remove(e.file)
		return false
	}
	e.lastUse = now
	c.mu.Unlock()
	f, err := os.Open(e.file)
	if err != nil {
		c.mu.Lock()
		c.dropLocked(e)
		c.mu.Unlock()
		return false
	}
	defer f.Close()
	for k, vv := range e.header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Cache-Status", "HIT")
	w.WriteHeader(e.status)
	if r.Method != http.MethodHead {
		io.Copy(w, f)
	}
	return true
}
func (c *Cache) Put(base string, vary []string, r *http.Request, status int, header http.Header, body []byte, ttl time.Duration) {
	index := base
	if len(vary) > 0 {
		index += "\x00" + varyKey(r, vary)
	}
	sum := md5.Sum([]byte(index))
	file := filepath.Join(c.dir, hex.EncodeToString(sum[:]))
	if err := os.WriteFile(file, body, 0644); err != nil {
		return
	}
	h := header.Clone()
	h.Del("X-Cache-Status")
	now := time.Now()
	c.mu.Lock()
	var stale []string
	if !sameNames(c.varyOf[base], vary) {
		stale = c.purgeLocked(base)
	}
	c.evict()
	if len(vary) > 0 {
		c.varyOf[base] = append([]string(nil), vary...)
	} else {
		delete(c.varyOf, base)
	}
	e := &entry{file: file, base: base, index: index, status: status, header: h, expires: now.Add(ttl), lastUse: now}
	c.entries[index] = e
	if c.variants[base] == nil {
		c.variants[base] = map[string]bool{}
	}
	c.variants[base][index] = true
	c.mu.Unlock()
	for _, p := range stale {
		if p != file {
			os.Remove(p)
		}
	}
}
func (c *Cache) indexKeyLocked(base string, r *http.Request) string {
	if n := c.varyOf[base]; len(n) > 0 {
		return base + "\x00" + varyKey(r, n)
	}
	return base
}
func (c *Cache) dropLocked(e *entry) {
	delete(c.entries, e.index)
	if s := c.variants[e.base]; s != nil {
		delete(s, e.index)
		if len(s) == 0 {
			delete(c.variants, e.base)
			delete(c.varyOf, e.base)
		}
	}
}
func (c *Cache) purgeLocked(base string) []string {
	set := c.variants[base]
	var out []string
	for k := range set {
		if e := c.entries[k]; e != nil {
			out = append(out, e.file)
			delete(c.entries, k)
		}
	}
	delete(c.variants, base)
	delete(c.varyOf, base)
	return out
}
func varyKey(r *http.Request, names []string) string {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(strings.ToLower(n))
		b.WriteByte('=')
		b.WriteString(r.Header.Get(n))
		b.WriteByte('\x00')
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
		os.Remove(old.file)
	}
}
