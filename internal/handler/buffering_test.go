package handler

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowWriter is a ResponseWriter that accepts bytes at a fixed rate, standing
// in for a client on a thin link.
type slowWriter struct {
	hdr     http.Header
	body    bytes.Buffer
	code    int
	perByte time.Duration
	mu      sync.Mutex
}

func newSlowWriter(perByte time.Duration) *slowWriter {
	return &slowWriter{hdr: http.Header{}, perByte: perByte}
}

func (s *slowWriter) Header() http.Header  { return s.hdr }
func (s *slowWriter) WriteHeader(code int) { s.code = code }
func (s *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(time.Duration(len(p)) * s.perByte)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}
func (s *slowWriter) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.body.Bytes()...)
}

// TestProxyBufferingReleasesWriterEarly is the point of the feature: the
// handler writing the response (standing in for the upstream read) finishes
// at its own speed instead of being paced by the slow client.
func TestProxyBufferingReleasesWriterEarly(t *testing.T) {
	const size = 512 << 10
	body := bytes.Repeat([]byte("x"), size)

	var writeDone time.Duration
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w.WriteHeader(200)
		w.Write(body)
		writeDone = time.Since(start)
	})

	// ~1µs/byte over 512k is ~0.5s of client time
	client := newSlowWriter(time.Microsecond)
	req := httptest.NewRequest("GET", "/big", nil)
	start := time.Now()
	WrapProxyBuffering(h, BufferOptions{MemLimit: 1 << 20}).ServeHTTP(client, req)
	total := time.Since(start)

	if got := client.received(); !bytes.Equal(got, body) {
		t.Fatalf("client got %d bytes, want %d", len(got), len(body))
	}
	t.Logf("upstream write finished in %v, client finished in %v", writeDone, total)
	if writeDone > total/2 {
		t.Errorf("upstream was paced by the client: write %v vs total %v", writeDone, total)
	}
}

// TestProxyBufferingSpillsToDisk covers a response larger than the memory
// budget: it must still arrive intact and in order, via the temp file.
func TestProxyBufferingSpillsToDisk(t *testing.T) {
	const size = 1 << 20
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i % 251) // non-repeating pattern catches reordering
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for off := 0; off < len(body); off += 7919 {
			end := off + 7919
			if end > len(body) {
				end = len(body)
			}
			w.Write(body[off:end])
		}
	})
	client := newSlowWriter(0)
	// tiny memory budget forces nearly everything through the spill file
	WrapProxyBuffering(h, BufferOptions{MemLimit: 16 << 10}).
		ServeHTTP(client, httptest.NewRequest("GET", "/big", nil))

	got := client.received()
	if len(got) != len(body) {
		t.Fatalf("got %d bytes, want %d", len(got), len(body))
	}
	if !bytes.Equal(got, body) {
		for i := range got {
			if got[i] != body[i] {
				t.Fatalf("content diverges at byte %d", i)
			}
		}
	}
}

// TestProxyBufferingNoSpillBackPressures checks proxy_max_temp_file_size 0:
// with the disk buffer disabled, an oversized response still completes, it
// just falls back to pacing the writer.
func TestProxyBufferingNoSpillBackPressures(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 256<<10)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	client := newSlowWriter(0)
	WrapProxyBuffering(h, BufferOptions{MemLimit: 8 << 10, MaxTempFile: -1}).
		ServeHTTP(client, httptest.NewRequest("GET", "/big", nil))
	if got := client.received(); !bytes.Equal(got, body) {
		t.Errorf("got %d bytes, want %d", len(got), len(body))
	}
}

// TestProxyBufferingKeepsIncrementalLatency guards the divergence from nginx
// that makes buffering safe to leave on: an incremental response is forwarded
// as it arrives rather than held until a buffer fills.
func TestProxyBufferingKeepsIncrementalLatency(t *testing.T) {
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release // hold the response open
		fmt.Fprint(w, "data: second\n\n")
	})

	client := newSlowWriter(0)
	done := make(chan struct{})
	go func() {
		WrapProxyBuffering(h, BufferOptions{}).
			ServeHTTP(client, httptest.NewRequest("GET", "/sse", nil))
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if strings.Contains(string(client.received()), "data: first") {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first event never reached the client while the response was open")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	close(release)
	<-done
	if got := string(client.received()); !strings.Contains(got, "data: second") {
		t.Errorf("second event missing: %q", got)
	}
}

// TestProxyBufferingClientGoesAway makes sure a dead client surfaces as a
// write error to the proxy, so it stops pulling from the upstream.
func TestProxyBufferingClientGoesAway(t *testing.T) {
	var writeErr error
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		for i := 0; i < 10000; i++ {
			if _, err := w.Write(bytes.Repeat([]byte("z"), 4096)); err != nil {
				writeErr = err
				return
			}
		}
	})
	WrapProxyBuffering(h, BufferOptions{MemLimit: 8 << 10, MaxTempFile: -1}).
		ServeHTTP(&deadWriter{hdr: http.Header{}}, httptest.NewRequest("GET", "/big", nil))
	if writeErr == nil {
		t.Error("proxy never saw the client write failure")
	}
}

type deadWriter struct{ hdr http.Header }

func (d *deadWriter) Header() http.Header { return d.hdr }
func (d *deadWriter) WriteHeader(int)     {}
func (d *deadWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}
