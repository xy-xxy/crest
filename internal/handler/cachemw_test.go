package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"crest/internal/cache"
)

func TestWrapCacheVarySeparatesRequests(t *testing.T) {
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Vary", "Accept-Language")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(r.Header.Get("Accept-Language")))
	})
	h := WrapCache(backend, c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}})
	request := func(lang string) string {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/x", nil)
		r.Header.Set("Accept-Language", lang)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Body.String()
	}
	if got := request("en"); got != "en" {
		t.Fatalf("first response = %q", got)
	}
	if got := request("fr"); got != "fr" {
		t.Fatalf("vary response = %q", got)
	}
	if got := request("en"); got != "en" {
		t.Fatalf("cached response = %q", got)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("backend hits = %d, want 2", got)
	}
}

func TestWrapCacheNoVaryWithGzipWrapper(t *testing.T) {
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("response"))
	})
	h := WrapGzip(WrapCache(backend, c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}}), &GzipConfig{Types: map[string]bool{"text/plain": true}})
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/x", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("backend hits = %d, want 1", got)
	}
}

func TestWrapCacheNoVaryWithProxyBuffering(t *testing.T) {
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write([]byte("response")) })
	h := WrapCache(WrapProxyBuffering(backend, BufferOptions{}), c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}})
	for i := 0; i < 2; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.test/x", nil))
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("backend hits = %d, want 1", got)
	}
}

func TestWrapCacheRestoresRequestAfterDownstreamRewrite(t *testing.T) {
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		r.Host = "upstream.internal"
		r.URL.Path = "/rewritten"
		w.Write([]byte("response"))
	})
	h := WrapCache(backend, c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}})
	r := httptest.NewRequest(http.MethodGet, "http://example.test/original", nil)
	first := httptest.NewRecorder()
	h.ServeHTTP(first, r)
	if r.Host != "example.test" || r.URL.Path != "/original" {
		t.Fatalf("request mutated after downstream: host=%q path=%q", r.Host, r.URL.Path)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, r)
	if got := hits.Load(); got != 1 {
		t.Fatalf("backend hits = %d, want 1", got)
	}
	if second.Header().Get("X-Cache-Status") != "HIT" {
		t.Fatalf("second request cache status = %q, want HIT", second.Header().Get("X-Cache-Status"))
	}
}

func TestWrapCacheVaryStarSkipsCache(t *testing.T) {
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	h := WrapCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Vary", "*")
		w.Write([]byte("ok"))
	}), c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}})
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "http://example.test/x", nil)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("backend hits = %d, want 2", got)
	}
}
