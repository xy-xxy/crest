package handler

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"crest/internal/cache"
)

// CacheValid mirrors proxy_cache_valid. Codes[-1] means any status.
type CacheValid struct {
	Codes map[int]bool
	TTL   time.Duration
}

const maxCacheableBody = 8 << 20

// Bound concurrent response recording across all cache zones. Each admitted
// response holds at most 8 MiB of body data; excess misses stream uncached.
// This is not a process RSS limit: buffers, connections and GC also use memory.
var cacheRecordSlots = make(chan struct{}, 4)

// WrapCache serves GET/HEAD hits; only GET can populate the shared entry.
func WrapCache(h http.Handler, c *cache.Cache, valids []CacheValid) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("Authorization") != "" {
			h.ServeHTTP(w, r)
			return
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		key := scheme + "://" + r.Host + r.URL.RequestURI()
		if c.ServeVary(w, r, key) {
			return
		}
		// Select variants from the client request even if a downstream handler
		// rewrites it. The standard ReverseProxy already clones its outbound request.
		clientRequest := r.Clone(r.Context())
		originalURL := r.URL
		defer func() {
			r.Host = clientRequest.Host
			r.URL = originalURL
			*r.URL = *clientRequest.URL
		}()
		rec := &cacheRecorder{ResponseWriter: w, valids: valids, allowCache: r.Method == http.MethodGet}
		defer rec.release()
		h.ServeHTTP(rec, r)
		if rec.hijacked {
			return
		}
		if !rec.wrote {
			rec.WriteHeader(http.StatusOK)
		}
		if rec.recording && !rec.failed {
			c.Put(key, rec.vary, clientRequest, rec.status, rec.header, rec.buf.Bytes(), rec.ttl)
		}
		// Publication completes before the client can finish a cacheable response.
		rec.flush()
	})
}

// responseVary canonicalizes policies and rejects wildcard responses.
func responseVary(h http.Header) ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	for _, value := range h.Values("Vary") {
		for _, part := range strings.Split(value, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "*" {
				return nil, false
			}
			if name != "" && !seen[name] {
				out = append(out, name)
				seen[name] = true
			}
		}
	}
	sort.Strings(out)
	return out, true
}

// cacheRecorder only buffers responses eligible for caching and admitted to
// the shared budget. Ineligible/oversized/over-budget responses stream through.
type cacheRecorder struct {
	http.ResponseWriter
	valids     []CacheValid
	allowCache bool
	status     int
	header     http.Header
	vary       []string
	ttl        time.Duration
	buf        bytes.Buffer
	wrote      bool
	flushed    bool
	recording  bool
	slot       bool
	failed     bool
	hijacked   bool
}

func (rec *cacheRecorder) WriteHeader(code int) {
	if rec.wrote {
		return
	}
	if code >= 100 && code < 200 {
		rec.ResponseWriter.WriteHeader(code)
		return
	}
	rec.wrote = true
	rec.status = code
	rec.Header().Set("X-Cache-Status", "MISS")
	rec.header = rec.Header().Clone()
	rec.header.Del("X-Cache-Status")
	rec.vary, rec.recording = responseVary(rec.header)
	rec.ttl = cacheTTL(code, rec.header, rec.valids)
	rec.recording = rec.recording && rec.allowCache && rec.ttl > 0 && rec.header.Get("Trailer") == "" &&
		!strings.HasPrefix(strings.ToLower(rec.header.Get("Content-Type")), "text/event-stream")
	if n, err := strconv.ParseInt(rec.header.Get("Content-Length"), 10, 64); err == nil && n > maxCacheableBody {
		rec.recording = false
	}
	if rec.recording {
		select {
		case cacheRecordSlots <- struct{}{}:
			rec.slot = true
		default:
			rec.recording = false
			rec.Header().Set("X-Cache-Status", "BYPASS")
		}
	}
	if !rec.recording {
		rec.flush()
	}
}

func (rec *cacheRecorder) Write(b []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	if rec.recording {
		if rec.buf.Len()+len(b) <= maxCacheableBody {
			return rec.buf.Write(b)
		}
		rec.recording = false
		rec.flush()
		rec.release()
	}
	n, err := rec.ResponseWriter.Write(b)
	if err != nil {
		rec.failed = true
	}
	return n, err
}

func (rec *cacheRecorder) Flush() {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	if !rec.recording {
		rec.flush()
		if f, ok := rec.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (rec *cacheRecorder) flush() {
	if rec.flushed || !rec.wrote {
		return
	}
	rec.flushed = true
	rec.ResponseWriter.WriteHeader(rec.status)
	if rec.buf.Len() > 0 {
		if _, err := rec.ResponseWriter.Write(rec.buf.Bytes()); err != nil {
			rec.failed = true
		}
		rec.buf = bytes.Buffer{}
	}
}

func (rec *cacheRecorder) release() {
	if rec.slot {
		<-cacheRecordSlots
		rec.slot = false
	}
}

func (rec *cacheRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rec.recording = false
	rec.hijacked = true
	rec.buf = bytes.Buffer{}
	rec.release()
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
