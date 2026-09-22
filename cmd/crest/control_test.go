package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crest/internal/server"
)

func TestControlHealthReadyAndStatus(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "crest.conf")
	if err := os.WriteFile(confPath, []byte(`
http {
    access_log off;
    error_log stderr crit;
    server { listen 127.0.0.1:0; location / { return 200 "ok"; } }
}
`), 0644); err != nil {
		t.Fatal(err)
	}
	r := &runner{confPath: confPath, inst: &server.Instance{}, console: io.Discard}
	token := "test-token"
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if !tokenOK(req, token) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h(w, req)
		}
	}
	mux := newControlMux(r, token, auth)
	do := func(path, accept, suppliedToken string) *http.Response {
		req := httptest.NewRequest(http.MethodGet, "http://control.test"+path, nil)
		if suppliedToken != "" {
			req.Header.Set("X-Crest-Token", suppliedToken)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, req)
		resp := recorder.Result()
		return resp
	}

	unauth := do("/healthz", "", "")
	if unauth.StatusCode != http.StatusForbidden {
		t.Fatalf("unauthorized healthz status = %d", unauth.StatusCode)
	}

	resp := do("/healthz", "", token)
	var health map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || health["status"] != "ok" {
		t.Fatalf("healthz = %d %#v", resp.StatusCode, health)
	}

	resp = do("/readyz", "", token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz before Start = %d, want 503", resp.StatusCode)
	}

	resp = do("/status", "", token)
	textBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(textBody), "crest/") {
		t.Fatalf("text status = %d %q", resp.StatusCode, textBody)
	}
	resp = do("/status?format=json", "", token)
	var status map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || status["ready"] != false || status["config_path"] != confPath {
		t.Fatalf("json status = %d %#v", resp.StatusCode, status)
	}
}
