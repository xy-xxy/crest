package cache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func req(t *testing.T, url, lang string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, url, nil)
	if lang != "" {
		r.Header.Set("Accept-Language", lang)
	}
	return r
}

func serve(c *Cache, r *http.Request, base string) (bool, string) {
	w := httptest.NewRecorder()
	hit := c.Serve(w, r, base)
	return hit, w.Body.String()
}

// TestServeSelectsVariant checks the basic Vary contract: each set of request
// header values gets its own stored body.
func TestServeSelectsVariant(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := "http://h/x"
	vary := []string{"accept-language"}
	c.Put(base, vary, req(t, base, "en"), 200, http.Header{}, []byte("EN"), time.Hour)
	c.Put(base, vary, req(t, base, "fr"), 200, http.Header{}, []byte("FR"), time.Hour)

	for _, tc := range []struct{ lang, want string }{{"en", "EN"}, {"fr", "FR"}} {
		hit, body := serve(c, req(t, base, tc.lang), base)
		if !hit || body != tc.want {
			t.Errorf("%s: hit=%v body=%q, want %q", tc.lang, hit, body, tc.want)
		}
	}
	// A value with no stored variant must miss rather than borrow another's.
	if hit, body := serve(c, req(t, base, "de"), base); hit {
		t.Errorf("unstored variant answered with %q", body)
	}
}

// TestPutPurgesVariantsOnVaryPolicyChange is the invariant that makes variant
// lookup safe. A response stored without Vary is filed under an empty vary
// key, which matches every request; if it survived alongside real variants it
// would answer all of them. The same applies in reverse, and when the set of
// Vary names itself changes: those keys can no longer be reproduced.
func TestPutPurgesVariantsOnVaryPolicyChange(t *testing.T) {
	base := "http://h/x"

	t.Run("no-Vary entry cannot shadow later variants", func(t *testing.T) {
		c, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c.Put(base, nil, req(t, base, ""), 200, http.Header{}, []byte("PLAIN"), time.Hour)
		c.Put(base, []string{"accept-language"}, req(t, base, "en"), 200, http.Header{}, []byte("EN"), time.Hour)

		if hit, body := serve(c, req(t, base, "en"), base); !hit || body != "EN" {
			t.Errorf("en: hit=%v body=%q, want EN", hit, body)
		}
		if hit, body := serve(c, req(t, base, "fr"), base); hit {
			t.Errorf("fr answered from the stale non-varying entry: %q", body)
		}
	})

	t.Run("variants dropped when Vary disappears", func(t *testing.T) {
		c, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c.Put(base, []string{"accept-language"}, req(t, base, "en"), 200, http.Header{}, []byte("EN"), time.Hour)
		c.Put(base, nil, req(t, base, ""), 200, http.Header{}, []byte("PLAIN"), time.Hour)

		// every request now resolves to the single non-varying entry
		for _, lang := range []string{"en", "fr", ""} {
			if hit, body := serve(c, req(t, base, lang), base); !hit || body != "PLAIN" {
				t.Errorf("lang=%q: hit=%v body=%q, want PLAIN", lang, hit, body)
			}
		}
	})

	t.Run("variants dropped when the Vary names change", func(t *testing.T) {
		c, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c.Put(base, []string{"accept-language"}, req(t, base, "en"), 200, http.Header{}, []byte("BY-LANG"), time.Hour)
		r := req(t, base, "en")
		r.Header.Set("Accept-Encoding", "gzip")
		c.Put(base, []string{"accept-encoding"}, r, 200, http.Header{}, []byte("BY-ENC"), time.Hour)

		if hit, body := serve(c, r, base); !hit || body != "BY-ENC" {
			t.Errorf("new policy: hit=%v body=%q, want BY-ENC", hit, body)
		}
		// the old key is unreachable now, so a lang-only request must miss
		if hit, body := serve(c, req(t, base, "en"), base); hit && body == "BY-LANG" {
			t.Errorf("entry from the superseded Vary policy is still reachable")
		}
	})
}

// BenchmarkLookup measures lookup cost without timing-dependent test failures.
func BenchmarkLookup(b *testing.B) {
	for _, n := range []int{16, 4000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			c, err := Open(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < n; i++ {
				key := fmt.Sprintf("http://h/%d", i)
				r := httptest.NewRequest("GET", key, nil)
				c.Put(key, nil, r, 200, http.Header{}, []byte("x"), time.Hour)
			}
			key := fmt.Sprintf("http://h/%d", n-1)
			r := httptest.NewRequest("GET", key, nil)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !c.Serve(httptest.NewRecorder(), r, key) {
					b.Fatal("expected cache hit")
				}
			}
		})
	}
}

// Replacing an entry while a reader holds its file must not change that reader's body.
func TestServeDuringReplacement(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := req(t, "http://h/x", "")
	c.Put("key", nil, r, 200, http.Header{"X-Generation": {"old"}}, []byte("old body"), time.Hour)
	w := &replaceWriter{ResponseRecorder: httptest.NewRecorder(), replace: func() {
		c.Put("key", nil, r, 200, http.Header{"X-Generation": {"new"}}, []byte("new body"), time.Hour)
	}}
	if !c.Serve(w, r, "key") {
		t.Fatal("expected hit")
	}
	if w.Header().Get("X-Generation") != "old" || w.Body.String() != "old body" {
		t.Fatalf("mixed generations: header=%q body=%q", w.Header().Get("X-Generation"), w.Body.String())
	}
	if hit, body := serve(c, r, "key"); !hit || body != "new body" {
		t.Fatalf("replacement: hit=%v body=%q", hit, body)
	}
}

type replaceWriter struct {
	*httptest.ResponseRecorder
	replace func()
}

func (w *replaceWriter) WriteHeader(code int) { w.replace(); w.ResponseRecorder.WriteHeader(code) }

func TestConcurrentReplacementIntegrity(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			r := httptest.NewRequest("GET", "http://h/x", nil)
			for i := 0; i < 30; i++ {
				body := strings.Repeat(fmt.Sprintf("%02d", id), 1024+id)
				c.Put("key", nil, r, 200, http.Header{"X-Body": {body[:2]}, "Content-Length": {strconv.Itoa(len(body))}}, []byte(body), time.Hour)
				w := httptest.NewRecorder()
				if !c.Serve(w, r, "key") {
					t.Error("unexpected miss")
					return
				}
				n, _ := strconv.Atoi(w.Header().Get("Content-Length"))
				if w.Body.Len() != n || w.Body.String() != strings.Repeat(w.Header().Get("X-Body"), n/2) {
					t.Error("mixed response generations")
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	files, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("retired files leaked: %d", len(files))
	}
}

func TestVaryUsesAllHeaderValues(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := req(t, "http://h/x", "")
	r.Header["Accept-Language"] = []string{"en", "fr"}
	c.Put("key", []string{"accept-language"}, r, 200, http.Header{}, []byte("EN-FR"), time.Hour)
	r.Header["Accept-Language"] = []string{"en", "de"}
	if hit, _ := serve(c, r, "key"); hit {
		t.Fatal("ignored second request header value")
	}
}

func TestClosePreservesActiveReaderAndRejectsLatePut(t *testing.T) {
	c, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := req(t, "http://h/x", "")
	c.Put("key", nil, r, 200, http.Header{}, []byte("body"), time.Hour)
	w := &replaceWriter{ResponseRecorder: httptest.NewRecorder(), replace: func() { c.Close(); c.Put("key", nil, r, 200, http.Header{}, []byte("late"), time.Hour) }}
	if !c.Serve(w, r, "key") || w.Body.String() != "body" {
		t.Fatal("close interrupted active response")
	}
	if hit, _ := serve(c, r, "key"); hit {
		t.Fatal("closed cache accepted late publication")
	}
	files, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("shutdown left %d files", len(files))
	}
}
