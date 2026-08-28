package server

import (
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noRedirectClient(ts *httptest.Server) *http.Client {
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return c
}

func TestRewrite(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "files"), 0755)
	mustWrite(t, filepath.Join(dir, "files", "a.txt"), "file A")

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        root "%s";
        rewrite ^/legacy/(.*)$ /files/$1 last;
        location /old/ {
            rewrite ^/old/(.*)$ https://new.example/$1 permanent;
        }
        location /drop/ {
            rewrite ^ /files/a.txt?keep=1? redirect;
        }
        location /files/ {
        }
    }
}
`, filepath.ToSlash(dir))
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()
	client := noRedirectClient(ts)

	// server-level rewrite with capture, flag last → internal
	resp, err := client.Get(ts.URL + "/legacy/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "file A" {
		t.Errorf("rewrite last: %d %q", resp.StatusCode, body)
	}

	// permanent → 301 with expanded target
	resp, err = client.Get(ts.URL + "/old/page.html")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://new.example/page.html" {
		t.Errorf("rewrite permanent: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// redirect flag with trailing "?" drops old args, keeps new ones
	resp, err = client.Get(ts.URL + "/drop/x?orig=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Fatalf("rewrite redirect: %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/files/a.txt?keep=1" {
		t.Errorf("trailing ? should drop original args: %q", loc)
	}
}

func TestTryFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.html"), "SPA shell")
	mustWrite(t, filepath.Join(dir, "app.js"), "console.log(1)")

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        root "%s";
        location / {
            try_files $uri /index.html;
        }
        location /strict/ {
            try_files $uri =404;
        }
    }
}
`, filepath.ToSlash(dir))
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	if code, body := get(t, ts, "/app.js", ""); code != 200 || body != "console.log(1)" {
		t.Errorf("existing file: %d %q", code, body)
	}
	if code, body := get(t, ts, "/no/such/route", ""); code != 200 || body != "SPA shell" {
		t.Errorf("SPA fallback: %d %q", code, body)
	}
	if code, _ := get(t, ts, "/strict/missing", ""); code != 404 {
		t.Errorf("=404 fallback: %d", code)
	}
}

func TestGzip(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("gonx compresses text. ", 500)
	mustWrite(t, filepath.Join(dir, "big.html"), big)
	mustWrite(t, filepath.Join(dir, "tiny.html"), "hi")

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    gzip on;
    gzip_min_length 100;
    server {
        listen 8080;
        root "%s";
    }
}
`, filepath.ToSlash(dir))
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/big.html", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	tr := &http.Transport{DisableCompression: true} // manual control of the encoding
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("big file should be gzipped, headers: %v", resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := io.ReadAll(zr)
	if string(dec) != big {
		t.Errorf("gzip roundtrip corrupted body (%d bytes)", len(dec))
	}

	req2, _ := http.NewRequest("GET", ts.URL+"/tiny.html", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	resp2, err := tr.RoundTrip(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.Header.Get("Content-Encoding") == "gzip" {
		t.Error("file below gzip_min_length must not be compressed")
	}
}

func TestTLSWithSNI(t *testing.T) {
	dir := t.TempDir()
	certA, keyA := selfSigned(t, dir, "a", "a.test")
	certB, keyB := selfSigned(t, dir, "b", "b.test")
	mustWrite(t, filepath.Join(dir, "index.html"), "unused")

	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 443 ssl default_server;
        server_name a.test;
        ssl_certificate "%s";
        ssl_certificate_key "%s";
        location / { return 200 "A over $scheme"; }
    }
    server {
        listen 443 ssl;
        server_name b.test;
        ssl_certificate "%s";
        ssl_certificate_key "%s";
        location / { return 200 "B over $scheme"; }
    }
}
`, filepath.ToSlash(certA), filepath.ToSlash(keyA), filepath.ToSlash(certB), filepath.ToSlash(keyB))

	inst := buildInstance(t, conf)
	a := inst.Addrs[0]
	if a.tlsConf == nil {
		t.Fatal("tls config missing")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: a, TLSConfig: a.tlsConf}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()

	for _, tc := range []struct{ sni, want string }{
		{"a.test", "A over https"},
		{"b.test", "B over https"},
		{"other.test", "A over https"}, // falls back to default server cert+vhost
	} {
		client := &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: tc.sni},
		}}
		req, _ := http.NewRequest("GET", "https://"+ln.Addr().String()+"/", nil)
		req.Host = tc.sni
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.sni, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != tc.want {
			t.Errorf("SNI %s: got %q want %q", tc.sni, body, tc.want)
		}
		// verify the served certificate matches the SNI
		if tc.sni != "other.test" {
			cn := resp.TLS.PeerCertificates[0].DNSNames
			if len(cn) == 0 || cn[0] != tc.sni {
				t.Errorf("SNI %s: served cert for %v", tc.sni, cn)
			}
		}
	}
}

func selfSigned(t *testing.T, dir, name, host string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, name+".crt")
	keyPath = filepath.Join(dir, name+".key")
	cf, _ := os.Create(certPath)
	pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()
	kb, _ := x509.MarshalECPrivateKey(key)
	kf, _ := os.Create(keyPath)
	pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	kf.Close()
	return certPath, keyPath
}

func TestTLSMinVersion(t *testing.T) {
	dir := t.TempDir()
	crt, key := selfSigned(t, dir, "m", "localhost")
	conf := fmt.Sprintf(`
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 443 ssl;
        server_name localhost;
        ssl_certificate "%s";
        ssl_certificate_key "%s";
        location / { return 200 "ok"; }
    }
}
`, filepath.ToSlash(crt), filepath.ToSlash(key))
	inst := buildInstance(t, conf)
	a := inst.Addrs[0]
	if a.tlsConf == nil || a.tlsConf.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLS MinVersion should be 1.2, got %#x", a.tlsConf.MinVersion)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: a, TLSConfig: a.tlsConf}
	go srv.ServeTLS(ln, "", "")
	defer srv.Close()

	dial := func(max uint16) error {
		c, err := tls.Dial("tcp", ln.Addr().String(),
			&tls.Config{InsecureSkipVerify: true, MinVersion: max, MaxVersion: max})
		if err == nil {
			c.Close()
		}
		return err
	}
	if dial(tls.VersionTLS11) == nil {
		t.Error("TLS 1.1 should be rejected")
	}
	if err := dial(tls.VersionTLS12); err != nil {
		t.Errorf("TLS 1.2 should be accepted: %v", err)
	}
}

// TestRewriteExternalRedirectArgs pins nginx's argument handling on an
// absolute replacement: the previous query string is appended unless the
// replacement ends in "?". Only local replacements used to get this, so an
// external redirect silently dropped the client's query.
func TestRewriteExternalRedirectArgs(t *testing.T) {
	conf := `
http {
    access_log off;
    error_log stderr crit;
    server {
        listen 8080;
        location /plain/ { rewrite ^/plain/(.*)$ http://ex.com/$1 redirect; }
        location /withq/ { rewrite ^/withq/(.*)$ http://ex.com/$1?fixed=1 redirect; }
        location /dropq/ { rewrite ^/dropq/(.*)$ http://ex.com/$1? redirect; }
    }
}`
	inst := buildInstance(t, conf)
	ts := httptest.NewServer(inst.Addrs[0])
	defer ts.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	tests := []struct{ path, want string }{
		{"/plain/a/b?x=1&y=2", "http://ex.com/a/b?x=1&y=2"},
		{"/plain/a/b", "http://ex.com/a/b"},
		{"/withq/a?x=1", "http://ex.com/a?fixed=1&x=1"},
		{"/dropq/a?x=1", "http://ex.com/a"},
	}
	for _, tc := range tests {
		resp, err := client.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Location"); got != tc.want {
			t.Errorf("%s: Location = %q, want %q", tc.path, got, tc.want)
		}
	}
}
