package server

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"crest/internal/config"
)

// backendHoldTime proxies a large response to a deliberately slow client and
// reports how long the backend handler stayed busy versus the whole download.
// With proxy_buffering on the backend should finish long before the client.
func backendHoldTime(t *testing.T, confFmt string, yaml bool) (backend, total time.Duration) {
	t.Helper()
	const size = 4 << 20
	body := make([]byte, size)
	var heldMs int64
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Write(body)
		atomic.StoreInt64(&heldMs, int64(time.Since(start)))
	}))
	defer be.Close()
	bu, _ := url.Parse(be.URL)

	var inst *Instance
	if yaml {
		inst = buildFromYAML(t, fmt.Sprintf(confFmt, bu.Host))
	} else {
		inst = buildInstance(t, fmt.Sprintf(confFmt, bu.Host))
	}
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	start := time.Now()
	resp, err := ts.Client().Get(ts.URL + "/big")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64<<10)
	got := 0
	for {
		n, err := resp.Body.Read(buf)
		got += n
		if err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond) // ~12 MB/s client
	}
	total = time.Since(start)
	if got != size {
		t.Fatalf("client received %d bytes, want %d", got, size)
	}
	return time.Duration(atomic.LoadInt64(&heldMs)), total
}

// TestProxyBufferingReleasesBackend is the feature's reason to exist: a slow
// client must not pin the upstream connection.
func TestProxyBufferingReleasesBackend(t *testing.T) {
	const on = `
http {
    access_log off;
    error_log stderr crit;
    server { listen 8080; location / { proxy_pass http://%s; } }
}`
	const off = `
http {
    access_log off;
    error_log stderr crit;
    server { listen 8080; location / { proxy_pass http://%s; proxy_buffering off; } } 
}`
	bufBackend, bufTotal := backendHoldTime(t, on, false)
	rawBackend, rawTotal := backendHoldTime(t, off, false)
	t.Logf("buffering on  (default): backend held %v of %v download", bufBackend, bufTotal)
	t.Logf("proxy_buffering off:     backend held %v of %v download", rawBackend, rawTotal)

	// Compare the two rather than testing an absolute fraction: kernel socket
	// buffers absorb a few MB even unbuffered, and the race detector skews
	// wall-clock numbers. What must hold is that buffering frees the backend
	// while the client is still receiving.
	if bufBackend*4 >= rawBackend {
		t.Errorf("buffering did not release the backend earlier: on=%v off=%v", bufBackend, rawBackend)
	}
	if bufBackend > bufTotal/5 {
		t.Errorf("buffered backend still paced by the client: %v of %v", bufBackend, bufTotal)
	}
}

// TestProxyBufferingYAML runs the same check through the YAML front-end.
func TestProxyBufferingYAML(t *testing.T) {
	const y = `
http:
  access_log: {off: true}
  servers:
    - listen: [8080]
      locations:
        - path: /
          proxy_pass: http://%s
          proxy_buffers: {count: 8, size: 64k}
`
	// A 512k budget against a 4MB body pushes most of the response through the
	// spool file, and backendHoldTime fails unless all 4MB arrive intact.
	// Because spooling runs on its own goroutine the backend is still released
	// early, so the same bound applies as for the in-memory path.
	backend, total := backendHoldTime(t, y, true)
	t.Logf("YAML proxy_buffers 8x64k (spool path): backend held %v of %v", backend, total)
	if backend > total/5 {
		t.Errorf("spooled response paced the backend: %v of %v", backend, total)
	}
}

// TestProxyBufferingBothFrontEnds pins the two loaders to the same result,
// the invariant every directive in crest has to hold.
func TestProxyBufferingBothFrontEnds(t *testing.T) {
	ngx, err := config.Parse(`
http {
    server {
        listen 8080;
        location /a/ {
            proxy_pass http://127.0.0.1:9000/;
            proxy_buffering off;
        }
        location /b/ {
            proxy_pass http://127.0.0.1:9000/;
            proxy_buffers 4 256k;
            proxy_max_temp_file_size 512m;
        }
    }
}`)
	if err != nil {
		t.Fatal(err)
	}
	fromNginx, err := config.Load(ngx)
	if err != nil {
		t.Fatal(err)
	}
	fromYAML, err := config.LoadYAML([]byte(`
http:
  servers:
    - listen: [8080]
      locations:
        - path: /a/
          proxy_pass: http://127.0.0.1:9000/
          proxy_buffering: false
        - path: /b/
          proxy_pass: http://127.0.0.1:9000/
          proxy_buffers: {count: 4, size: 256k}
          proxy_max_temp_file_size: 512m
`))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range fromNginx.HTTP.Servers[0].Locations {
		got := fromYAML.HTTP.Servers[0].Locations[i]
		if (want.ProxyBuffering == nil) != (got.ProxyBuffering == nil) ||
			(want.ProxyBuffering != nil && *want.ProxyBuffering != *got.ProxyBuffering) {
			t.Errorf("location %d proxy_buffering differs: nginx=%v yaml=%v",
				i, deref(want.ProxyBuffering), deref(got.ProxyBuffering))
		}
		if !eq64(want.ProxyBufferMem, got.ProxyBufferMem) {
			t.Errorf("location %d proxy_buffers differs: nginx=%v yaml=%v",
				i, want.ProxyBufferMem, got.ProxyBufferMem)
		}
		if !eq64(want.ProxyMaxTempFile, got.ProxyMaxTempFile) {
			t.Errorf("location %d proxy_max_temp_file_size differs", i)
		}
	}
	if n := fromNginx.HTTP.Servers[0].Locations[1].ProxyBufferMem; n == nil || *n != 4*256*1024 {
		t.Errorf("proxy_buffers 4 256k = %v, want %d", n, 4*256*1024)
	}
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func eq64(a, b *int64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

var _ = io.Discard
