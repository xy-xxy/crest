package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wsBackend is a raw HTTP/1.1 listener that completes a websocket handshake
// and then writes one frame-sized payload, so the test can prove bytes flow
// over the hijacked connection.
func wsBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					c.Close()
					return
				}
				if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
					fmt.Fprint(c, "HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n")
					c.Close()
					return
				}
				fmt.Fprint(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\nWSDATA")
			}(c)
		}
	}()
	return ln.Addr().String()
}

// wsHandshake sends an upgrade request through inst and returns the status
// line plus whatever the backend wrote after it.
func wsHandshake(t *testing.T, inst *Instance) (string, string) {
	t.Helper()
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()
	c, err := net.Dial("tcp", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET /socket.io/ HTTP/1.1\r\nHost: example.com\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status line: %v", err)
	}
	for { // drain response headers so the next read sees tunneled bytes
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	buf := make([]byte, 512)
	n, _ := br.Read(buf)
	return strings.TrimSpace(status), string(buf[:n])
}

// TestWebSocketUpgrade guards the Hijack passthrough every response-writer
// wrapper needs: add_header / expires used to swallow it and turn the 101
// into a 502.
func TestWebSocketUpgrade(t *testing.T) {
	be := wsBackend(t)
	loc := fmt.Sprintf(`location /socket.io/ {
			proxy_pass http://%s/socket.io/;
			proxy_set_header Host $host;
			proxy_set_header Upgrade $http_upgrade;
			proxy_set_header Connection "upgrade";
		}`, be)

	cases := []struct{ name, conf string }{
		{"plain", fmt.Sprintf(`http { server { listen 0; %s } }`, loc)},
		{"add_header", fmt.Sprintf(`http { add_header X-Frame-Options SAMEORIGIN; server { listen 0; %s } }`, loc)},
		{"expires", fmt.Sprintf(`http { expires 1h; server { listen 0; %s } }`, loc)},
		{"gzip", fmt.Sprintf(`http { gzip on; server { listen 0; %s } }`, loc)},
		{"error_page", fmt.Sprintf(`http { server { listen 0; error_page 500 502 503 504 /50x.html; %s } }`, loc)},
		// through an upstream pool the response body is wrapped to release
		// the backend on close; the wrapper must stay writable or the 101
		// tunnel cannot be built.
		{"upstream", fmt.Sprintf(`http { upstream wsb { server %s; }
			server { listen 0; location /socket.io/ {
				proxy_pass http://wsb/socket.io/;
				proxy_set_header Upgrade $http_upgrade;
				proxy_set_header Connection "upgrade"; } } }`, be)},
		{"all", fmt.Sprintf(`http { gzip on; add_header X-A b; expires 1h;
			server { listen 0; error_page 502 /50x.html; %s } }`, loc)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, rest := wsHandshake(t, buildInstance(t, tc.conf))
			if !strings.Contains(status, "101") {
				t.Fatalf("want 101 Switching Protocols, got %q", status)
			}
			if !strings.Contains(rest, "WSDATA") {
				t.Errorf("backend payload did not reach the client: %q", rest)
			}
		})
	}
}
