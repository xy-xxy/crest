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

	"crest/internal/cache"
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
		if c.ServeVary(w, r, key) {
			return
		}
		// ReverseProxy's Director rewrites Host and URL in place. Restore the
		// client-visible request after the downstream handler returns so a
		// caller that reuses the request object cannot build a different cache
		// key on its next attempt.
		originalHost := r.Host
		originalURL := *r.URL
		defer func() {
			r.Host = originalHost
			*r.URL = originalURL
		}()
		rec := &cacheRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		// Only a GET may populate the entry: a HEAD carries no body, and
		// storing it under the shared key would answer later GETs with an
		// empty 200. HEAD still reads from an entry a GET created.
		if r.Method != http.MethodGet {
			return
		}
		if ttl := cacheTTL(rec.status, rec.header, valids); ttl > 0 && !rec.overflow && rec.wrote {
			if vary, ok := responseVary(rec.header); ok {
				c.Put(key, vary, r, rec.status, rec.header, rec.buf.Bytes(), ttl)
			}
		}
		rec.flush()
	})
}

func responseVary(h http.Header) ([]string, bool) {
	var out []string
	for _, value := range h.Values("Vary") {
		for _, part := range strings.Split(value, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "*" {
				return nil, false
			}
			if name == "" {
				continue
			}
			seen := false
			for _, old := range out {
				if old == name {
					seen = true
					break
				}
			}
			if !seen {
				out = append(out, name)
			}
		}
	}
	return out, true
}

// cacheRecorder captures a bounded response before releasing it to the client,
// snapshotting headers before downstream middleware mutates them.
type cacheRecorder struct {
	http.ResponseWriter
	status   int
	header   http.Header
	buf      bytes.Buffer
	overflow bool
	wrote    bool
	flushed  bool
}

func (rec *cacheRecorder) WriteHeader(code int) {
	if !rec.wrote {
		rec.wrote = true
		rec.status = code
		rec.Header().Set("X-Cache-Status", "MISS")
		rec.header = rec.Header().Clone()
		rec.header.Del("X-Cache-Status")
	}
}

func (rec *cacheRecorder) Write(b []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	if !rec.overflow {
		if rec.buf.Len()+len(b) > maxCacheableBody {
			rec.flush()
			rec.overflow = true
		} else {
			return rec.buf.Write(b)
		}
	}
	if !rec.flushed {
		rec.flush()
	}
	return rec.ResponseWriter.Write(b)
}

func (rec *cacheRecorder) Flush() {
	// Delay flushes until cache insertion completes. An upstream buffering
	// layer may call Flush while the response is still being assembled.
	if rec.overflow {
		rec.flush()
	}
}

func (rec *cacheRecorder) flush() {
	if rec.flushed || !rec.wrote {
		return
	}
	rec.flushed = true
	rec.ResponseWriter.WriteHeader(rec.status)
	if rec.buf.Len() > 0 {
		rec.ResponseWriter.Write(rec.buf.Bytes())
		rec.buf.Reset()
	}
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
