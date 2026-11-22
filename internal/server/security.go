package server

import (
	"io"
	"net/http"
	"regexp"
)

var actuatorPath = regexp.MustCompile(`(?i)(^|/)actuator([/;]|$)`)

func resolveBlockActuator(global, server *bool) bool {
	if server != nil {
		return *server
	}
	if global != nil {
		return *global
	}
	return true
}

// blockActuator checks the decoded URL path, including after internal rewrites.
// It runs before location handlers so exact routes and proxy caches cannot
// override the default protection.
func blockActuator(w http.ResponseWriter, r *http.Request) bool {
	if !actuatorPath.MatchString(r.URL.Path) {
		return false
	}
	// A security denial must not invoke a configured error_page handler, which
	// could proxy the request or replace the denial body.
	if ew, ok := w.(*errorPageWriter); ok {
		w = ew.ResponseWriter
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", "9")
	w.WriteHeader(http.StatusForbidden)
	if r.Method != http.MethodHead {
		io.WriteString(w, "Forbidden")
	}
	return true
}
