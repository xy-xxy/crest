package config

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func loadNginx(t *testing.T, src string) (*Config, error) {
	t.Helper()
	ds, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	return Load(ds)
}

func prefixes(ss ...string) []netip.Prefix {
	out := []netip.Prefix{}
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// Both front-ends must produce the same settings at every scope.
func TestRealIPBothFrontEnds(t *testing.T) {
	fromNginx, err := loadNginx(t, `
http {
    set_real_ip_from 10.0.0.0/8;
    set_real_ip_from 192.168.1.77/24;   # host bits are masked
    set_real_ip_from 127.0.0.1;
    set_real_ip_from 2001:db8::/32;
    real_ip_header X-Forwarded-For;
    real_ip_recursive on;
    server {
        listen 8080;
        real_ip_header CF-Connecting-IP;
        location / {
            set_real_ip_from ::1;
            real_ip_recursive off;
            return 200;
        }
    }
}`)
	if err != nil {
		t.Fatal(err)
	}
	fromYAML, err := LoadYAML([]byte(`
http:
  set_real_ip_from: [10.0.0.0/8, 192.168.1.77/24, 127.0.0.1, "2001:db8::/32"]
  real_ip_header: X-Forwarded-For
  real_ip_recursive: true
  servers:
    - listen: [8080]
      real_ip_header: CF-Connecting-IP
      locations:
        - path: /
          set_real_ip_from: ["::1"]
          real_ip_recursive: false
          return: {code: 200}
`))
	if err != nil {
		t.Fatal(err)
	}

	on, off := true, false
	want := []RealIP{
		{From: prefixes("10.0.0.0/8", "192.168.1.0/24", "127.0.0.1/32", "2001:db8::/32"), Header: "X-Forwarded-For", Recursive: &on},
		{Header: "CF-Connecting-IP"},
		{From: prefixes("::1/128"), Recursive: &off},
	}
	for name, cfg := range map[string]*Config{"nginx": fromNginx, "yaml": fromYAML} {
		if len(cfg.Warnings) != 0 {
			t.Errorf("%s: unexpected warnings %v", name, cfg.Warnings)
		}
		sv := cfg.HTTP.Servers[0]
		got := []RealIP{cfg.HTTP.RealIP, sv.RealIP, sv.Locations[0].RealIP}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
}

func TestRealIPInherit(t *testing.T) {
	on, off := true, false
	outer := RealIP{From: prefixes("10.0.0.0/8"), Header: "X-Forwarded-For", Recursive: &on}
	if got := (RealIP{}).Inherit(outer); !reflect.DeepEqual(got, outer) {
		t.Errorf("unset scope: got %+v", got)
	}
	inner := RealIP{From: []netip.Prefix{}, Header: "X-Real-IP", Recursive: &off}
	if got := inner.Inherit(outer); !reflect.DeepEqual(got, inner) {
		t.Errorf("fully set scope: got %+v", got)
	}
	if !(RealIP{}).IsZero() || inner.IsZero() || (RealIP{From: []netip.Prefix{}}).IsZero() {
		t.Error("IsZero must treat an explicit empty list as set")
	}
}

func TestRealIPNestedLocation(t *testing.T) {
	cfg, err := loadNginx(t, `
http { server { listen 8080;
    location /a/ {
        set_real_ip_from 10.0.0.0/8;
        location /a/b/ {
            real_ip_header X-Forwarded-For;
            location /a/b/c/ { return 200; }
        }
    }
} }`)
	if err != nil {
		t.Fatal(err)
	}
	locs := cfg.HTTP.Servers[0].Locations
	if len(locs) != 3 {
		t.Fatalf("got %d locations", len(locs))
	}
	want := RealIP{From: prefixes("10.0.0.0/8"), Header: "X-Forwarded-For"}
	for _, l := range locs[1:] {
		if !reflect.DeepEqual(l.RealIP, want) {
			t.Errorf("%s: got %+v, want %+v", l.Pattern, l.RealIP, want)
		}
	}
}

// Directives real nginx configs carry but crest cannot honor must not make it
// trust more than nginx would.
func TestRealIPUnsupportedSources(t *testing.T) {
	cfg, err := loadNginx(t, `
http {
    set_real_ip_from 10.0.0.0/8;
    server {
        listen 8080;
        set_real_ip_from unix:;
        real_ip_header proxy_protocol;
    }
}`)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.HTTP.Servers[0].RealIP
	if got.From == nil || len(got.From) != 0 {
		t.Errorf("unix: must override the http list with an empty one, got %#v", got.From)
	}
	if got.Header != RealIPProxyProtocol {
		t.Errorf("header = %q", got.Header)
	}
	all := strings.Join(cfg.Warnings, "\n")
	for _, w := range []string{"unix:", "proxy_protocol"} {
		if !strings.Contains(all, w) {
			t.Errorf("no warning about %s in %q", w, all)
		}
	}

	y, err := LoadYAML([]byte("http:\n  set_real_ip_from: [10.0.0.0/8]\n  servers:\n    - listen: [8080]\n      set_real_ip_from: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if from := y.HTTP.Servers[0].RealIP.From; from == nil || len(from) != 0 {
		t.Errorf("yaml empty list must trust nobody, got %#v", from)
	}
}

func TestRealIPInvalid(t *testing.T) {
	for _, d := range []string{
		"set_real_ip_from;",
		"set_real_ip_from 10.0.0.1 10.0.0.2;",
		"set_real_ip_from proxy.example.com;",
		"set_real_ip_from 10.0.0.0/33;",
		"set_real_ip_from 10.0.0.1:80;",
		"set_real_ip_from 010.0.0.1;",
		"set_real_ip_from ::ffff:10.0.0.1;",
		"set_real_ip_from ::ffff:10.0.0.0/104;",
		`set_real_ip_from "";`,
		"real_ip_header;",
		"real_ip_header X-Real-IP X-Forwarded-For;",
		"real_ip_header $http_x_forwarded_for;",
		"real_ip_header X-Real-IP:;",
		`real_ip_header "";`,
		"real_ip_recursive;",
		"real_ip_recursive true;",
	} {
		for _, src := range []string{
			"http { " + d + " server { listen 8080; } }",
			"http { server { listen 8080; " + d + " } }",
			"http { server { listen 8080; location / { " + d + " } } }",
		} {
			if _, err := loadNginx(t, src); err == nil {
				t.Errorf("accepted: %s", src)
			}
		}
	}
	for _, f := range []string{
		"set_real_ip_from: [proxy.example.com]",
		"set_real_ip_from: ['10.0.0.0/33']",
		"set_real_ip_from: ['::ffff:10.0.0.1']",
		"set_real_ip_from: ['unix:']",
		"set_real_ip_from: ['']",
		"real_ip_header: $http_x_forwarded_for",
		"real_ip_header: proxy_protocol",
		"real_ip_recursive: maybe",
	} {
		for _, src := range []string{
			"http:\n  " + f + "\n  servers:\n    - listen: [8080]\n",
			"http:\n  servers:\n    - listen: [8080]\n      " + f + "\n",
			"http:\n  servers:\n    - listen: [8080]\n      locations:\n        - path: /\n          " + f + "\n",
		} {
			if _, err := LoadYAML([]byte(src)); err == nil {
				t.Errorf("accepted: %s", src)
			}
		}
	}
}
