package handler

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"time"
)

// WrapLimitRate throttles the response body to rate bytes/sec, matching
// ngx_http_core_module's limit_rate. after bytes are sent full-speed first
// (limit_rate_after). rate <= 0 disables throttling.
func WrapLimitRate(h http.Handler, rate, after int64) http.Handler {
	if rate <= 0 {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&rateWriter{ResponseWriter: w, rate: rate, after: after, start: time.Now()}, r)
	})
}

type rateWriter struct {
	http.ResponseWriter
	rate  int64 // bytes/sec
	after int64 // unthrottled prefix
	sent  int64
	start time.Time
}

func (w *rateWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		// unthrottled prefix
		if w.sent < w.after {
			room := int(w.after - w.sent)
			if room > len(b) {
				room = len(b)
			}
			n, err := w.ResponseWriter.Write(b[:room])
			written += n
			w.sent += int64(n)
			b = b[n:]
			if err != nil {
				return written, err
			}
			continue
		}
		// throttled: send at most one "second's worth" per iteration, then
		// sleep until the schedule catches up
		chunk := int(w.rate)
		if chunk < 1 {
			chunk = 1
		}
		if chunk > len(b) {
			chunk = len(b)
		}
		n, err := w.ResponseWriter.Write(b[:chunk])
		written += n
		w.sent += int64(n)
		b = b[n:]
		if err != nil {
			return written, err
		}
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		// how long these bytes "should" have taken since limiting began
		limited := w.sent - w.after
		want := time.Duration(float64(limited) / float64(w.rate) * float64(time.Second))
		if ahead := want - time.Since(w.start); ahead > 0 {
			time.Sleep(ahead)
		}
	}
	return written, nil
}

func (w *rateWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *rateWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}
