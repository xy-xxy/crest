package cache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

// TestLookupDoesNotScan guards against the index regressing to a linear walk.
// Matching a variant needs the stored response's Vary names, and learning them
// by iterating every entry made each request O(n) over a 4096-entry cache.
func TestLookupDoesNotScan(t *testing.T) {
	if testing.Short() {
		t.Skip("timing sensitive")
	}
	measure := func(n int) time.Duration {
		c, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("http://h/p%d", i)
			c.Put(key, nil, req(t, key, ""), 200, http.Header{}, []byte("x"), time.Hour)
		}
		last := fmt.Sprintf("http://h/p%d", n-1)
		r := req(t, last, "")
		const iters = 300
		start := time.Now()
		for i := 0; i < iters; i++ {
			c.Serve(httptest.NewRecorder(), r, last)
		}
		return time.Since(start) / iters
	}
	small, large := measure(16), measure(4000)
	t.Logf("lookup: %v with 16 entries, %v with 4000", small, large)
	if large > small*2 {
		t.Errorf("lookup cost grew with cache size: %v → %v", small, large)
	}
}
