package handler

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gonx/internal/cache"
)

// CacheValid mirrors proxy_cache_valid: which statuses to cache and for how
// long when the upstream sets no caching headers. Codes[-1] means "any".
type CacheValid struct {
	Codes map[int]bool
	TTL   time.Duration
}

// maxCacheableBody bounds what one cached object may buffer in memory.
const maxCacheableBody = 8 << 20

// WrapCache serves GETs from the cache and records cacheable misses.
func WrapCache(h http.Handler, c *cache.Cache, valids []CacheValid) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
			r.Header.Get("Authorization") != "" {
			h.ServeHTTP(w, r)
			return
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		key := scheme + r.Host + r.URL.RequestURI()
		if c.Serve(w, r, key) {
			return
		}
		rec := &cacheRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if ttl := cacheTTL(rec.status, rec.header, valids); ttl > 0 && !rec.overflow && rec.wrote {
			c.Put(key, rec.status, rec.header, rec.buf.Bytes(), ttl)
		}
	})
}

// cacheRecorder tees the response into a buffer while it streams to the
// client, snapshotting headers before downstream middleware mutates them.
type cacheRecorder struct {
	http.ResponseWriter
	status   int
	header   http.Header
	buf      bytes.Buffer
	overflow bool
	wrote    bool
}

func (rec *cacheRecorder) WriteHeader(code int) {
	if !rec.wrote {
		rec.wrote = true
		rec.status = code
		rec.Header().Set("X-Cache-Status", "MISS")
		rec.header = rec.Header().Clone()
		rec.header.Del("X-Cache-Status")
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *cacheRecorder) Write(b []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	if !rec.overflow {
		if rec.buf.Len()+len(b) > maxCacheableBody {
			rec.overflow = true
			rec.buf.Reset()
		} else {
			rec.buf.Write(b)
		}
	}
	return rec.ResponseWriter.Write(b)
}

func (rec *cacheRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rec *cacheRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rec.overflow = true // hijacked responses are not cacheable
	if hj, ok := rec.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// cacheTTL follows nginx precedence: upstream Cache-Control, then Expires,
// then proxy_cache_valid. Set-Cookie responses are never cached.
func cacheTTL(status int, h http.Header, valids []CacheValid) time.Duration {
	if h == nil || h.Get("Set-Cookie") != "" {
		return 0
	}
	cc := strings.ToLower(h.Get("Cache-Control"))
	if strings.Contains(cc, "no-store") || strings.Contains(cc, "no-cache") || strings.Contains(cc, "private") {
		return 0
	}
	if i := strings.Index(cc, "max-age="); i >= 0 {
		v := cc[i+len("max-age="):]
		if j := strings.IndexAny(v, ", "); j >= 0 {
			v = v[:j]
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	if exp := h.Get("Expires"); exp != "" {
		if t, err := http.ParseTime(exp); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
			return 0
		}
	}
	for _, v := range valids {
		if v.Codes[status] || v.Codes[-1] {
			return v.TTL
		}
	}
	return 0
}
