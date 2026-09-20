package server

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"

	"crest/internal/handler"
	"crest/internal/vars"
)

// respWriter captures status and byte count for access logging, and stamps
// the Server header (server_tokens) unless the response already carries one.
type respWriter struct {
	http.ResponseWriter
	status       int
	bytes        int64
	serverHeader string
}

func (w *respWriter) ensureHeaders() {
	if w.serverHeader != "" && w.Header().Get("Server") == "" {
		w.Header().Set("Server", w.serverHeader)
	}
}

func (w *respWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ensureHeaders()
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
		w.ensureHeaders()
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *respWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passthrough keeps Upgrade (websocket) proxying working.
func (w *respWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// errorPageWriter intercepts matching error statuses so dispatch can serve
// the configured error_page instead. Upstream responses are left alone
// (proxy_intercept_errors off) unless the proxy handler marked the response
// as crest-generated.
type errorPageWriter struct {
	http.ResponseWriter
	pages       map[int]string
	proxyRoute  bool
	intercepted int
	started     bool
}

func (w *errorPageWriter) WriteHeader(code int) {
	if w.intercepted != 0 {
		return
	}
	if !w.started {
		fromCrest := !w.proxyRoute || w.Header().Get(handler.InternalErrorHeader) != ""
		if uri := w.pages[code]; uri != "" && fromCrest {
			w.intercepted = code
			// The intercepted response never reaches the client, so neither
			// should the headers it staged: its Content-Type would mislabel
			// the error page (an HTML page served as the 502's text/plain
			// renders as source), and the internal marker would leak. nginx
			// builds the error page as a fresh response; do the same.
			h := w.Header()
			for k := range h {
				delete(h, k)
			}
			return
		}
	}
	w.started = true
	w.Header().Del(handler.InternalErrorHeader)
	w.ResponseWriter.WriteHeader(code)
}

func (w *errorPageWriter) Write(b []byte) (int, error) {
	if w.intercepted != 0 {
		return len(b), nil // discard the original error body
	}
	w.started = true
	return w.ResponseWriter.Write(b)
}

func (w *errorPageWriter) Flush() {
	if w.intercepted != 0 {
		return
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *errorPageWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// statusOverrideWriter forces the original error status onto whatever the
// error page handler writes.
type statusOverrideWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (w *statusOverrideWriter) WriteHeader(int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(w.code)
}

func (w *statusOverrideWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(0)
	}
	return w.ResponseWriter.Write(b)
}

type returnHandler struct {
	code int
	arg  *vars.Template // nil when return has no text/URL argument
}

func (h *returnHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	arg := ""
	if h.arg != nil {
		arg = h.arg.Expand(vars.Request(r))
	}
	switch h.code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		if arg != "" {
			http.Redirect(w, r, arg, h.code)
			return
		}
	}
	if arg != "" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(h.code)
		io.WriteString(w, arg)
		return
	}
	w.WriteHeader(h.code)
}
