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
	"sync"
	"time"
)

type Cache struct {
	dir        string
	mu         sync.Mutex
	entries    map[string]*entry
	maxEntries int
}

type entry struct {
	file    string
	status  int
	header  http.Header
	expires time.Time
	lastUse time.Time
}

func Open(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &Cache{dir: dir, entries: map[string]*entry{}, maxEntries: 4096}, nil
}

// Serve writes the cached response for key if a fresh entry exists.
func (c *Cache) Serve(w http.ResponseWriter, r *http.Request, key string) bool {
	c.mu.Lock()
	e := c.entries[key]
	if e == nil {
		c.mu.Unlock()
		return false
	}
	now := time.Now()
	if now.After(e.expires) {
		delete(c.entries, key)
		c.mu.Unlock()
		os.Remove(e.file)
		return false
	}
	e.lastUse = now
	c.mu.Unlock()

	f, err := os.Open(e.file)
	if err != nil {
		c.mu.Lock()
		delete(c.entries, key)
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

// Put stores a response whose body the caller already collected.
func (c *Cache) Put(key string, status int, header http.Header, body []byte, ttl time.Duration) {
	sum := md5.Sum([]byte(key))
	file := filepath.Join(c.dir, hex.EncodeToString(sum[:]))
	if err := os.WriteFile(file, body, 0644); err != nil {
		return
	}
	h := header.Clone()
	h.Del("X-Cache-Status")
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evict()
	c.entries[key] = &entry{file: file, status: status, header: h, expires: now.Add(ttl), lastUse: now}
}

// evict drops the least recently used entry once the index is full.
func (c *Cache) evict() {
	if len(c.entries) < c.maxEntries {
		return
	}
	var oldK string
	var old *entry
	for k, e := range c.entries {
		if old == nil || e.lastUse.Before(old.lastUse) {
			oldK, old = k, e
		}
	}
	if old != nil {
		delete(c.entries, oldK)
		os.Remove(old.file)
	}
}
