package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestLimitReq(t *testing.T) {
	conf := `
http {
    access_log off;
    error_log stderr crit;
    limit_req_zone $binary_remote_addr zone=perip:10m rate=1r/s;
    server {
        listen 8080;
        location = /limited {
            limit_req zone=perip;
            return 200 "ok";
        }
        location = /free {
            return 200 "ok";
        }
    }
}
`
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	if code, _ := get(t, ts, "/limited", ""); code != 200 {
		t.Fatalf("first request: %d", code)
	}
	if code, _ := get(t, ts, "/limited", ""); code != 503 {
		t.Errorf("second immediate request should be limited: %d", code)
	}
	// other locations are unaffected
	if code, _ := get(t, ts, "/free", ""); code != 200 {
		t.Errorf("unlimited location: %d", code)
	}
}

func TestProxyCache(t *testing.T) {
	var hits int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		fmt.Fprintf(w, "response %d", n)
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)
	cacheDir := t.TempDir()

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    proxy_cache_path "%s" keys_zone=zone1:10m;
    server {
        listen 8080;
        location /api/ {
            proxy_pass http://%s;
            proxy_cache zone1;
            proxy_cache_valid 200 10m;
        }
    }
}
`, filepath.ToSlash(cacheDir), bu.Host)

	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/api/data", nil)
	resp1, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if got := resp1.Header.Get("X-Cache-Status"); got != "MISS" {
		t.Errorf("first request: X-Cache-Status = %q, want MISS", got)
	}

	code2, body2 := get(t, ts, "/api/data", "")
	if code2 != 200 || body2 != "response 1" {
		t.Errorf("cached response wrong: %d %q", code2, body2)
	}
	if atomic.LoadInt64(&hits) != 1 {
		t.Errorf("backend hit %d times, want 1", hits)
	}

	resp3, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if got := resp3.Header.Get("X-Cache-Status"); got != "HIT" {
		t.Errorf("second request: X-Cache-Status = %q, want HIT", got)
	}

	// a different URI misses independently
	if _, body := get(t, ts, "/api/other", ""); body != "response 2" {
		t.Errorf("different URI should reach backend: %q", body)
	}
}

func TestProxyCacheRespectsNoStore(t *testing.T) {
	var hits int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "%d", atomic.AddInt64(&hits, 1))
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    proxy_cache_path "%s" keys_zone=z:1m;
    server {
        listen 8080;
        location / {
            proxy_pass http://%s;
            proxy_cache z;
            proxy_cache_valid 200 10m;
        }
    }
}
`, filepath.ToSlash(t.TempDir()), bu.Host)

	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	get(t, ts, "/x", "")
	get(t, ts, "/x", "")
	if atomic.LoadInt64(&hits) != 2 {
		t.Errorf("no-store responses must not be cached: %d backend hits", hits)
	}
}
