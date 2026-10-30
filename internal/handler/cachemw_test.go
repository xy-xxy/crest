package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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

func TestWrapCacheHeadMissPreservesResponse(t *testing.T) {
	c, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var hits int
	h := WrapCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Length", "4")
		w.Header().Set("X-Origin", "yes")
		w.WriteHeader(202)
	}), c, []CacheValid{{Codes: map[int]bool{202: true}, TTL: time.Minute}})
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("HEAD", "http://h/x", nil))
		if w.Code != 202 || w.Header().Get("X-Origin") != "yes" {
			t.Fatalf("HEAD response: %d %v", w.Code, w.Header())
		}
	}
	if hits != 2 {
		t.Fatalf("HEAD populated cache: hits=%d", hits)
	}
}

func TestWrapCacheNonCacheableStreams(t *testing.T) {
	for _, tc := range []struct{ name, key, value string }{{"no-store", "Cache-Control", "no-store"}, {"wildcard", "Vary", "*"}, {"SSE", "Content-Type", "text/event-stream"}} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := cache.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			h := WrapCache(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
				out.Header().Set(tc.key, tc.value)
				out.Write([]byte("first"))
				out.(http.Flusher).Flush()
				if w.Body.String() != "first" || !w.Flushed {
					t.Error("stream withheld until handler completion")
				}
				out.Write([]byte("second"))
			}), c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}})
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://h/x", nil))
			if w.Body.String() != "firstsecond" {
				t.Fatalf("body=%q", w.Body.String())
			}
		})
	}
}

func TestWrapCacheBudgetBypassesWithoutBlocking(t *testing.T) {
	c, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, cap(cacheRecordSlots))
	release := make(chan struct{})
	var wg sync.WaitGroup
	h := WrapCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
		if r.URL.Path == "/hold" {
			started <- struct{}{}
			<-release
		}
	}), c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}})
	defer func() { close(release); wg.Wait() }()
	for i := 0; i < cap(cacheRecordSlots); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://h/hold", nil))
		}()
	}
	for i := 0; i < cap(cacheRecordSlots); i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("recorders did not start")
		}
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, httptest.NewRequest("GET", "http://h/excess", nil)); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("excess request blocked on cache budget")
	}
	if w.Code != 200 || w.Body.String() != "ok" || w.Header().Get("X-Cache-Status") != "BYPASS" {
		t.Fatalf("bypass response=%d %q %v", w.Code, w.Body.String(), w.Header())
	}
}

func TestWrapCachePublishesBeforeResponse(t *testing.T) {
	c, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "http://h/x", nil)
	w := &cachePublishWriter{ResponseRecorder: httptest.NewRecorder(), check: func() {
		hit := httptest.NewRecorder()
		if !c.Serve(hit, r, "http://h/x") || hit.Body.String() != "ok" {
			t.Error("response released before cache publication")
		}
	}}
	h := WrapCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")); w.(http.Flusher).Flush() }), c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}})
	h.ServeHTTP(w, r)
}

type cachePublishWriter struct {
	*httptest.ResponseRecorder
	check func()
}

func (w *cachePublishWriter) WriteHeader(code int) { w.check(); w.ResponseRecorder.WriteHeader(code) }

func TestWrapCacheOversizePreservesBodyAndReleasesBudget(t *testing.T) {
	c, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	var hits int
	h := WrapCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		for i := 0; i < 9; i++ {
			w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	}), c, []CacheValid{{Codes: map[int]bool{200: true}, TTL: time.Minute}})
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://h/x", nil))
		if w.Body.Len() != 9<<20 || bytes.Count(w.Body.Bytes(), []byte("x")) != 9<<20 {
			t.Fatalf("oversize body length=%d", w.Body.Len())
		}
	}
	if hits != 2 {
		t.Fatal("oversized response was cached")
	}
	if len(cacheRecordSlots) != 0 {
		t.Fatal("cache recording slot leaked")
	}
}
