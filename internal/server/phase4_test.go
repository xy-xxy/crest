package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestStreamTCP(t *testing.T) {
	// TCP echo backend
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(c)
		}
	}()

	listen := freeAddr(t)
	conf := fmt.Sprintf(`
stream {
    upstream tcp_backend {
        server %s;
    }
    server {
        listen %s;
        proxy_pass tcp_backend;
        proxy_timeout 5s;
    }
}
`, backend.Addr().String(), listen)

	inst := buildInstance(t, conf)
	errc := inst.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		inst.Shutdown(ctx)
	}()
	select {
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}

	c, err := net.DialTimeout("tcp", listen, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	msg := "hello stream"
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != msg {
		t.Errorf("tcp echo through proxy: got %q, want %q", buf[:n], msg)
	}
}

func TestStreamUDP(t *testing.T) {
	// UDP echo backend
	backend, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := backend.ReadFrom(buf)
			if err != nil {
				return
			}
			backend.WriteTo(buf[:n], addr)
		}
	}()

	listen := freeAddr(t)
	conf := fmt.Sprintf(`
stream {
    server {
        listen %s udp;
        proxy_pass %s;
        proxy_timeout 5s;
        proxy_responses 1;
    }
}
`, listen, backend.LocalAddr().String())

	inst := buildInstance(t, conf)
	errc := inst.Start()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		inst.Shutdown(ctx)
	}()
	select {
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(100 * time.Millisecond):
	}

	c, err := net.Dial("udp", listen)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping-udp")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "ping-udp" {
		t.Errorf("udp echo through proxy: got %q", buf[:n])
	}
}

func TestErrorPage(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "50x.html"), "<h1>custom error page</h1>")

	// upstream that answers 500 with its own body
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, "backend-error-body")
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)

	// a dead backend for the gonx-generated 502 case
	dead := freeAddr(t)

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        root "%s";
        error_page 404 500 502 /50x.html;
        location /api/ {
            proxy_pass http://%s;
        }
        location /down/ {
            proxy_pass http://%s;
        }
        location = /50x.html {
        }
    }
}
`, filepath.ToSlash(dir), bu.Host, dead)

	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	// gonx's own 404 → custom page with status 404
	if code, body := get(t, ts, "/missing", ""); code != 404 || !strings.Contains(body, "custom error page") {
		t.Errorf("404 error_page: %d %q", code, body)
	}
	// upstream 500 passes through untouched (proxy_intercept_errors off)
	if code, body := get(t, ts, "/api/x", ""); code != 500 || body != "backend-error-body" {
		t.Errorf("upstream 500 must not be intercepted: %d %q", code, body)
	}
	// gonx-generated 502 (backend down) → custom page with status 502
	if code, body := get(t, ts, "/down/x", ""); code != 502 || !strings.Contains(body, "custom error page") {
		t.Errorf("502 error_page: %d %q", code, body)
	}
}

func TestClientMaxBodySize(t *testing.T) {
	conf := `
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        location = /upload {
            client_max_body_size 10;
            return 200 "ok";
        }
    }
}
`
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/upload", "text/plain", strings.NewReader("0123456789ABCDEF"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", resp.StatusCode)
	}
	resp2, err := http.Post(ts.URL+"/upload", "text/plain", strings.NewReader("tiny"))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Errorf("small body: %d, want 200", resp2.StatusCode)
	}
}
