package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crest/internal/config"
)

// fetch makes one request on a fresh connection, so it always reaches
// whichever instance is accepting at that moment.
func fetch(addr, path string) (int, string, error) {
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

func mustFetch(t *testing.T, addr, path, want string) {
	t.Helper()
	code, body, err := fetch(addr, path)
	if err != nil || code != http.StatusOK || body != want {
		t.Fatalf("GET %s%s = %d %q, %v; want 200 %q", addr, path, code, body, err, want)
	}
}

func startInstance(t *testing.T, inst *Instance) <-chan error {
	t.Helper()
	errc := inst.Start()
	select {
	case err := <-errc:
		t.Fatal(err)
	default:
	}
	return errc
}

func shutdownWithin(inst *Instance, d time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	inst.Shutdown(ctx)
}

func assertNoServeError(t *testing.T, errc <-chan error) {
	t.Helper()
	select {
	case err := <-errc:
		t.Fatalf("instance reported a fatal serve error: %v", err)
	default:
	}
}

func returnConf(body string, addrs ...string) string {
	var b strings.Builder
	b.WriteString("http {\n access_log off;\n error_log stderr crit;\n")
	for _, a := range addrs {
		fmt.Fprintf(&b, " server { listen %s; location / { return 200 %q; } }\n", a, body)
	}
	b.WriteString("}\n")
	return b.String()
}

// Regression: reload used to close every listener and wait for in-flight
// requests (up to 5s) before binding again, refusing connections meanwhile.
func TestReloadKeepsListenerWhileOldInstanceDrains(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	slowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		io.WriteString(w, "slow done")
	}))
	defer slowUpstream.Close()
	defer releaseOnce.Do(func() { close(release) })

	addr := freeAddr(t)
	old := buildInstance(t, fmt.Sprintf(`http {
    access_log off;
    error_log stderr crit;
    server {
        listen %s;
        location /slow { proxy_pass %s; }
        location / { return 200 "old"; }
    }
}`, addr, slowUpstream.URL))
	startInstance(t, old)
	mustFetch(t, addr, "/", "old")

	slow := make(chan string, 1)
	go func() {
		code, body, err := fetch(addr, "/slow")
		slow <- fmt.Sprintf("%d %s %v", code, body, err)
	}()
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("slow request never reached the upstream")
	}

	next := buildInstance(t, returnConf("new", addr))
	errc, err := next.StartReplacing(old)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownWithin(next, time.Second)
	drained := make(chan struct{})
	go func() {
		shutdownWithin(old, 5*time.Second)
		close(drained)
	}()

	for n := 0; n < 5; n++ {
		mustFetch(t, addr, "/", "new")
	}
	select {
	case <-drained:
		t.Fatal("old instance stopped before its in-flight request finished")
	default:
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case got := <-slow:
		if got != "200 slow done <nil>" {
			t.Fatalf("in-flight request across reload = %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("old instance did not finish draining")
	}
	mustFetch(t, addr, "/", "new")
	assertNoServeError(t, errc)
}

// Regression: a reload whose new address failed to bind reported success,
// then the bind error stopped the whole process, including unchanged ports.
func TestReloadBindFailureKeepsRunningInstance(t *testing.T) {
	addr := freeAddr(t)
	spare := freeAddr(t)
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	old := buildInstance(t, returnConf("old", addr))
	oldErrc := startInstance(t, old)
	defer shutdownWithin(old, time.Second)

	failed := buildInstance(t, returnConf("new", addr, spare, busy.Addr().String()))
	if _, err := failed.StartReplacing(old); err == nil || !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("StartReplacing with an occupied port: err = %v", err)
	}
	shutdownWithin(failed, time.Second) // what the runner does with a rejected instance

	mustFetch(t, addr, "/", "old")
	if !old.Ready() {
		t.Fatal("running instance lost readiness after a failed reload")
	}
	assertNoServeError(t, oldErrc)
	if ln, err := net.Listen("tcp4", spare); err != nil {
		t.Fatalf("failed reload left %s bound: %v", spare, err)
	} else {
		ln.Close()
	}

	// The running instance still owns its socket, so a corrected reload works.
	next := buildInstance(t, returnConf("new", addr))
	errc, err := next.StartReplacing(old)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownWithin(next, time.Second)
	mustFetch(t, addr, "/", "new")
	assertNoServeError(t, errc)
}

func TestReloadClosesRemovedListener(t *testing.T) {
	kept, removed := freeAddr(t), freeAddr(t)
	old := buildInstance(t, returnConf("old", kept, removed))
	startInstance(t, old)
	mustFetch(t, removed, "/", "old")

	next := buildInstance(t, returnConf("new", kept))
	errc, err := next.StartReplacing(old)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownWithin(next, time.Second)
	shutdownWithin(old, time.Second)

	mustFetch(t, kept, "/", "new")
	if c, err := net.DialTimeout("tcp4", removed, time.Second); err == nil {
		c.Close()
		t.Fatalf("%s is still accepting after it was removed from the config", removed)
	}
	assertNoServeError(t, errc)
}

func TestReloadDuplicateListenRejected(t *testing.T) {
	addr := freeAddr(t)
	inst := buildInstance(t, fmt.Sprintf(`
http { access_log off; error_log stderr crit;
    server { listen %s; location / { return 200 "http"; } } }
stream { server { listen %s; proxy_pass 127.0.0.1:9; } }`, addr, addr))
	errc := inst.Start()
	defer shutdownWithin(inst, time.Second)
	select {
	case err := <-errc:
		if !strings.Contains(err.Error(), addr) {
			t.Fatalf("duplicate listen error = %v", err)
		}
	default:
		t.Fatal("an http and a stream server on one address both started")
	}
	if ln, err := net.Listen("tcp4", addr); err != nil {
		t.Fatalf("rejected start left %s bound: %v", addr, err)
	} else {
		ln.Close()
	}
}

func prefixTCPBackend(t *testing.T, prefix string) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
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
				defer c.Close()
				buf := make([]byte, 256)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write([]byte(prefix + string(buf[:n]))); err != nil {
						return
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func prefixUDPBackend(t *testing.T, prefix string) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 256)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo([]byte(prefix+string(buf[:n])), from)
		}
	}()
	return pc.LocalAddr().String()
}

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	return addr
}

func roundTrip(t *testing.T, c net.Conn, msg, want string) {
	t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 256)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("waiting for %q: %v", want, err)
	}
	if got := string(buf[:n]); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestReloadHandsOffStreamSockets(t *testing.T) {
	tcpAddr, udpAddr := freeAddr(t), freeUDPAddr(t)
	streamConf := func(tcpBackend, udpBackend string) string {
		return fmt.Sprintf(`
stream {
    server { listen %s; proxy_pass %s; proxy_timeout 2s; }
    server { listen %s udp; proxy_pass %s; proxy_timeout 2s; }
}`, tcpAddr, tcpBackend, udpAddr, udpBackend) // short timeout: UDP sessions hold ephemeral ports until it expires
	}
	old := buildInstance(t, streamConf(prefixTCPBackend(t, "1:"), prefixUDPBackend(t, "1:")))
	startInstance(t, old)

	established, err := net.DialTimeout("tcp4", tcpAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer established.Close()
	roundTrip(t, established, "a", "1:a")
	udpClient, err := net.Dial("udp4", udpAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer udpClient.Close()
	roundTrip(t, udpClient, "x", "1:x")

	next := buildInstance(t, streamConf(prefixTCPBackend(t, "2:"), prefixUDPBackend(t, "2:")))
	errc, err := next.StartReplacing(old)
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownWithin(next, time.Second)
	shutdownWithin(old, time.Second)

	// A proxied TCP connection survives the reload on its original upstream.
	roundTrip(t, established, "b", "1:b")
	fresh, err := net.DialTimeout("tcp4", tcpAddr, time.Second)
	if err != nil {
		t.Fatalf("TCP stream port refused after reload: %v", err)
	}
	defer fresh.Close()
	roundTrip(t, fresh, "c", "2:c")
	// The same UDP socket keeps receiving; new datagrams use the new config.
	roundTrip(t, udpClient, "y", "2:y")
	assertNoServeError(t, errc)
}

func TestAddrKeyCanonicalizesSpellings(t *testing.T) {
	cases := []struct {
		network, a, b string
		same          bool
	}{
		{"tcp4", ":80", "0.0.0.0:80", true},
		{"tcp4", "*:80", "0.0.0.0:80", true},
		{"tcp6", "[::0]:80", "[::]:80", true},
		{"udp4", ":53", "0.0.0.0:53", true},
		{"tcp4", "127.0.0.1:80", "0.0.0.0:80", false},
		{"tcp4", ":80", ":81", false},
		{"tcp4", "localhost:80", "127.0.0.1:80", false}, // hostnames compare as written
	}
	for _, c := range cases {
		if got := addrKey(c.network, c.a) == addrKey(c.network, c.b); got != c.same {
			t.Errorf("addrKey(%s, %q) == addrKey(%s, %q) is %v, want %v", c.network, c.a, c.network, c.b, got, c.same)
		}
	}
	if addrKey("tcp4", ":80") == addrKey("udp4", ":80") {
		t.Error("TCP and UDP on one port must be distinct sockets")
	}
}

func buildReplacing(t *testing.T, conf string, prev *Instance) *Instance {
	t.Helper()
	ds, err := config.Parse(conf)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(ds)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := NewReplacing(cfg, Options{}, prev)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func serveLocal(inst *Instance, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	inst.Addrs[0].ServeHTTP(w, httptest.NewRequest("GET", "http://localhost"+path, nil))
	return w
}

type zoneSite struct {
	listen, extraListen, cacheDir, reqKey, rate, upstream string
}

func (z zoneSite) conf() string {
	extra := ""
	if z.extraListen != "" {
		extra = fmt.Sprintf(` server { listen %s; location / { return 200 "x"; } }`, z.extraListen)
	}
	return fmt.Sprintf(`http {
    access_log off;
    error_log stderr crit;
    proxy_cache_path %q keys_zone=c:1m;
    limit_req_zone %s zone=one:1m rate=%s;
    limit_conn_zone $remote_addr zone=conns:1m;
    server {
        listen %s;
        location /cached { proxy_cache c; proxy_pass %s; }
        location /limited { limit_req zone=one; return 200 "ok"; }
        location /slow { limit_conn conns 1; proxy_pass %s; }
    }%s
}`, filepath.ToSlash(z.cacheDir), z.reqKey, z.rate, z.listen, z.upstream, z.upstream, extra)
}

func cachingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, "body")
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

func cacheFiles(t *testing.T, dir string) int {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "entry-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m)
}

func assertCacheStatus(t *testing.T, inst *Instance, want string) {
	t.Helper()
	if got := serveLocal(inst, "/cached").Header().Get("X-Cache-Status"); got != want {
		t.Fatalf("X-Cache-Status = %q, want %q", got, want)
	}
}

func assertLimited(t *testing.T, inst *Instance, wantCode int) {
	t.Helper()
	if got := serveLocal(inst, "/limited").Code; got != wantCode {
		t.Fatalf("/limited = %d, want %d", got, wantCode)
	}
}

func TestReloadKeepsUnchangedZonesAndResetsChangedOnes(t *testing.T) {
	up, hits := cachingUpstream(t)
	d1, d2 := filepath.Join(t.TempDir(), "c1"), filepath.Join(t.TempDir(), "c2")
	site := zoneSite{listen: freeAddr(t), cacheDir: d1, reqKey: "$remote_addr", rate: "1r/m", upstream: up.URL}

	a := buildInstance(t, site.conf())
	startInstance(t, a)
	assertCacheStatus(t, a, "MISS")
	assertCacheStatus(t, a, "HIT")
	assertLimited(t, a, 200)
	assertLimited(t, a, 503)

	// A rate change keeps the buckets (nginx semantics); the cache dir is unchanged.
	site.rate = "2r/m"
	b := buildReplacing(t, site.conf(), a)
	if _, err := b.StartReplacing(a); err != nil {
		t.Fatal(err)
	}
	shutdownWithin(a, time.Second)
	assertCacheStatus(t, b, "HIT")
	assertLimited(t, b, 503)
	if n := cacheFiles(t, d1); n != 1 {
		t.Fatalf("replaced instance removed the kept cache: %d files", n)
	}

	// A changed key or directory starts that zone empty.
	site.reqKey, site.cacheDir = "$uri", d2
	c := buildReplacing(t, site.conf(), b)
	if _, err := c.StartReplacing(b); err != nil {
		t.Fatal(err)
	}
	defer shutdownWithin(c, time.Second)
	shutdownWithin(b, time.Second)
	assertLimited(t, c, 200)
	assertCacheStatus(t, c, "MISS")
	if n := cacheFiles(t, d1); n != 0 {
		t.Errorf("cache zone moved to a new directory left %d files in the old one", n)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("upstream fetched %d times, want 2 (once per cache directory)", n)
	}
}

func TestRejectedReloadKeepsCache(t *testing.T) {
	up, hits := cachingUpstream(t)
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	dir := filepath.Join(t.TempDir(), "cache")
	site := zoneSite{listen: freeAddr(t), cacheDir: dir, reqKey: "$remote_addr", rate: "1r/m", upstream: up.URL}
	old := buildInstance(t, site.conf())
	startInstance(t, old)
	defer shutdownWithin(old, time.Second)
	assertCacheStatus(t, old, "MISS")

	site.extraListen = busy.Addr().String()
	rejected := buildReplacing(t, site.conf(), old)
	if _, err := rejected.StartReplacing(old); err == nil {
		t.Fatal("StartReplacing onto an occupied port succeeded")
	}
	shutdownWithin(rejected, time.Second) // what the runner does with it

	assertCacheStatus(t, old, "HIT")
	if n := cacheFiles(t, dir); n != 1 {
		t.Fatalf("rejected reload deleted the running cache: %d files", n)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("upstream fetched %d times, want 1", n)
	}
}

func TestReloadLimitConnCountsDrainingRequests(t *testing.T) {
	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case arrived <- struct{}{}:
			<-release // only the first request blocks
		default:
		}
		io.WriteString(w, "slow")
	}))
	defer up.Close()
	defer once.Do(func() { close(release) })

	site := zoneSite{listen: freeAddr(t), cacheDir: filepath.Join(t.TempDir(), "c"), reqKey: "$remote_addr", rate: "1r/m", upstream: up.URL}
	old := buildInstance(t, site.conf())
	startInstance(t, old)
	done := make(chan int, 1)
	go func() { done <- serveLocal(old, "/slow").Code }()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("slow request never reached the upstream")
	}

	next := buildReplacing(t, site.conf(), old)
	if _, err := next.StartReplacing(old); err != nil {
		t.Fatal(err)
	}
	defer shutdownWithin(next, time.Second)
	if got := serveLocal(next, "/slow").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("limit_conn 1 allowed a second request during the drain: %d", got)
	}
	once.Do(func() { close(release) })
	if got := <-done; got != http.StatusOK {
		t.Fatalf("draining request = %d", got)
	}
	shutdownWithin(old, time.Second)
	if got := serveLocal(next, "/slow").Code; got != http.StatusOK {
		t.Fatalf("slot not released after the drained request finished: %d", got)
	}
}

func TestStartReplacingRejectsOtherPredecessor(t *testing.T) {
	a := buildInstance(t, returnConf("a", freeAddr(t)))
	b := buildInstance(t, returnConf("b", freeAddr(t)))
	next := buildReplacing(t, returnConf("n", freeAddr(t)), a)
	if _, err := next.StartReplacing(b); err == nil {
		t.Fatal("instance built for a started in place of b")
	}
	if _, err := next.StartReplacing(nil); err == nil {
		t.Fatal("instance built for a started without it")
	}
}

// Regression: a replaced instance left its keep-alive upstream connections
// open until IdleConnTimeout (90s), so frequent reloads piled them up.
func TestShutdownClosesIdleUpstreamConnections(t *testing.T) {
	var open atomic.Int32
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	up.Config.ConnState = func(c net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed, http.StateHijacked:
			open.Add(-1)
		}
	}
	up.Start()
	defer up.Close()

	inst := buildInstance(t, fmt.Sprintf(`http { access_log off; error_log stderr crit;
    upstream pool { server %s; }
    server { listen 8080;
        location /direct { proxy_pass %s; }
        location /pooled { proxy_pass http://pool; }
        location /unbuffered { proxy_buffering off; proxy_pass %s; } } }`,
		strings.TrimPrefix(up.URL, "http://"), up.URL, up.URL))
	for _, p := range []string{"/direct", "/pooled", "/unbuffered"} {
		if w := serveLocal(inst, p); w.Code != http.StatusOK {
			t.Fatalf("%s = %d", p, w.Code)
		}
	}
	if open.Load() < 3 {
		t.Fatalf("expected an idle keep-alive connection per proxy, upstream sees %d", open.Load())
	}
	shutdownWithin(inst, time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for open.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := open.Load(); n != 0 {
		t.Fatalf("%d upstream connections still open after Shutdown", n)
	}
}
