// Command perf runs bounded local HTTP workloads against a separate crest process.
// RSS comes from ps (macOS/Linux); the load generator and backend are excluded.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type scenario struct {
	Name                           string
	Concurrent                     int
	Size                           int
	Cache, Unique, Static, NoStore bool
	Slow                           bool
	Direct                         bool
	TLS, Gzip                      bool
	Duration                       time.Duration
}
type sample struct {
	Seconds float64
	RSSMiB  float64
}
type result struct {
	Name                                     string
	Concurrent, BodyBytes                    int
	Requests, Errors, BackendHits, CacheHits int64
	Seconds, RPS, P50ms, P95ms, P99ms        float64
	IdleRSSMiB, PeakRSSMiB, AfterRSSMiB      float64
	Samples                                  []sample
	ErrorExamples                            []string
	ConsoleTail                              string
	ClientDials, PeakClientConnections       int64
}
type report struct {
	OS, Arch, Go, Binary, MemoryLimit string
	Ramp                              string
	Procs                             int
	HTTPReady                         bool
	Results                           []result
}

func main() {
	binary := flag.String("binary", "dist/perf/crest", "crest executable")
	output := flag.String("output", "dist/perf/results.json", "JSON report")
	only := flag.String("only", "", "comma-separated scenario names (empty runs all)")
	duration := flag.Duration("duration", 10*time.Second, "admission duration per scenario")
	mem := flag.String("memory-limit", "128MiB", "crest GOMEMLIMIT (soft runtime target)")
	capMiB := flag.Int("rss-stop-mib", 768, "abort a scenario above this crest RSS")
	procs := flag.Int("procs", runtime.NumCPU(), "GOMAXPROCS for the crest process")
	ramp := flag.Duration("ramp", 2*time.Millisecond, "delay between client launches; 0 tests an abrupt connection burst")
	clientMax := flag.Int("client-max-conns", -1, "client connections per host (-1 equals workers; 0 is unbounded)")
	httpReady := flag.Bool("http-ready", false, "wait for a complete HTTP response before starting workload (diagnostic)")
	flag.Parse()
	if err := run(*binary, *output, *only, *duration, *mem, *capMiB, *ramp, *procs, *httpReady, *clientMax); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(binary, output, only string, duration time.Duration, mem string, capMiB int, ramp time.Duration, procs int, httpReady bool, clientMax int) error {
	binary, err := filepath.Abs(binary)
	if err != nil {
		return err
	}
	if duration <= 0 || capMiB <= 0 || ramp < 0 || procs < 1 {
		return fmt.Errorf("duration and rss-stop-mib must be positive")
	}
	scenarios := []scenario{
		{Name: "proxy-32", Concurrent: 32, Size: 1024},
		{Name: "proxy-128", Concurrent: 128, Size: 1024},
		{Name: "proxy-512", Concurrent: 512, Size: 1024},
		{Name: "direct-512", Concurrent: 512, Size: 1024, Direct: true},
		{Name: "tls-128", Concurrent: 128, Size: 1024, TLS: true},
		{Name: "gzip-128", Concurrent: 128, Size: 1024, Gzip: true},
		{Name: "static-256", Concurrent: 256, Size: 1024, Static: true},
		{Name: "cache-hot-256", Concurrent: 256, Size: 1024, Cache: true},
		{Name: "cache-cold-64", Concurrent: 64, Size: 4 << 20, Cache: true, Unique: true},
		{Name: "no-store-64", Concurrent: 64, Size: 4 << 20, Cache: true, NoStore: true},
		{Name: "slow-64", Concurrent: 64, Size: 4 << 20, Slow: true},
		{Name: "soak-128", Concurrent: 128, Size: 1024, Duration: 60 * time.Second},
	}
	var selected []scenario
	for _, s := range scenarios {
		if only == "" || strings.Contains(","+only+",", ","+s.Name+",") {
			selected = append(selected, s)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("no matching scenarios")
	}
	rep := report{OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(), Binary: binary, MemoryLimit: mem, Ramp: ramp.String(), Procs: procs, HTTPReady: httpReady}
	failed := false
	for _, s := range selected {
		if s.Duration == 0 {
			s.Duration = duration
		}
		r, err := measure(binary, s, mem, capMiB, ramp, procs, httpReady, clientMax)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		rep.Results = append(rep.Results, r)
		fmt.Printf("%s: n=%d errors=%d rps=%.0f p99=%.1fms RSS idle/peak/after=%.1f/%.1f/%.1fMiB\n", s.Name, r.Requests, r.Errors, r.RPS, r.P99ms, r.IdleRSSMiB, r.PeakRSSMiB, r.AfterRSSMiB)
		if r.Errors > 0 {
			failed = true
		}
		if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
			return err
		}
		data, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(output, append(data, '\n'), 0644); err != nil {
			return err
		}
	}
	if failed {
		return fmt.Errorf("workload errors; see %s", output)
	}
	return nil
}
func rss(pid int) (float64, error) {
	b, e := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if e != nil {
		return 0, e
	}
	n, e := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return n / 1024, e
}
func measure(binary string, s scenario, mem string, capMiB int, ramp time.Duration, procs int, httpReady bool, clientMax int) (res result, err error) {
	res.Name = s.Name
	res.Concurrent = s.Concurrent
	res.BodyBytes = s.Size
	dir, err := os.MkdirTemp("", "crest-perf-")
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)
	body := bytes.Repeat([]byte("x"), s.Size)
	sum := sha256.Sum256(body)
	expected := hex.EncodeToString(sum[:])
	var backendHits atomic.Int64
	backendHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHits.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if s.NoStore {
			w.Header().Set("Cache-Control", "no-store")
		}
		if s.Cache {
			w.Header().Set("Vary", "Accept-Language")
		}
		w.Write(body)
	})
	var backend *httptest.Server
	if s.TLS {
		backend = httptest.NewTLSServer(backendHandler)
	} else {
		backend = httptest.NewServer(backendHandler)
	}
	defer backend.Close()
	ln, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return res, e
	}
	addr := ln.Addr().String()
	ln.Close()
	route := fmt.Sprintf("proxy_pass %s;", backend.URL)
	if s.Static {
		if e = os.WriteFile(filepath.Join(dir, "data"), body, 0644); e != nil {
			return res, e
		}
		route = fmt.Sprintf("root %q;", dir)
	}
	if s.Cache {
		route += " proxy_cache test; proxy_cache_valid 200 1m;"
	}

	listen := addr
	sslConfig := ""
	if s.TLS {
		cert := backend.TLS.Certificates[0]
		key, e := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
		if e != nil {
			return res, e
		}
		certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
		if e = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0600); e != nil {
			return res, e
		}
		if e = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0600); e != nil {
			return res, e
		}
		listen += " ssl"
		sslConfig = fmt.Sprintf("ssl_certificate %q; ssl_certificate_key %q;", certPath, keyPath)
	}
	gzipConfig := ""
	if s.Gzip {
		gzipConfig = "gzip on; gzip_types application/octet-stream; gzip_min_length 1;"
	}
	conf := fmt.Sprintf(`http { access_log off; error_log stderr error; %s proxy_cache_path %q keys_zone=test:10m; server { listen %s; %s location / { %s } } }`, gzipConfig, filepath.Join(dir, "cache"), listen, sslConfig, route)

	confPath := filepath.Join(dir, "crest.conf")
	if e = os.WriteFile(confPath, []byte(conf), 0644); e != nil {
		return res, e
	}
	log, e := os.Create(filepath.Join(dir, "console.txt"))
	if e != nil {
		return res, e
	}
	defer log.Close()
	// Put the binary beside temporary logs so setupLogging does not touch the workspace.
	exe := filepath.Join(dir, "crest")
	if e = os.Symlink(binary, exe); e != nil {
		return res, e
	}
	cmd := exec.Command(exe, "-c", confPath)
	cmd.Env = append(os.Environ(), "GOMEMLIMIT="+mem, "GOMAXPROCS="+strconv.Itoa(procs))
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		return res, e
	}
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-ended:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-ended
		}
	}()
	url := "http://" + addr + "/data"
	if s.TLS {
		url = "https://" + addr + "/data"
	}
	if s.Direct {
		url = backend.URL + "/data"
	}
	tr := &http.Transport{MaxIdleConns: 2 * s.Concurrent, MaxIdleConnsPerHost: s.Concurrent, DisableCompression: !s.Gzip, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} // local ephemeral test certificate only
	var clientDials, clientOpen, clientPeak atomic.Int64
	if clientMax < 0 {
		clientMax = s.Concurrent
	}
	tr.MaxConnsPerHost = clientMax
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		clientDials.Add(1)
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		n := clientOpen.Add(1)
		for old := clientPeak.Load(); n > old; old = clientPeak.Load() {
			if clientPeak.CompareAndSwap(old, n) {
				break
			}
		}
		return &countedConn{Conn: conn, closed: func() { clientOpen.Add(-1) }}, nil
	}
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	defer tr.CloseIdleConnections()
	for attempt := 0; ; attempt++ {
		if attempt == 100 {
			b, _ := os.ReadFile(filepath.Join(dir, "console.txt"))
			return res, fmt.Errorf("startup timed out: %s", b)
		}
		c, e := net.DialTimeout("tcp4", addr, 50*time.Millisecond)
		if e == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if httpReady {
		probeTransport := tr.Clone()
		probeTransport.DisableKeepAlives = true
		probe := &http.Client{Transport: probeTransport, Timeout: 5 * time.Second}
		resp, e := probe.Get(url)
		if e != nil {
			return res, e
		}
		_, e = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		probeTransport.CloseIdleConnections()
		if e != nil || resp.StatusCode != 200 {
			return res, fmt.Errorf("HTTP readiness: status=%d error=%v", resp.StatusCode, e)
		}
	}
	// Warm every variant only for the hot-cache scenario.
	if s.Cache && !s.Unique && !s.NoStore {
		for i := 0; i < 8; i++ {
			req, _ := http.NewRequest("GET", url, nil)
			req.Header.Set("Accept-Language", strconv.Itoa(i))
			resp, e := client.Do(req)
			if e != nil {
				return res, e
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	if res.IdleRSSMiB, e = rss(cmd.Process.Pid); e != nil {
		return res, e
	}
	res.PeakRSSMiB = res.IdleRSSMiB
	var mu sync.Mutex
	counts := make([]int64, 30001)
	addError := func(msg string) {
		mu.Lock()
		res.Errors++
		if len(res.ErrorExamples) < 5 {
			res.ErrorExamples = append(res.ErrorExamples, msg)
		}
		mu.Unlock()
	}
	start := time.Now()
	deadline := start.Add(s.Duration)
	stopped := make(chan struct{})
	monDone := make(chan struct{})
	go func() {
		defer close(monDone)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-ticker.C:
				n, e := rss(cmd.Process.Pid)
				if e != nil {
					continue
				}
				res.Samples = append(res.Samples, sample{time.Since(start).Seconds(), n})
				if n > res.PeakRSSMiB {
					res.PeakRSSMiB = n
				}
				if n > float64(capMiB) {
					addError("RSS safety stop")
					cmd.Process.Kill()
					return
				}
			}
		}
	}()
	var seq atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < s.Concurrent; w++ {
		if w > 0 {
			time.Sleep(ramp)
		}
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			buf := make([]byte, 32<<10)
			for attempt := 0; time.Now().Before(deadline); attempt++ {
				u := url
				if s.Unique {
					u += "?id=" + strconv.FormatInt(seq.Add(1), 10)
				}
				req, _ := http.NewRequestWithContext(context.Background(), "GET", u, nil)
				req.Header.Set("Accept-Language", strconv.Itoa(id%8))
				t := time.Now()
				resp, e := client.Do(req)
				n := int64(0)
				h := sha256.New()
				if e == nil {
					if resp.StatusCode != 200 {
						e = fmt.Errorf("status %d", resp.StatusCode)
					}
					for {
						nr, er := resp.Body.Read(buf)
						if nr > 0 {
							h.Write(buf[:nr])
							n += int64(nr)
							if s.Slow {
								time.Sleep(time.Duration(nr) * time.Second / (1 << 20))
							}
						}
						if er != nil {
							if er != io.EOF && e == nil {
								e = er
							}
							break
						}
					}
					resp.Body.Close()
					if e == nil && (n != int64(s.Size) || hex.EncodeToString(h.Sum(nil)) != expected) {
						e = fmt.Errorf("body mismatch: %d bytes", n)
					}
				}
				if e != nil {
					addError(fmt.Sprintf("t=%.3fs worker=%d attempt=%d: %v", time.Since(start).Seconds(), id, attempt, e))
				}
				elapsed := time.Since(t).Milliseconds()
				if elapsed > 30000 {
					elapsed = 30000
				}
				mu.Lock()
				res.Requests++
				counts[elapsed]++
				if resp != nil && resp.Header.Get("X-Cache-Status") == "HIT" {
					res.CacheHits++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	res.Seconds = time.Since(start).Seconds()
	close(stopped)
	<-monDone
	res.ClientDials = clientDials.Load()
	res.PeakClientConnections = clientPeak.Load()
	res.BackendHits = backendHits.Load()
	res.RPS = float64(res.Requests) / res.Seconds
	percentile := func(p float64) float64 {
		target := int64(float64(res.Requests) * p)
		if target < 1 {
			target = 1
		}
		var n int64
		for i, c := range counts {
			n += c
			if n >= target {
				return float64(i)
			}
		}
		return 0
	}
	res.P50ms = percentile(.50)
	res.P95ms = percentile(.95)
	res.P99ms = percentile(.99)
	tr.CloseIdleConnections()
	time.Sleep(3 * time.Second)
	res.AfterRSSMiB, _ = rss(cmd.Process.Pid)
	if res.Requests == 0 {
		res.Errors++
		res.ErrorExamples = append(res.ErrorExamples, "no requests completed")
	}
	if res.Errors > 0 {
		b, _ := os.ReadFile(filepath.Join(dir, "console.txt"))
		if len(b) > 4096 {
			b = b[len(b)-4096:]
		}
		res.ConsoleTail = string(b)
	}
	return res, nil
}

// countedConn observes TCP socket count; it does not retry requests or alter I/O.
type countedConn struct {
	net.Conn
	once   sync.Once
	closed func()
}

func (c *countedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.closed); return err }
