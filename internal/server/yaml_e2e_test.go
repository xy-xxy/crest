package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"gonx/internal/config"
)

func buildFromYAML(t *testing.T, yml string) *Instance {
	t.Helper()
	cfg, err := config.LoadYAML([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestYAMLEndToEnd(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend %s host=%s", r.URL.Path, r.Host)
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.html"), "yaml home")

	yml := fmt.Sprintf(`
error_log: {path: stderr, level: crit}
http:
  access_log: {off: true}
  servers:
    - listen: [8080]
      root: %q
      locations:
        - path: /
          try_files: [$uri, /index.html]
        - path: /ping
          match: exact
          return: {code: 200, body: "pong"}
        - path: /api/
          proxy_pass: http://%s/
          proxy_set_header:
            Host: $host
`, filepath.ToSlash(dir), bu.Host)

	inst := buildFromYAML(t, yml)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	if code, body := get(t, ts, "/", ""); code != 200 || body != "yaml home" {
		t.Errorf("static: %d %q", code, body)
	}
	if code, body := get(t, ts, "/spa/route", ""); code != 200 || body != "yaml home" {
		t.Errorf("try_files fallback: %d %q", code, body)
	}
	if code, body := get(t, ts, "/ping", ""); code != 200 || body != "pong" {
		t.Errorf("return: %d %q", code, body)
	}
	// trailing slash in proxy_pass replaces the /api/ prefix, so /api/users → /users
	if _, body := get(t, ts, "/api/users", "myhost.test"); body != "backend /users host=myhost.test" {
		t.Errorf("proxy with Host header: %q", body)
	}
}
