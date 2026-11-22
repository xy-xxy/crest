package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestActuatorDefaultProtection(t *testing.T) {
	for _, format := range []string{"nginx", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var inst *Instance
			if format == "nginx" {
				inst = buildInstance(t, `http {
    access_log off;
    server {
        listen 8080;
        server_name first.test;
        location = /actuator { return 200 "exact"; }
        location ^~ /api/ { return 200 "priority"; }
        location / { return 200 "public"; }
    }
    server {
        listen 8080;
        server_name second.test;
        location / { return 200 "public"; }
    }
}`)
			} else {
				inst = buildFromYAML(t, `http:
  access_log: {off: true}
  servers:
    - listen: [8080]
      server_name: [first.test]
      locations:
        - {path: /actuator, match: exact, return: {code: 200, body: exact}}
        - {path: /api/, match: prefix_priority, return: {code: 200, body: priority}}
        - {path: /, return: {code: 200, body: public}}
    - listen: [8080]
      server_name: [second.test]
      locations:
        - {path: /, return: {code: 200, body: public}}
`)
			}
			t.Cleanup(func() { inst.Shutdown(context.Background()) })
			var denied, total int64
			for _, host := range []string{"first.test", "second.test", "unknown.test"} {
				for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"} {
					for _, path := range []string{
						"/actuator", "/actuator/health", "/ACTUATOR", "/api/AcTuAtOr/env",
						"/actuator;jsessionid=123/env", "/api/actuator;", "/actuator?probe=true",
						"/%61ctuator/env", "/api%2Factuator%2Fenv", "/actuator%3Bsession/env",
						"//actuator//env", "/./actuator/env",
					} {
						r := httptest.NewRequest(method, "http://"+host+path, nil)
						w := httptest.NewRecorder()
						inst.Addrs[0].ServeHTTP(w, r)
						assertActuatorDenied(t, w, method)
						denied++
						total++
					}
				}
				for _, path := range []string{
					"/", "/actuators", "/myactuator", "/actuator-info", "/actuator.json",
					"/actuator_health", "/public?next=/actuator/env", "/healthz", "/readyz",
				} {
					w := httptest.NewRecorder()
					inst.Addrs[0].ServeHTTP(w, httptest.NewRequest("GET", "http://"+host+path, nil))
					if w.Code != http.StatusOK || w.Body.String() != "public" {
						t.Fatalf("allowed path %q: %d %q", path, w.Code, w.Body.String())
					}
					total++
				}
			}
			stats := inst.Stats()
			if stats.Total != total || stats.C4xx != denied || stats.C2xx != total-denied {
				t.Fatalf("statistics lost requests: %+v; total=%d denied=%d", stats, total, denied)
			}
		})
	}
}

func assertActuatorDenied(t *testing.T, w *httptest.ResponseRecorder, method string) {
	t.Helper()
	wantBody := "Forbidden"
	if method == http.MethodHead {
		wantBody = ""
	}
	if w.Code != http.StatusForbidden || w.Body.String() != wantBody {
		t.Fatalf("%s: got %d %q, want 403 %q", method, w.Code, w.Body.String(), wantBody)
	}
	if w.Header().Get("Content-Length") != "9" || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("denial headers: %v", w.Header())
	}
}

func TestActuatorSettingInheritance(t *testing.T) {
	for _, format := range []string{"nginx", "yaml"} {
		for _, global := range []string{"unset", "on", "off"} {
			for _, site := range []string{"unset", "on", "off"} {
				t.Run(format+"/http="+global+"/server="+site, func(t *testing.T) {
					var inst *Instance
					if format == "nginx" {
						directive := func(value string) string {
							if value == "unset" {
								return ""
							}
							return "block_actuator " + value + ";"
						}
						inst = buildInstance(t, fmt.Sprintf(`http {
    access_log off;
    %s
    server {
        listen 8080; server_name site.test;
        %s
        location / { return 200 "allowed"; }
    }
    server {
        listen 8080; server_name inherited.test;
        location / { return 200 "allowed"; }
    }
}`, directive(global), directive(site)))
					} else {
						directive := func(value, indent string) string {
							if value == "unset" {
								return ""
							}
							return fmt.Sprintf("%sblock_actuator: %t\n", indent, value == "on")
						}
						inst = buildFromYAML(t, fmt.Sprintf(`http:
  access_log: {off: true}
%s  servers:
    - listen: [8080]
      server_name: [site.test]
%s      locations:
        - {path: /, return: {code: 200, body: allowed}}
    - listen: [8080]
      server_name: [inherited.test]
      locations:
        - {path: /, return: {code: 200, body: allowed}}
`, directive(global, "  "), directive(site, "      ")))
					}
					defer inst.Shutdown(context.Background())
					for _, host := range []string{"site.test", "inherited.test"} {
						blocked := global != "off"
						if host == "site.test" && site != "unset" {
							blocked = site == "on"
						}
						w := httptest.NewRecorder()
						inst.Addrs[0].ServeHTTP(w, httptest.NewRequest("GET", "http://"+host+"/actuator/env", nil))
						if blocked {
							assertActuatorDenied(t, w, "GET")
						} else if w.Code != http.StatusOK || w.Body.String() != "allowed" {
							t.Fatalf("%s: disabled protection returned %d %q", host, w.Code, w.Body.String())
						}
					}
				})
			}
		}
	}
}

func TestActuatorDisabledPreservesRouting(t *testing.T) {
	dir := t.TempDir()
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, r.URL.Path)
	}))
	defer backend.Close()
	inst := buildInstance(t, fmt.Sprintf(`http {
    access_log off;
    server {
        listen 8080;
        block_actuator off;
        root %q;
        error_page 404 /actuator/error;
        rewrite ^/server-rewrite$ /actuator/env last;
        location /last { rewrite ^/last$ /actuator/env last; }
        location /break {
            rewrite ^/break$ /actuator/env break;
            proxy_pass %s;
        }
        location /fallback { try_files /nonexistent /actuator/env; }
        location /missing { return 404; }
        location = /actuator/manual { return 403 "manual rule"; }
        location / { proxy_pass %s; }
    }
}`, filepath.ToSlash(dir), backend.URL, backend.URL))
	defer inst.Shutdown(context.Background())
	for _, path := range []string{"/actuator/env", "/server-rewrite", "/last", "/break", "/fallback", "/missing"} {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			inst.Addrs[0].ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
			status, body := http.StatusOK, "/actuator/env"
			if path == "/missing" {
				status, body = http.StatusNotFound, "/actuator/error"
			}
			if w.Code != status || w.Body.String() != body {
				t.Fatalf("disabled protection changed routing: %d %q, want %d %q", w.Code, w.Body.String(), status, body)
			}
		})
	}
	if hits.Load() != 6 {
		t.Fatalf("upstream hits = %d, want 6", hits.Load())
	}
	w := httptest.NewRecorder()
	inst.Addrs[0].ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/actuator/manual", nil))
	if w.Code != http.StatusForbidden || w.Body.String() != "manual rule" || hits.Load() != 6 {
		t.Fatalf("explicit location rule was overridden: %d %q, hits=%d", w.Code, w.Body.String(), hits.Load())
	}
}

func TestActuatorDoesNotReachProxy(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "backend")
	}))
	defer backend.Close()
	inst := buildInstance(t, fmt.Sprintf(`http {
    access_log off;
    server { listen 8080; location / { proxy_pass %s; } }
}`, backend.URL))
	defer inst.Shutdown(context.Background())
	for _, tls := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", tls), func(t *testing.T) {
			ts := httptest.NewUnstartedServer(inst.Addrs[0])
			if tls {
				ts.StartTLS()
			} else {
				ts.Start()
			}
			defer ts.Close()
			before := hits.Load()
			for _, path := range []string{"/actuator", "/app/ACTUATOR/env", "/%61ctuator%3Bsession"} {
				if code, body := get(t, ts, path, ""); code != 403 || body != "Forbidden" {
					t.Fatalf("%s: %d %q", path, code, body)
				}
			}
			if hits.Load() != before {
				t.Fatal("actuator request reached upstream")
			}
			if code, body := get(t, ts, "/public", ""); code != 200 || body != "backend" {
				t.Fatalf("normal proxy request: %d %q", code, body)
			}
			if hits.Load() != before+1 {
				t.Fatal("normal proxy request did not reach upstream")
			}
		})
	}
}

func TestActuatorInternalRedirectsAndAccessLog(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "actuator"), 0755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "actuator", "env"), "must not be served")
	logPath := filepath.Join(dir, "access.log")
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "must not reach backend")
	}))
	defer backend.Close()
	inst := buildInstance(t, fmt.Sprintf(`http {
    log_format security '$status $request';
    access_log %q security;
    server {
        listen 8080;
        root %q;
        error_page 403 /custom403;
        error_page 404 /actuator/env;
        rewrite ^/server-rewrite$ /actuator/env last;
        rewrite ^/actuator$ /public last;
        location /last { rewrite ^/last$ /actuator/env last; }
        location /break { rewrite ^/break$ /actuator/env break; }
        location /fallback { try_files /nonexistent /actuator/env; }
        location /file { try_files /actuator/env =404; }
        location /named {
            rewrite ^/named$ /actuator/env break;
            try_files /nonexistent @backend;
        }
        location @backend { proxy_pass %s; }
        location /missing { return 404; }
        location /custom403 { proxy_pass %s; }
        location /public { return 200 "public"; }
        location / { proxy_pass %s; }
    }
}`, filepath.ToSlash(logPath), filepath.ToSlash(dir), backend.URL, backend.URL, backend.URL))
	defer inst.Shutdown(context.Background())
	paths := []string{"/actuator", "/server-rewrite", "/last", "/break", "/fallback", "/file", "/named", "/missing"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w := httptest.NewRecorder()
			inst.Addrs[0].ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
			assertActuatorDenied(t, w, "GET")
		})
	}
	if hits.Load() != 0 {
		t.Fatalf("denial or error-page handling reached upstream %d times", hits.Load())
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if !strings.Contains(string(logData), "403 GET http://localhost"+path+" HTTP/1.1") {
			t.Errorf("missing 403 access-log record for %s: %s", path, logData)
		}
	}
}
