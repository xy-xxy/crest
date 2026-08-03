package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddHeader(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.html"), "hi")
	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    add_header X-Global always;
    server {
        listen 8080;
        root "%s";
        add_header X-From-Server server-val;
        location / {
            add_header X-From-Loc loc-val;
        }
    }
}
`, filepath.ToSlash(dir))
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// location add_header replaces server/http level (nginx inheritance)
	if got := resp.Header.Get("X-From-Loc"); got != "loc-val" {
		t.Errorf("X-From-Loc = %q", got)
	}
	if resp.Header.Get("X-From-Server") != "" || resp.Header.Get("X-Global") != "" {
		t.Error("outer add_header should be shadowed by location-level directives")
	}
}

func TestAllowDeny(t *testing.T) {
	conf := `
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        location = /admin {
            allow 127.0.0.1;
            allow ::1;
            deny all;
            return 200 "admin ok";
        }
        location = /open {
            return 200 "open";
        }
    }
}
`
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	// requests come from loopback → allowed
	if code, body := get(t, ts, "/admin", ""); code != 200 || body != "admin ok" {
		t.Errorf("loopback should be allowed: %d %q", code, body)
	}
	if code, _ := get(t, ts, "/open", ""); code != 200 {
		t.Errorf("unrestricted location: %d", code)
	}
}

func TestAuthBasic(t *testing.T) {
	dir := t.TempDir()
	// user "admin", apr1 hash of "secret123"
	htpasswd := "admin:$apr1$saltsalt$AVDPWMuMvN31vdabhs8HK1\n"
	hpPath := filepath.Join(dir, "htpasswd")
	os.WriteFile(hpPath, []byte(htpasswd), 0600)

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        location / {
            auth_basic "Restricted";
            auth_basic_user_file "%s";
            return 200 "authed";
        }
    }
}
`, filepath.ToSlash(hpPath))
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	// no credentials → 401 with challenge
	resp, _ := ts.Client().Get(ts.URL + "/")
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Restricted") {
		t.Errorf("missing auth: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// correct credentials → 200
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.SetBasicAuth("admin", "secret123")
	resp2, _ := ts.Client().Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("valid credentials: %d", resp2.StatusCode)
	}
	// wrong password → 401
	req.SetBasicAuth("admin", "wrong")
	resp3, _ := ts.Client().Do(req)
	resp3.Body.Close()
	if resp3.StatusCode != 401 {
		t.Errorf("wrong password: %d", resp3.StatusCode)
	}
}

func TestAutoindex(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "A")
	os.Mkdir(filepath.Join(dir, "sub"), 0755)

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        root "%s";
        autoindex on;
    }
}
`, filepath.ToSlash(dir))
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	code, body := get(t, ts, "/", "")
	if code != 200 || !strings.Contains(body, "Index of /") || !strings.Contains(body, "a.txt") || !strings.Contains(body, "sub/") {
		t.Errorf("autoindex listing wrong: %d\n%s", code, body)
	}
}

func TestNamedLocationTryFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "exists.txt"), "static file")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend handled %s", r.URL.Path)
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        root "%s";
        location / {
            try_files $uri @backend;
        }
        location @backend {
            proxy_pass http://%s;
        }
    }
}
`, filepath.ToSlash(dir), bu.Host)
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	if _, body := get(t, ts, "/exists.txt", ""); body != "static file" {
		t.Errorf("existing file should be served statically: %q", body)
	}
	if _, body := get(t, ts, "/dynamic/path", ""); !strings.Contains(body, "backend handled") {
		t.Errorf("missing file should fall back to @backend: %q", body)
	}
}

func TestMapAndUpstreamAddr(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	logFile := filepath.Join(t.TempDir(), "access.log")
	conf := fmt.Sprintf(`
http {
    error_log stderr crit;
    log_format up '$upstream_addr $mapped';
    map $request_method $mapped {
        default other;
        GET      got-get;
    }
    access_log "%s" up;
    server {
        listen 8080;
        location / {
            proxy_pass http://%s;
        }
    }
}
`, filepath.ToSlash(logFile), bu.Host)
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	get(t, ts, "/", "")
	// flush by shutting down
	inst.Shutdown(context.Background())

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.Contains(line, bu.Host) {
		t.Errorf("$upstream_addr not logged: %q", line)
	}
	if !strings.Contains(line, "got-get") {
		t.Errorf("$mapped (map GET) not resolved: %q", line)
	}
}

func TestBatch1Expires(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "x")
	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        root "%s";
        location / {
            expires 1h;
        }
    }
}
`, filepath.ToSlash(dir))
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL + "/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "max-age=3600" {
		t.Errorf("Cache-Control = %q, want max-age=3600", cc)
	}
	if resp.Header.Get("Expires") == "" {
		t.Error("Expires header missing")
	}
}

func TestBatch1ServerTokens(t *testing.T) {
	on := `
http {
    access_log off;
    error_log stderr crit;
    server { listen 8080; location / { return 200 "ok"; } }
}`
	off := `
http {
    access_log off;
    error_log stderr crit;
    server_tokens off;
    server { listen 8080; location / { return 200 "ok"; } }
}`
	for _, tc := range []struct{ conf, wantPrefix string }{
		{on, "gonx/"}, // 带版本
		{off, "gonx"}, // 仅名称
	} {
		inst := buildInstance(t, tc.conf)
		ts := httptest.NewServer(inst.Addrs[0])
		resp, err := ts.Client().Get(ts.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		got := resp.Header.Get("Server")
		if tc.wantPrefix == "gonx/" && got != "gonx/"+Version {
			t.Errorf("server_tokens on: Server = %q", got)
		}
		if tc.wantPrefix == "gonx" && got != "gonx" {
			t.Errorf("server_tokens off: Server = %q, want plain gonx", got)
		}
		ts.Close()
	}
}

func TestBatch1ProxyRedirectAndHideHeader(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://backend.internal:9000/app/next")
		w.Header().Set("X-Secret", "leak")
		w.Header().Set("X-Keep", "ok")
		w.WriteHeader(302)
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
            proxy_pass http://%s/;
            proxy_redirect http://backend.internal:9000/ /api/;
            proxy_hide_header X-Secret;
        }
    }
}
`, bu.Host)
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(ts.URL + "/api/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/api/app/next" {
		t.Errorf("proxy_redirect: Location = %q, want /api/app/next", loc)
	}
	if resp.Header.Get("X-Secret") != "" {
		t.Error("proxy_hide_header: X-Secret should be hidden")
	}
	if resp.Header.Get("X-Keep") != "ok" {
		t.Error("X-Keep should pass through")
	}
}

func TestBatch1SSLProtocols(t *testing.T) {
	dir := t.TempDir()
	crt, key := selfSigned(t, dir, "p", "localhost")
	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    ssl_protocols TLSv1.3;
    server {
        listen 443 ssl;
        server_name localhost;
        ssl_certificate "%s";
        ssl_certificate_key "%s";
        location / { return 200 "ok"; }
    }
}
`, filepath.ToSlash(crt), filepath.ToSlash(key))
	inst := buildInstance(t, conf)
	a := inst.Addrs[0]
	if a.tlsConf.MinVersion != tls.VersionTLS13 || a.tlsConf.MaxVersion != tls.VersionTLS13 {
		t.Errorf("ssl_protocols TLSv1.3: min=%#x max=%#x", a.tlsConf.MinVersion, a.tlsConf.MaxVersion)
	}
}

func TestBatch2GzipStatic(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "app.js"), "console.log('raw')")
	// 预压缩版本（内容不同以便区分是否命中 .gz）
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte("console.log('gzipped')"))
	zw.Close()
	os.WriteFile(filepath.Join(dir, "app.js.gz"), buf.Bytes(), 0644)

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        root "%s";
        location / { gzip_static on; }
    }
}
`, filepath.ToSlash(dir))
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	tr := &http.Transport{DisableCompression: true}
	req, _ := http.NewRequest("GET", ts.URL+"/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("gzip_static should serve .gz, headers: %v", resp.Header)
	}
	zr, _ := gzip.NewReader(resp.Body)
	body, _ := io.ReadAll(zr)
	if string(body) != "console.log('gzipped')" {
		t.Errorf("served wrong content: %q", body)
	}
	// 不接受 gzip 的客户端应拿到原文件
	req2, _ := http.NewRequest("GET", ts.URL+"/app.js", nil)
	resp2, _ := tr.RoundTrip(req2)
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	if resp2.Header.Get("Content-Encoding") == "gzip" || string(b2) != "console.log('raw')" {
		t.Errorf("no-gzip client should get raw file: %q enc=%q", b2, resp2.Header.Get("Content-Encoding"))
	}
}

func TestBatch2ValidReferers(t *testing.T) {
	conf := `
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        server_name example.com;
        location /img/ {
            valid_referers none server_names *.trusted.com;
            return 200 "image";
        }
    }
}
`
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	do := func(referer string) int {
		req, _ := http.NewRequest("GET", ts.URL+"/img/x.png", nil)
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if do("") != 200 {
		t.Error("none: empty referer should pass")
	}
	if do("http://example.com/page") != 200 {
		t.Error("server_names: own domain should pass")
	}
	if do("http://a.trusted.com/x") != 200 {
		t.Error("*.trusted.com should pass")
	}
	if do("http://evil.com/steal") != 403 {
		t.Error("untrusted referer should be 403")
	}
}

func TestBatch2LimitRate(t *testing.T) {
	// 200 字节 @ 1000 B/s，limit_rate_after 0 → 约 0.2s；给足余量只验证"确有延迟"
	body := strings.Repeat("x", 2000)
	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        location / {
            limit_rate 5k;
            return 200 "%s";
        }
    }
}
`, body)
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()
	start := time.Now()
	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)
	if len(got) != 2000 {
		t.Fatalf("body length %d, want 2000", len(got))
	}
	// 2000B @ 5KB/s ≈ 0.39s；至少应明显 > 0（宽松下限，避免 CI 抖动误判）
	if elapsed < 200*time.Millisecond {
		t.Errorf("limit_rate did not throttle: %v for 2000B @ 5KB/s", elapsed)
	}
}

func TestBatch2UpstreamKeepalive(t *testing.T) {
	// 仅验证配置解析 + 传递到 proxy（不便直接观测连接池）
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)
	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    upstream pool {
        server %s;
        keepalive 32;
    }
    server {
        listen 8080;
        location / { proxy_pass http://pool; }
    }
}
`, bu.Host)
	inst := buildInstance(t, conf)
	if inst.cfg.HTTP.Upstreams["pool"].Keepalive != 32 {
		t.Errorf("keepalive not parsed: %d", inst.cfg.HTTP.Upstreams["pool"].Keepalive)
	}
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()
	// 多次请求确保代理正常（连接复用是内部行为）
	for i := 0; i < 3; i++ {
		if _, body := get(t, ts, "/", ""); body != "ok" {
			t.Fatalf("proxy with keepalive failed: %q", body)
		}
	}
}
