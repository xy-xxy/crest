package handler

import (
	"bufio"
	"compress/gzip"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// GzipConfig holds resolved gzip settings for one location.
type GzipConfig struct {
	Types     map[string]bool // content types to compress; "*" = all. text/html always.
	MinLength int64
	Level     int
}

// WrapGzip compresses responses like ngx_http_gzip_module. Nil config
// disables it.
func WrapGzip(h http.Handler, cfg *GzipConfig) http.Handler {
	if cfg == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Method == http.MethodHead {
			h.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w, cfg: cfg}
		defer gw.close()
		h.ServeHTTP(gw, r)
	})
}

type gzipWriter struct {
	http.ResponseWriter
	cfg      *GzipConfig
	gz       *gzip.Writer
	decided  bool
	compress bool
}

func (w *gzipWriter) WriteHeader(code int) {
	if !w.decided {
		w.decide(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipWriter) decide(code int) {
	w.decided = true
	h := w.Header()
	// 206 stays uncompressed: gzip would invalidate Content-Range
	switch code {
	case http.StatusPartialContent, http.StatusNoContent, http.StatusNotModified:
		return
	}
	if h.Get("Content-Encoding") != "" {
		return
	}
	ct := h.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if ct != "text/html" && !w.cfg.Types["*"] && !w.cfg.Types[ct] {
		return
	}
	if cl := h.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n < w.cfg.MinLength {
			return
		}
	}
	w.compress = true
	h.Del("Content-Length")
	h.Del("Accept-Ranges")
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	w.gz, _ = gzip.NewWriterLevel(w.ResponseWriter, w.cfg.Level)
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	if !w.decided {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.decide(http.StatusOK)
	}
	if w.gz != nil {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *gzipWriter) close() {
	if w.gz != nil {
		w.gz.Close()
	}
}

func (w *gzipWriter) Flush() {
	if w.gz != nil {
		w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}
