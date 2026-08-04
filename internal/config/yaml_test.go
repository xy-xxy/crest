package config

import (
	"strings"
	"testing"
	"time"
)

func TestYAMLBasic(t *testing.T) {
	src := `
http:
  gzip:
    enabled: true
    min_length: 1k
  upstreams:
    backend:
      balancer: least_conn
      servers:
        - address: 127.0.0.1:9001
          weight: 3
          max_fails: 2
          fail_timeout: 30s
        - address: 127.0.0.1:9002
          backup: true
  limit_req_zones:
    perip:
      key: $binary_remote_addr
      rate: 5r/s
  servers:
    - listen: [8080, {address: 443, ssl: true, default: true}]
      server_name: [example.com, "*.example.org"]
      root: /var/www
      ssl_certificate: /etc/cert.pem
      ssl_certificate_key: /etc/key.pem
      error_pages:
        - codes: [502, 503]
          uri: /50x.html
      locations:
        - path: /
          try_files: [$uri, /index.html]
        - path: /ping
          match: exact
          return: {code: 200, body: pong}
        - path: /api/
          proxy_pass: http://backend
          proxy_set_header:
            Host: $host
          limit_req: {zone: perip, burst: 10, nodelay: true}
        - path: "\\.(jpg|png)$"
          match: regex_i
          root: /var/img
`
	cfg, err := LoadYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.HTTP
	if h.Gzip.On == nil || !*h.Gzip.On || *h.Gzip.MinLength != 1024 {
		t.Errorf("gzip wrong: %+v", h.Gzip)
	}

	up := h.Upstreams["backend"]
	if up == nil || up.Balancer != "least_conn" || len(up.Servers) != 2 {
		t.Fatalf("upstream wrong: %+v", up)
	}
	if up.Servers[0].Weight != 3 || up.Servers[0].MaxFails != 2 || up.Servers[0].FailTimeout != 30*time.Second {
		t.Errorf("upstream server 0 wrong: %+v", up.Servers[0])
	}
	if !up.Servers[1].Backup {
		t.Errorf("upstream server 1 should be backup")
	}

	z := h.LimitReqZones["perip"]
	if z.Key != "$binary_remote_addr" || z.Rate != 5 {
		t.Errorf("limit_req_zone wrong: %+v", z)
	}

	s := h.Servers[0]
	if len(s.Listens) != 2 || s.Listens[0].Addr != ":8080" {
		t.Errorf("listen[0] wrong: %+v", s.Listens)
	}
	if s.Listens[1].Addr != ":443" || !s.Listens[1].SSL || !s.Listens[1].Default {
		t.Errorf("listen[1] ssl/default wrong: %+v", s.Listens[1])
	}
	if s.SSLCert != "/etc/cert.pem" || len(s.ServerNames) != 2 {
		t.Errorf("server fields wrong: %+v", s)
	}
	if len(s.ErrorPages) != 1 || s.ErrorPages[0].URI != "/50x.html" {
		t.Errorf("error_pages wrong: %+v", s.ErrorPages)
	}
	if len(s.Locations) != 4 {
		t.Fatalf("want 4 locations, got %d", len(s.Locations))
	}
	if s.Locations[0].TryFiles[1] != "/index.html" {
		t.Errorf("try_files wrong: %+v", s.Locations[0])
	}
	ping := s.Locations[1]
	if ping.Modifier != "=" || ping.ReturnCode != 200 || ping.ReturnArg != "pong" {
		t.Errorf("return location wrong: %+v", ping)
	}
	api := s.Locations[2]
	if api.ProxyPass != "http://backend" || api.LimitReq == nil || api.LimitReq.Burst != 10 || !api.LimitReq.NoDelay {
		t.Errorf("proxy/limit location wrong: %+v", api)
	}
	if len(api.ProxyHeaders) != 1 || api.ProxyHeaders[0].Name != "Host" {
		t.Errorf("proxy_set_header wrong: %+v", api.ProxyHeaders)
	}
	img := s.Locations[3]
	if img.Modifier != "~*" || img.Root != "/var/img" {
		t.Errorf("regex_i location wrong: %+v", img)
	}
}

func TestYAMLUnknownFieldRejected(t *testing.T) {
	src := `
http:
  servers:
    - listen: [8080]
      typo_field: oops
`
	if _, err := LoadYAML([]byte(src)); err == nil {
		t.Error("unknown YAML field should be rejected")
	}
}

func TestYAMLStream(t *testing.T) {
	src := `
stream:
  servers:
    - listen: 19100
      udp: true
      proxy_pass: 192.168.3.100:19100
      proxy_timeout: 10s
      proxy_responses: 1
`
	cfg, err := LoadYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Stream == nil || len(cfg.Stream.Servers) != 1 {
		t.Fatal("stream not parsed")
	}
	ss := cfg.Stream.Servers[0]
	if !ss.UDP || ss.Addr != ":19100" || ss.ProxyResponses != 1 || ss.ProxyTimeout != 10*time.Second {
		t.Errorf("stream server wrong: %+v", ss)
	}
}

func TestYAMLAccessControlOrder(t *testing.T) {
	src := `
http:
  servers:
    - listen: [8080]
      locations:
        - path: /
          access:
            - allow: 127.0.0.1
            - allow: 10.0.0.0/8
            - deny: all
`
	cfg, err := LoadYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	acc := cfg.HTTP.Servers[0].Locations[0].Access
	if len(acc) != 3 || !acc[0].Allow || acc[0].Cidr != "127.0.0.1" || acc[2].Allow || acc[2].Cidr != "all" {
		t.Errorf("access rules order wrong: %+v", acc)
	}
}

func TestYAMLListenForms(t *testing.T) {
	src := `
http:
  servers:
    - listen: [80, "443 ssl", "8080 ssl default", {port: 9000, ssl: true}]
      locations:
        - path: /
          return: {code: 200, body: ok}
`
	cfg, err := LoadYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	ls := cfg.HTTP.Servers[0].Listens
	if len(ls) != 4 {
		t.Fatalf("want 4 listens, got %d", len(ls))
	}
	if ls[0].Addr != ":80" || ls[0].SSL {
		t.Errorf("listen[0] wrong: %+v", ls[0])
	}
	if ls[1].Addr != ":443" || !ls[1].SSL {
		t.Errorf("listen[1] '443 ssl' wrong: %+v", ls[1])
	}
	if ls[2].Addr != ":8080" || !ls[2].SSL || !ls[2].Default {
		t.Errorf("listen[2] '8080 ssl default' wrong: %+v", ls[2])
	}
	if ls[3].Addr != ":9000" || !ls[3].SSL {
		t.Errorf("listen[3] mapping wrong: %+v", ls[3])
	}
}

func TestYAMLListenMissingAddress(t *testing.T) {
	src := `
http:
  servers:
    - listen: [{ssl: true}]
      locations:
        - path: /
          return: {code: 200, body: ok}
`
	_, err := LoadYAML([]byte(src))
	if err == nil {
		t.Fatal("mapping listen without port should error")
	}
	if !strings.Contains(err.Error(), "needs a port") {
		t.Errorf("error should guide the fix, got: %v", err)
	}
}

func TestYAMLListenAddressStillWorks(t *testing.T) {
	// 旧写法 {address: ...} 仍兼容
	src := `
http:
  servers:
    - listen: [{address: 8443, ssl: true}]
      locations:
        - path: /
          return: {code: 200, body: ok}
`
	cfg, err := LoadYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.HTTP.Servers[0].Listens[0]
	if l.Addr != ":8443" || !l.SSL {
		t.Errorf("legacy address form broken: %+v", l)
	}
}

func TestYAMLProxySendTimeout(t *testing.T) {
	src := `
http:
  proxy_send_timeout: 45s
  upstreams:
    pool:
      keepalive: 512
      servers:
        - address: 127.0.0.1:9090
  servers:
    - listen: [8080]
      locations:
        - path: /api/
          proxy_pass: http://pool
          proxy_send_timeout: 90s
`
	cfg, err := LoadYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.ProxySendTimeout == nil || *cfg.HTTP.ProxySendTimeout != 45*time.Second {
		t.Errorf("http proxy_send_timeout: %v", cfg.HTTP.ProxySendTimeout)
	}
	loc := cfg.HTTP.Servers[0].Locations[0]
	if loc.ProxySendTimeout == nil || *loc.ProxySendTimeout != 90*time.Second {
		t.Errorf("location proxy_send_timeout: %v", loc.ProxySendTimeout)
	}
	if cfg.HTTP.Upstreams["pool"].Keepalive != 512 {
		t.Errorf("upstream keepalive: %d", cfg.HTTP.Upstreams["pool"].Keepalive)
	}
}
