package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Regression: \n and \t in nginx config strings reached clients and logs as
// a literal backslash and letter, unlike nginx, which resolves them.
func TestConfigEscapesReachResponsesAndLogs(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "access.log")
	inst := buildInstance(t, fmt.Sprintf(`http {
    log_format tsv '$status\t$request_method\t$uri';
    access_log %q tsv;
    server {
        listen 8080;
        location /quote { add_header X-Quote "it\'s"; return 200 'a\tb'; }
        location / { return 200 "ok\n"; }
    }
}`, filepath.ToSlash(logPath)))
	defer inst.Shutdown(context.Background())

	serve := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		inst.Addrs[0].ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
		return w
	}
	if w := serve("/"); w.Body.String() != "ok\n" {
		t.Errorf(`return 200 "ok\n" body = %q`, w.Body.String())
	}
	w := serve("/quote")
	if w.Body.String() != "a\tb" {
		t.Errorf(`return 200 'a\tb' body = %q`, w.Body.String())
	}
	if got := w.Header().Get("X-Quote"); got != "it's" {
		t.Errorf(`add_header "it\'s" = %q`, got)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "200\tGET\t/\n200\tGET\t/quote\n"; string(logData) != want {
		t.Errorf("tab-separated access log = %q, want %q", logData, want)
	}
}

// The YAML front-end gets the same values from YAML's own double-quoted
// escapes, so both formats produce the same response.
func TestConfigEscapesMatchYAML(t *testing.T) {
	fromNginx := buildInstance(t, `http { server { listen 8080; location / { return 200 "line1\nline2\t!"; } } }`)
	defer fromNginx.Shutdown(context.Background())
	fromYAML := buildFromYAML(t, `
http:
  servers:
    - listen: [8080]
      locations:
        - path: /
          return: {code: 200, body: "line1\nline2\t!"}
`)
	defer fromYAML.Shutdown(context.Background())
	for name, inst := range map[string]*Instance{"nginx": fromNginx, "yaml": fromYAML} {
		w := httptest.NewRecorder()
		inst.Addrs[0].ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/", nil))
		if w.Body.String() != "line1\nline2\t!" {
			t.Errorf("%s body = %q", name, w.Body.String())
		}
	}
}
