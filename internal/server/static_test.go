package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"gonx/internal/handler"
)

// TestStaticNoRootFallsThroughTo404 covers a proxy-only server: it has no
// root and no "/" location, so unmatched requests reach the synthetic
// catch-all. nginx looks under its built-in default root and 404s; gonx used
// to answer 500, turning every stray /favicon.ico into a server error.
func TestStaticNoRootFallsThroughTo404(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)
	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        location /api/ { proxy_pass http://%s/api/; }
    }
}`, bu.Host)
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	for _, p := range []string{"/", "/favicon.ico", "/anything"} {
		if code, _ := get(t, ts, p, ""); code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, code)
		}
	}
	if code, body := get(t, ts, "/api/x", ""); code != 200 || body != "ok" {
		t.Errorf("proxied location broke: %d %q", code, body)
	}
}

// TestStaticFilePathStaysUnderRoot pins the root/alias mapping against
// traversal. A backslash is a path separator to Windows filepath.Join but
// not to path.Clean, so "..\" used to walk out of the configured root there;
// the mapping now normalises it on every platform.
func TestStaticFilePathStaysUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "www")
	st := &handler.Static{Root: root, Prefix: "/"}
	for _, tc := range []struct{ uri, want string }{
		{"/ok.txt", filepath.Join(root, "ok.txt")},
		{"/../../secret.txt", filepath.Join(root, "secret.txt")},
		{`/..\..\secret.txt`, filepath.Join(root, "secret.txt")},
		{`/a\..\..\secret.txt`, filepath.Join(root, "secret.txt")},
		{"/a/../../secret.txt", filepath.Join(root, "secret.txt")},
	} {
		u, err := url.Parse("http://x" + tc.uri)
		if err != nil {
			t.Fatalf("%q: %v", tc.uri, err)
		}
		got := st.FilePath(u.Path)
		if got != tc.want {
			t.Errorf("FilePath(%q) = %q, want %q", u.Path, got, tc.want)
		}
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Errorf("FilePath(%q) = %q escaped root %q", u.Path, got, root)
		}
	}

	alias := filepath.Join(t.TempDir(), "aliased")
	sa := &handler.Static{Alias: alias, Prefix: "/screen/"}
	for _, uri := range []string{"/screen/x", "/screen/../../secret", `/screen/..\..\secret`} {
		u, _ := url.Parse("http://x" + uri)
		got := sa.FilePath(u.Path)
		if got != "" && !strings.HasPrefix(got, alias+string(filepath.Separator)) {
			t.Errorf("alias FilePath(%q) = %q escaped %q", u.Path, got, alias)
		}
	}
}
