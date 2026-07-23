package config

import (
	"strings"
	"testing"
)

const sampleConf = `
worker_processes auto;
events { worker_connections 1024; }

http {
    log_format main '$remote_addr - "$request" ' '$status';
    access_log logs/access.log main;
    error_log logs/error.log warn;

    upstream backend {
        server 127.0.0.1:9001 weight=3;
        server 127.0.0.1:9002;
        server 10.0.0.1;
    }

    server {
        listen 8080 default_server;
        listen 127.0.0.1:9090;
        server_name example.com *.example.org;
        root /var/www;
        index index.html index.htm;

        location / {
        }
        location = /ping {
            return 200 "pong";
        }
        location ~* \.(jpg|png)$ {
            root /var/img;
        }
        location /api/ {
            proxy_pass http://backend;
        }
        location /old/ {
            return 301 http://example.com/new/;
        }
        made_up_directive on;
    }
}
`

func loadSample(t *testing.T) *Config {
	t.Helper()
	ds, err := Parse(sampleConf)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(ds)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestLoadHTTP(t *testing.T) {
	cfg := loadSample(t)
	h := cfg.HTTP
	if h.AccessLog != "logs/access.log" || h.AccessLogFormat != "main" {
		t.Errorf("access_log wrong: %q %q", h.AccessLog, h.AccessLogFormat)
	}
	if h.ErrorLog != "logs/error.log" || h.ErrorLevel != "warn" {
		t.Errorf("error_log wrong: %q %q", h.ErrorLog, h.ErrorLevel)
	}
	// adjacent quoted strings concatenate
	if h.LogFormats["main"] != `$remote_addr - "$request" $status` {
		t.Errorf("log_format wrong: %q", h.LogFormats["main"])
	}
}

func TestLoadUpstream(t *testing.T) {
	up := loadSample(t).HTTP.Upstreams["backend"]
	if up == nil {
		t.Fatal("upstream backend missing")
	}
	if len(up.Servers) != 3 {
		t.Fatalf("want 3 servers, got %d", len(up.Servers))
	}
	if up.Servers[0].Weight != 3 || up.Servers[1].Weight != 1 {
		t.Errorf("weights wrong: %+v", up.Servers)
	}
	if up.Servers[2].Addr != "10.0.0.1:80" {
		t.Errorf("default port not applied: %q", up.Servers[2].Addr)
	}
}

func TestLoadServer(t *testing.T) {
	s := loadSample(t).HTTP.Servers[0]
	if len(s.Listens) != 2 || s.Listens[0].Addr != ":8080" || !s.Listens[0].Default {
		t.Errorf("listen wrong: %+v", s.Listens)
	}
	if s.Listens[1].Addr != "127.0.0.1:9090" {
		t.Errorf("listen addr wrong: %+v", s.Listens[1])
	}
	if len(s.ServerNames) != 2 || s.Root != "/var/www" {
		t.Errorf("server fields wrong: %+v", s)
	}
	if len(s.Locations) != 5 {
		t.Fatalf("want 5 locations, got %d", len(s.Locations))
	}
	ping := s.Locations[1]
	if ping.Modifier != "=" || ping.ReturnCode != 200 || ping.ReturnArg != "pong" {
		t.Errorf("return location wrong: %+v", ping)
	}
	img := s.Locations[2]
	if img.Modifier != "~*" || img.Root != "/var/img" {
		t.Errorf("regex location wrong: %+v", img)
	}
	api := s.Locations[3]
	if api.ProxyPass != "http://backend" {
		t.Errorf("proxy_pass wrong: %+v", api)
	}
	old := s.Locations[4]
	if old.ReturnCode != 301 || old.ReturnArg != "http://example.com/new/" {
		t.Errorf("redirect wrong: %+v", old)
	}
}

func TestLoadWarnsUnknown(t *testing.T) {
	cfg := loadSample(t)
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "made_up_directive") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected warning for unknown directive, got %v", cfg.Warnings)
	}
}

func TestLoadErrors(t *testing.T) {
	cases := []string{
		`server {}`,                        // no http block
		`http {}`,                          // no servers
		`http { server { listen; } }`,      // listen without addr
		`http { server { location { } } }`, // location without pattern
		`http { server { listen 80; location /x { proxy_pass ftp://a; } } }`, // bad scheme
		`http { upstream u {} server { listen 80; } }`,                       // empty upstream
	}
	for _, src := range cases {
		ds, err := Parse(src)
		if err != nil {
			t.Fatalf("%q: parse error %v", src, err)
		}
		if _, err := Load(ds); err == nil {
			t.Errorf("%q: want load error, got nil", src)
		}
	}
}
