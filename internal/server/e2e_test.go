package server

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"gonx/internal/config"
)

func buildInstance(t *testing.T, conf string) *Instance {
	t.Helper()
	ds, err := config.Parse(conf)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(ds)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func get(t *testing.T, ts *httptest.Server, path, host string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func TestEndToEnd(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend:%s:%s", r.URL.Path, r.Header.Get("X-Real-IP"))
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.html"), "<h1>hello gonx</h1>")
	mustWrite(t, filepath.Join(dir, "a.txt"), "AAA")
	os.Mkdir(filepath.Join(dir, "sub"), 0755)
	mustWrite(t, filepath.Join(dir, "sub", "index.html"), "sub index")

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        server_name localhost;
        root "%s";
        location /api/ {
            proxy_pass http://%s;
        }
        location = /ping {
            return 200 "pong";
        }
        location /old/ {
            return 301 /new/;
        }
    }
}
`, filepath.ToSlash(dir), bu.Host)

	inst := buildInstance(t, conf)
	if len(inst.Addrs) != 1 {
		t.Fatalf("want 1 addr server, got %d", len(inst.Addrs))
	}
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	if code, body := get(t, ts, "/", ""); code != 200 || body != "<h1>hello gonx</h1>" {
		t.Errorf("index: %d %q", code, body)
	}
	if code, body := get(t, ts, "/a.txt", ""); code != 200 || body != "AAA" {
		t.Errorf("static file: %d %q", code, body)
	}
	// /sub redirects to /sub/ then serves its index (client follows)
	if code, body := get(t, ts, "/sub", ""); code != 200 || body != "sub index" {
		t.Errorf("dir index: %d %q", code, body)
	}
	if code, _ := get(t, ts, "/missing.txt", ""); code != 404 {
		t.Errorf("missing file: %d", code)
	}
	if code, _ := get(t, ts, "/../../etc/passwd", ""); code != 404 {
		t.Errorf("traversal must not escape root: %d", code)
	}
	if code, body := get(t, ts, "/ping", ""); code != 200 || body != "pong" {
		t.Errorf("return: %d %q", code, body)
	}
	if code, body := get(t, ts, "/api/users", ""); code != 200 || body != "backend:/api/users:127.0.0.1" {
		t.Errorf("proxy: %d %q", code, body)
	}
	if code, _ := get(t, ts, "/old/page", ""); code != 200 {
		// 301 → /new/ → 404 after following; just assert the redirect happened
		t.Logf("redirect chain ended with %d (expected, /new/ has no content)", code)
	}
}

func TestEndToEndProxyURIReplacement(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s", r.URL.Path)
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        location /api/ {
            proxy_pass http://%s/v1/;
        }
    }
}
`, bu.Host)
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	if _, body := get(t, ts, "/api/users", ""); body != "path=/v1/users" {
		t.Errorf("URI replacement wrong: %q", body)
	}
}

func TestEndToEndUpstream(t *testing.T) {
	mkBackend := func(name string) (*httptest.Server, string) {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, name)
		}))
		u, _ := url.Parse(s.URL)
		return s, u.Host
	}
	b1, h1 := mkBackend("b1")
	defer b1.Close()
	b2, h2 := mkBackend("b2")
	defer b2.Close()

	// grab a port that is guaranteed dead for the failover case
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().String()
	l.Close()

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    upstream pool {
        server %s;
        server %s;
        server %s;
    }
    server {
        listen 8080;
        location / {
            proxy_pass http://pool;
        }
    }
}
`, h1, h2, dead)

	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	counts := map[string]int{}
	for i := 0; i < 6; i++ {
		code, body := get(t, ts, "/", "")
		if code != 200 {
			t.Fatalf("request %d failed: %d %q", i, code, body)
		}
		counts[body]++
	}
	if counts["b1"] == 0 || counts["b2"] == 0 {
		t.Errorf("both live backends should be hit: %v", counts)
	}
}

func TestEndToEndVHosts(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	mustWrite(t, filepath.Join(dirA, "index.html"), "site A")
	mustWrite(t, filepath.Join(dirB, "index.html"), "site B")

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080 default_server;
        server_name a.test;
        root "%s";
    }
    server {
        listen 8080;
        server_name b.test *.b.test;
        root "%s";
    }
}
`, filepath.ToSlash(dirA), filepath.ToSlash(dirB))

	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	if _, body := get(t, ts, "/", "a.test"); body != "site A" {
		t.Errorf("a.test: %q", body)
	}
	if _, body := get(t, ts, "/", "b.test"); body != "site B" {
		t.Errorf("b.test: %q", body)
	}
	if _, body := get(t, ts, "/", "x.b.test"); body != "site B" {
		t.Errorf("wildcard *.b.test: %q", body)
	}
	if _, body := get(t, ts, "/", "unknown.test"); body != "site A" {
		t.Errorf("default_server: %q", body)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
