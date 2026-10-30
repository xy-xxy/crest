package server

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type stubListener struct {
	incoming chan accepted
	observed chan struct{}
	done     chan struct{}
	once     sync.Once
}

func newStubListener() *stubListener {
	return &stubListener{incoming: make(chan accepted, 8), observed: make(chan struct{}, 8), done: make(chan struct{})}
}
func (l *stubListener) Accept() (net.Conn, error) {
	select {
	case a := <-l.incoming:
		l.observed <- struct{}{}
		return a.conn, a.err
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *stubListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *stubListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234} }

func TestQueuedListenerCloseDrainsPendingConnections(t *testing.T) {
	base := newStubListener()
	q := queueListener(base, 2)
	defer q.Close()
	var peers []net.Conn
	for i := 0; i < 3; i++ {
		a, b := net.Pipe()
		peers = append(peers, b)
		defer b.Close()
		base.incoming <- accepted{conn: a}
	}
	// The queue accepts ahead of the HTTP consumer, but only holds size+1 conns.
	for i := 0; i < 3; i++ {
		select {
		case <-base.observed:
		case <-time.After(time.Second):
			t.Fatal("accept pump stalled")
		}
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		p.SetReadDeadline(time.Now().Add(time.Second))
		var b [1]byte
		if _, err := p.Read(b[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("pending connection left open: %v", err)
		}
	}
	if _, err := q.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept after close: %v", err)
	}
}
func TestQueuedListenerKeepsDeliveredConnectionOpen(t *testing.T) {
	base := newStubListener()
	q := queueListener(base, 2)
	defer q.Close()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	base.incoming <- accepted{conn: a}
	conn, err := q.Accept()
	if err != nil || conn != a {
		t.Fatalf("Accept=%v %v", conn, err)
	}
	q.Close()
	done := make(chan error, 1)
	go func() { _, err := conn.Write([]byte("x")); done <- err }()
	b.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if _, err := b.Read(buf[:]); err != nil || buf[0] != 'x' {
		t.Fatalf("active connection closed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestQueuedListenerCloseUnblocksAccept(t *testing.T) {
	q := queueListener(newStubListener(), 2)
	done := make(chan error, 1)
	go func() { _, err := q.Accept(); done <- err }()
	q.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept stayed blocked")
	}
}

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "temporary accept failure" }
func (temporaryAcceptError) Timeout() bool   { return false }
func (temporaryAcceptError) Temporary() bool { return true }
func TestQueuedListenerAcceptErrors(t *testing.T) {
	base := newStubListener()
	q := queueListener(base, 2)
	defer q.Close()
	base.incoming <- accepted{err: temporaryAcceptError{}}
	if _, err := q.Accept(); err == nil {
		t.Fatal("lost temporary error")
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	base.incoming <- accepted{conn: a}
	if conn, err := q.Accept(); err != nil || conn != a {
		t.Fatalf("temporary error prevented recovery: %v", err)
	}
	want := errors.New("fatal accept error")
	base.incoming <- accepted{err: want}
	if _, err := q.Accept(); !errors.Is(err, want) {
		t.Fatalf("fatal error=%v", err)
	}
}

func TestQueuedListenerTLS(t *testing.T) {
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	ts.Listener = queueListener(ts.Listener, 2)
	ts.StartTLS()
	defer ts.Close()
	resp, err := ts.Client().Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("TLS through accept queue: %q %v", body, err)
	}
}
