package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"crest/internal/server"
)

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func fetchFresh(addr, path string) (string, error) {
	c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get("http://" + addr + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, b), err
}

// startRunner wires a runner the way runServer does, without signals or the
// control channel.
func startRunner(t *testing.T, conf string) *runner {
	t.Helper()
	cfg, err := loadConfig(conf)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := server.NewWithOptions(cfg, server.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ru := &runner{confPath: conf, inst: inst, console: io.Discard, ui: cfg.UI, errs: make(chan error, 8), quit: make(chan struct{})}
	errc := inst.Start()
	select {
	case err := <-errc:
		t.Fatal(err)
	default:
	}
	ru.watch(errc)
	t.Cleanup(func() {
		ru.requestStop()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ru.shutdown(ctx)
	})
	return ru
}

func writeConf(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func expectNoFatal(t *testing.T, ru *runner, wait time.Duration) {
	t.Helper()
	select {
	case err := <-ru.errs:
		t.Fatalf("the process would exit: %v", err)
	case <-time.After(wait):
	}
}

// Regression: adding an occupied port reported "reloaded", then the bind
// error stopped the process and took every port down with it.
func TestRunnerReloadBindFailureKeepsServing(t *testing.T) {
	addr := freeTCPAddr(t)
	busy, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	conf := filepath.Join(t.TempDir(), "crest.conf")
	site := func(listen, body string) string {
		return fmt.Sprintf(" server { listen %s; location / { return 200 %q; } }\n", listen, body)
	}
	writeConf(t, conf, "http { access_log off; error_log stderr crit;\n"+site(addr, "old")+"}")
	ru := startRunner(t, conf)

	writeConf(t, conf, "http { access_log off; error_log stderr crit;\n"+site(addr, "new")+site(busy.Addr().String(), "new")+"}")
	if _, err := ru.reload(); err == nil || !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("reload onto an occupied port: err = %v", err)
	}
	expectNoFatal(t, ru, 300*time.Millisecond)
	if got, err := fetchFresh(addr, "/"); got != "200 old" {
		t.Fatalf("after rejected reload: %q, %v", got, err)
	}

	writeConf(t, conf, "http { access_log off; error_log stderr crit;\n"+site(addr, "new")+"}")
	if _, err := ru.reload(); err != nil {
		t.Fatal(err)
	}
	if got, err := fetchFresh(addr, "/"); got != "200 new" {
		t.Fatalf("after corrected reload: %q, %v", got, err)
	}
	expectNoFatal(t, ru, 100*time.Millisecond)
}

// Regression: reload waited for in-flight requests with the port closed, so
// new connections were refused for up to 5s.
func TestRunnerReloadDoesNotWaitForInFlightRequests(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		<-release
		io.WriteString(w, "slow")
	}))
	defer slow.Close()
	defer releaseOnce.Do(func() { close(release) })

	addr := freeTCPAddr(t)
	conf := filepath.Join(t.TempDir(), "crest.conf")
	site := func(body string) string {
		return fmt.Sprintf(`http { access_log off; error_log stderr crit;
    server { listen %s; location /slow { proxy_pass %s; } location / { return 200 %q; } } }`, addr, slow.URL, body)
	}
	writeConf(t, conf, site("old"))
	ru := startRunner(t, conf)

	inFlight := make(chan string, 1)
	go func() {
		got, err := fetchFresh(addr, "/slow")
		inFlight <- fmt.Sprintf("%s %v", got, err)
	}()
	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("slow request never reached the upstream")
	}

	writeConf(t, conf, site("new"))
	reloaded := make(chan error, 1)
	go func() {
		_, err := ru.reload()
		reloaded <- err
	}()
	select {
	case err := <-reloaded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reload blocked on an in-flight request")
	}
	if got, err := fetchFresh(addr, "/"); got != "200 new" {
		t.Fatalf("new connection during drain: %q, %v", got, err)
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case got := <-inFlight:
		if got != "200 slow <nil>" {
			t.Fatalf("in-flight request across reload: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	expectNoFatal(t, ru, 100*time.Millisecond)
}

// Regression: every reload started proxy_cache, limit_req and limit_conn
// from scratch. nginx keeps unchanged zones across a reload, and so does crest.
func TestRunnerReloadKeepsZoneState(t *testing.T) {
	var upstreamHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Cache-Control", "max-age=60")
		io.WriteString(w, "cached body")
	}))
	defer backend.Close()

	addr := freeTCPAddr(t)
	dir := t.TempDir()
	conf := filepath.Join(dir, "crest.conf")
	site := func(version string) string {
		return fmt.Sprintf(`http {
    access_log off;
    error_log stderr crit;
    proxy_cache_path %q keys_zone=c:1m;
    limit_req_zone $remote_addr zone=one:1m rate=1r/m;
    server {
        listen %s;
        location /cached { proxy_cache c; proxy_pass %s; }
        location /limited { limit_req zone=one; return 200 "ok"; }
        location / { return 200 %q; }
    }
}`, filepath.ToSlash(filepath.Join(dir, "cache")), addr, backend.URL, version)
	}
	cacheStatus := func() string {
		c := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		resp, err := c.Get("http://" + addr + "/cached")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.Header.Get("X-Cache-Status")
	}
	writeConf(t, conf, site("v1"))
	ru := startRunner(t, conf)

	if got := cacheStatus(); got != "MISS" {
		t.Fatalf("first request: X-Cache-Status = %q", got)
	}
	if got := cacheStatus(); got != "HIT" {
		t.Fatalf("second request: X-Cache-Status = %q", got)
	}
	if got, _ := fetchFresh(addr, "/limited"); got != "200 ok" {
		t.Fatalf("first limited request: %q", got)
	}
	if got, _ := fetchFresh(addr, "/limited"); !strings.HasPrefix(got, "503") {
		t.Fatalf("second limited request within 1r/m: %q", got)
	}

	writeConf(t, conf, site("v2"))
	if _, err := ru.reload(); err != nil {
		t.Fatal(err)
	}
	if got, _ := fetchFresh(addr, "/"); got != "200 v2" {
		t.Fatalf("reload not applied: %q", got)
	}
	if got := cacheStatus(); got != "HIT" {
		t.Errorf("after reload: X-Cache-Status = %q, want HIT (cache kept)", got)
	}
	if n := upstreamHits.Load(); n != 1 {
		t.Errorf("upstream fetched %d times, want 1", n)
	}
	if got, _ := fetchFresh(addr, "/limited"); !strings.HasPrefix(got, "503") {
		t.Errorf("after reload the limit_req bucket was reset: %q", got)
	}
}
