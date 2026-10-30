package handler

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"

	"crest/internal/logging"
)

// BufferOptions sizes the proxy response buffer, mirroring
// ngx_http_proxy_module's proxy_buffers / proxy_max_temp_file_size.
type BufferOptions struct {
	// MemLimit is how many response bytes may sit in memory before the
	// buffer starts spooling to a temporary file. <= 0 uses defaultBufferMem.
	MemLimit int64
	// MaxTempFile caps the spool file. A negative value disables the disk
	// buffer entirely, so a response larger than MemLimit falls back to
	// back-pressure — the proxy_buffering off behaviour — instead of
	// touching the disk.
	MaxTempFile int64
	// TempDir holds the spool files; empty uses the OS temp directory.
	TempDir string
	ErrLog  *logging.ErrorLog
}

const (
	// 8 * 32k. nginx defaults to 8 * 4k, which pushes all but the smallest
	// responses onto disk; a larger default keeps ordinary API responses
	// entirely in memory while still bounding one request's footprint.
	defaultBufferMem = 256 << 10
	// nginx's proxy_max_temp_file_size default.
	defaultMaxTempFile = 1 << 30
	drainChunk         = 32 << 10
)

// WrapProxyBuffering decouples reading the upstream response from writing it
// to the client, which is the whole point of nginx's proxy_buffering: the
// backend connection is released as soon as the response has been read,
// rather than being held open for as long as the slowest client takes to
// receive it.
//
// Two details differ from nginx, both deliberate:
//
//   - The drain forwards whatever it has immediately instead of waiting for a
//     buffer to fill, so server-sent events and other incremental responses
//     keep their latency with buffering left on.
//   - Spooling to the temp file happens on its own goroutine. Writing it
//     inline puts disk latency back on the upstream's critical path, which
//     measured no better than not buffering at all.
func WrapProxyBuffering(h http.Handler, opts BufferOptions) http.Handler {
	if opts.MemLimit <= 0 {
		opts.MemLimit = defaultBufferMem
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bw := newBufferedWriter(w, opts)
		defer bw.finish()
		h.ServeHTTP(bw, r)
	})
}

// segment is one piece of the response, held in FIFO order. It lives either
// in memory or at an offset in the spool file; the spool goroutine converts
// segments in place, so moving data to disk never reorders it.
type segment struct {
	mem []byte // nil once spooled
	off int64  // offset in the spool file
	n   int
}

// bufferedWriter accepts the proxy's writes without blocking on the client or
// on the disk.
type bufferedWriter struct {
	out  http.ResponseWriter
	opts BufferOptions

	// hdr is owned by this writer rather than shared with the client's:
	// ReverseProxy touches Header() again after WriteHeader (to fill
	// trailers) while the drain is already writing to out, and one shared map
	// would race. WriteHeader clones it for the drain, which copies any late
	// keys across at the end.
	hdr http.Header

	mu   sync.Mutex
	cond *sync.Cond

	code     int
	codeSet  bool
	snapshot http.Header

	segs     []segment
	popped   int64 // segments the drain has removed from the front
	memBytes int64
	spill    *os.File
	spillW   int64 // next free offset in the spool file
	noSpill  bool  // disk buffer disabled or unavailable

	done     bool  // the proxy finished writing
	err      error // the client went away
	hijacked bool

	drained chan struct{}
	spooled chan struct{}
}

func newBufferedWriter(w http.ResponseWriter, opts BufferOptions) *bufferedWriter {
	b := &bufferedWriter{
		out: w, opts: opts, hdr: http.Header{},
		drained: make(chan struct{}), spooled: make(chan struct{}),
	}
	b.cond = sync.NewCond(&b.mu)
	b.noSpill = opts.MaxTempFile < 0
	go b.spool()
	go b.drain()
	return b
}

func (b *bufferedWriter) Header() http.Header { return b.hdr }

func (b *bufferedWriter) WriteHeader(code int) {
	b.mu.Lock()
	b.startLocked(code)
	b.mu.Unlock()
}

func (b *bufferedWriter) startLocked(code int) {
	if !b.codeSet {
		b.code, b.codeSet = code, true
		b.snapshot = b.hdr.Clone()
		b.cond.Broadcast()
	}
}

// hardMemLimit is where the writer starts waiting for the spool or the drain
// to catch up. Slack above MemLimit keeps the upstream moving while the spool
// works through its backlog.
func (b *bufferedWriter) hardMemLimit() int64 {
	if b.noSpill {
		return b.opts.MemLimit
	}
	return b.opts.MemLimit * 2
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startLocked(http.StatusOK)
	written := 0
	for len(p) > 0 {
		if b.err != nil {
			return written, b.err
		}
		if b.memBytes >= b.hardMemLimit() {
			// Nothing to do but wait: either the spool moves memory onto disk
			// or the drain hands bytes to the client. With no disk buffer
			// this is the back-pressure proxy_buffering off would give.
			b.cond.Wait()
			continue
		}
		n := int64(len(p))
		if room := b.hardMemLimit() - b.memBytes; n > room {
			n = room
		}
		chunk := make([]byte, n)
		copy(chunk, p[:n])
		b.segs = append(b.segs, segment{mem: chunk, n: int(n)})
		b.memBytes += n
		written += int(n)
		p = p[n:]
		b.cond.Broadcast()
	}
	return written, nil
}

// spool moves the newest in-memory segments to the temp file once the memory
// budget is exceeded, keeping the front of the queue — the bytes the drain is
// about to send — in memory. It never touches the front segment, so the drain
// never waits on a conversion.
func (b *bufferedWriter) spool() {
	defer close(b.spooled)
	for {
		b.mu.Lock()
		for !b.done && b.err == nil && !b.hijacked && !b.noSpill &&
			!(b.memBytes > b.opts.MemLimit && len(b.segs) > 1) {
			b.cond.Wait()
		}
		if b.err != nil || b.hijacked || b.noSpill {
			b.mu.Unlock()
			return
		}
		idx := -1
		if b.memBytes > b.opts.MemLimit {
			for i := len(b.segs) - 1; i > 0; i-- {
				if b.segs[i].mem != nil {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			done := b.done
			b.mu.Unlock()
			if done {
				return
			}
			continue
		}
		max := b.opts.MaxTempFile
		if max == 0 {
			max = defaultMaxTempFile
		}
		seg := b.segs[idx]
		if b.spillW+int64(seg.n) > max || !b.openSpillLocked() {
			// no room on disk, or no disk at all: fall back to back-pressure
			b.noSpill = true
			b.cond.Broadcast()
			b.mu.Unlock()
			return
		}
		off, f, pop0 := b.spillW, b.spill, b.popped
		b.spillW += int64(seg.n)
		b.mu.Unlock()

		// the disk write happens off the lock and off the upstream's goroutine
		_, err := f.WriteAt(seg.mem, off)

		b.mu.Lock()
		// The drain pops from the front while the lock is released, shifting
		// every index down. Re-anchor before touching the slice, or this
		// would convert a different segment and duplicate its bytes.
		idx -= int(b.popped - pop0)
		switch {
		case err != nil:
			b.logf("proxy buffer: spool write failed, falling back to unbuffered: %v", err)
			b.noSpill = true
			b.spillW = off
		case idx >= 0 && idx < len(b.segs) && b.segs[idx].mem != nil && b.segs[idx].n == seg.n:
			b.segs[idx] = segment{off: off, n: seg.n}
			b.memBytes -= int64(seg.n)
		}
		b.cond.Broadcast()
		b.mu.Unlock()
	}
}

// openSpillLocked creates the temp file on first use. A failure is not fatal:
// the caller falls back to back-pressure, which is slower but still correct.
func (b *bufferedWriter) openSpillLocked() bool {
	if b.spill != nil {
		return true
	}
	f, err := os.CreateTemp(b.opts.TempDir, "crest-proxy-*")
	if err != nil {
		b.logf("proxy buffer: cannot create spool file, falling back to unbuffered: %v", err)
		return false
	}
	b.spill = f
	return true
}

func (b *bufferedWriter) logf(format string, args ...any) {
	if b.opts.ErrLog != nil {
		b.opts.ErrLog.Logf(logging.LevelWarn, format, args...)
	}
}

// Flush is a signal, not a barrier: the drain already forwards everything it
// has as soon as it has it, and flushes the client after each write.
func (b *bufferedWriter) Flush() {
	b.mu.Lock()
	b.cond.Broadcast()
	b.mu.Unlock()
}

// Hijack bypasses the buffer entirely — a 101 upgrade takes over the
// connection and the proxy writes the raw response itself.
func (b *bufferedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := b.out.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	b.mu.Lock()
	b.hijacked = true
	b.cond.Broadcast()
	b.mu.Unlock()
	return hj.Hijack()
}

// finish marks the response complete and waits for the drain to hand
// everything to the client: the ResponseWriter dies with the handler.
func (b *bufferedWriter) finish() {
	b.mu.Lock()
	b.done = true
	b.cond.Broadcast()
	b.mu.Unlock()
	<-b.drained
	<-b.spooled
	if b.spill != nil {
		name := b.spill.Name()
		b.spill.Close()
		os.Remove(name)
	}
}

// drain forwards buffered bytes to the client at the client's own pace.
func (b *bufferedWriter) drain() {
	defer close(b.drained)
	rbuf := make([]byte, drainChunk)
	headerSent := false
	for {
		b.mu.Lock()
		for !b.done && b.err == nil && !b.hijacked && len(b.segs) == 0 && (!b.codeSet || headerSent) {
			b.cond.Wait()
		}
		if b.err != nil || b.hijacked {
			b.mu.Unlock()
			return
		}
		code, codeSet, snap := b.code, b.codeSet, b.snapshot
		var seg segment
		var have bool
		if len(b.segs) > 0 {
			seg, have = b.segs[0], true
			b.segs = b.segs[1:]
			b.popped++
			if seg.mem != nil {
				b.memBytes -= int64(seg.n)
			}
			b.cond.Broadcast() // freed memory may release a waiting writer
		}
		last := !have && b.done
		f := b.spill
		b.mu.Unlock()

		if codeSet && !headerSent {
			copyInto(b.out.Header(), snap)
			b.out.WriteHeader(code)
			headerSent = true
		}
		if have {
			data := seg.mem
			if data == nil {
				buf := rbuf
				if seg.n > len(buf) {
					buf = make([]byte, seg.n)
				}
				n, err := f.ReadAt(buf[:seg.n], seg.off)
				if err != nil && n < seg.n {
					b.fail(err)
					return
				}
				data = buf[:n]
			}
			if _, err := b.out.Write(data); err != nil {
				b.fail(err)
				return
			}
			if fl, ok := b.out.(http.Flusher); ok {
				fl.Flush()
			}
			continue
		}
		if last {
			// The proxy has returned, so hdr is stable. Carry over only what
			// it added *after* WriteHeader — real trailers. Re-copying the
			// staged headers would undo a downstream writer that dropped them
			// on purpose (error_page rebuilding the response from scratch).
			dst := b.out.Header()
			for k, vv := range b.hdr {
				if headerSent {
					if _, staged := snap[k]; staged {
						continue
					}
				}
				if _, seen := dst[k]; !seen {
					dst[k] = append([]string(nil), vv...)
				}
			}
			if !headerSent && codeSet {
				b.out.WriteHeader(code)
			}
			return
		}
	}
}

// copyInto publishes the staged headers onto the client's writer. Only the
// drain goroutine ever touches the client's header map.
func copyInto(dst, src http.Header) {
	for k, vv := range src {
		dst[k] = append([]string(nil), vv...)
	}
}

// fail records a client-side write error so the proxy stops reading the
// upstream: there is no one left to send the rest of the body to.
func (b *bufferedWriter) fail(err error) {
	b.mu.Lock()
	if b.err == nil {
		b.err = err
	}
	b.cond.Broadcast()
	b.mu.Unlock()
}

var _ io.Writer = (*bufferedWriter)(nil)
