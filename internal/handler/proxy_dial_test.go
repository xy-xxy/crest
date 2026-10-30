package handler

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDialLimitCancellationAndReuse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDial := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseDial()
	var calls atomic.Int32
	dial := limitDials(func(ctx context.Context, _, _ string) (net.Conn, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}, 1, 5*time.Second)
	first := make(chan net.Conn, 1)
	go func() {
		conn, err := dial(context.Background(), "tcp", "test")
		if err != nil {
			t.Error(err)
		}
		first <- conn
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dial(ctx, "tcp", "test"); !errors.Is(err, context.Canceled) {
		t.Errorf("queued cancellation: %v", err)
	}
	if calls.Load() != 1 {
		t.Error("canceled waiter reached underlying dialer")
	}
	releaseDial()
	conn := <-first
	if conn == nil {
		t.Fatal("initial dial failed")
	}
	defer conn.Close()
	// A live connection must not occupy a dial slot (SSE/WebSocket support).
	second, err := dial(context.Background(), "tcp", "test")
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestDialLimitReleasesSlotAfterError(t *testing.T) {
	want := errors.New("dial failed")
	dial := limitDials(func(context.Context, string, string) (net.Conn, error) { return nil, want }, 1, time.Second)
	for i := 0; i < 3; i++ {
		if _, err := dial(context.Background(), "tcp", "test"); !errors.Is(err, want) {
			t.Fatalf("slot leaked after failure: %v", err)
		}
	}
}

func TestProxyTransportKeepaliveOverride(t *testing.T) {
	for _, tc := range []struct{ configured, want int }{{0, 512}, {8, 8}} {
		h, err := NewProxy("http://127.0.0.1:8080", ProxyOptions{Keepalive: tc.configured}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		tr := h.(*httputil.ReverseProxy).Transport.(*http.Transport)
		if tr.MaxIdleConnsPerHost != tc.want {
			t.Fatalf("keepalive=%d: got %d", tc.configured, tr.MaxIdleConnsPerHost)
		}
		if tr.MaxConnsPerHost != 0 {
			t.Fatal("dial budget must not limit long-lived active connections")
		}
	}
}
