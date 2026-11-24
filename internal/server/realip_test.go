package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"crest/internal/config"
)

// send runs one request from peer (an "ip:port") straight through the server.
func send(inst *Instance, peer, path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = peer
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	inst.Addrs[0].ServeHTTP(rec, req)
	return rec
}

func TestRealIPClient(t *testing.T) {
	const peer = "10.0.0.1"
	trusted := []string{"10.0.0.0/8", "2001:db8:aaaa::/48"}
	cases := []struct {
		name      string
		peer      string
		recursive bool
		values    []string
		want      string // "" = address kept
	}{
		{"untrusted peer", "192.0.2.9", true, []string{"203.0.113.5"}, ""},
		{"single", peer, false, []string{"203.0.113.5"}, "203.0.113.5:0"},
		{"last entry wins", peer, false, []string{"6.6.6.6, 203.0.113.5"}, "203.0.113.5:0"},
		{"non-recursive stops at a trusted hop", peer, false, []string{"203.0.113.5, 10.0.0.2"}, "10.0.0.2:0"},
		{"recursive skips trusted hops", peer, true, []string{"6.6.6.6, 203.0.113.5, 10.0.0.3, 10.0.0.2"}, "203.0.113.5:0"},
		{"recursive, all trusted", peer, true, []string{"10.0.0.4, 10.0.0.3"}, "10.0.0.4:0"},
		{"lines form one list", peer, true, []string{"6.6.6.6", "203.0.113.5", "10.0.0.2"}, "203.0.113.5:0"},
		{"non-recursive reads the last line", peer, false, []string{"6.6.6.6", "203.0.113.5"}, "203.0.113.5:0"},
		{"empty last line reports nothing", peer, true, []string{"6.6.6.6", ""}, ""},
		{"empty line ends the walk", peer, true, []string{"6.6.6.6", " , ", "10.0.0.2"}, "10.0.0.2:0"},
		{"garbage", peer, true, []string{"unknown"}, ""},
		{"garbage ends the walk", peer, true, []string{"6.6.6.6, unknown, 10.0.0.2"}, "10.0.0.2:0"},
		{"separators", peer, true, []string{" 203.0.113.5 ,\t10.0.0.2 , "}, "203.0.113.5:0"},
		{"port", peer, false, []string{"203.0.113.5:4711"}, "203.0.113.5:4711"},
		{"port zero", peer, false, []string{"203.0.113.5:0"}, ""},
		{"ipv6", peer, false, []string{"2001:db8::7"}, "[2001:db8::7]:0"},
		{"ipv6 port", peer, false, []string{"[2001:db8::7]:4711"}, "[2001:db8::7]:4711"},
		{"ipv6 peer", "2001:db8:aaaa::1", true, []string{"203.0.113.5, 2001:db8:aaaa::2"}, "203.0.113.5:0"},
		{"ipv4-mapped peer", "::ffff:10.0.0.1", false, []string{"203.0.113.5"}, "203.0.113.5:0"},
		{"ipv4-mapped entry", peer, true, []string{"203.0.113.5, ::ffff:10.0.0.2"}, "203.0.113.5:0"},
		{"ipv4-mapped client", peer, false, []string{"::ffff:203.0.113.5"}, "203.0.113.5:0"},
		{"zone", peer, false, []string{"fe80::1%eth0"}, ""},
		{"zone as injection", peer, false, []string{`fe80::1%"x`}, ""},
		{"octal", peer, false, []string{"0203.0.113.5"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ri := &realIP{recursive: c.recursive}
			for _, p := range trusted {
				ri.from = append(ri.from, netip.MustParsePrefix(p))
			}
			got := ""
			if ap, ok := ri.client(netip.MustParseAddr(c.peer), c.values); ok {
				got = ap.String()
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestRealIPHeaderChoice(t *testing.T) {
	conf := func(directive string) string {
		return `http { access_log off; error_log stderr crit;
    set_real_ip_from 10.0.0.0/8; ` + directive + `
    server { listen 8080; location / { return 200 "$remote_addr $realip_remote_addr $realip_remote_port"; } } }`
	}
	const peer = "10.0.0.1:5555"
	cases := []struct {
		name, directive, peer, want string
		headers                     []string
	}{
		{"default is X-Real-IP", "", peer, "203.0.113.5 10.0.0.1 5555",
			[]string{"X-Real-IP", "203.0.113.5", "X-Forwarded-For", "198.51.100.1"}},
		{"X-Forwarded-For", "real_ip_header x-forwarded-for;", peer, "198.51.100.1 10.0.0.1 5555",
			[]string{"X-Real-IP", "203.0.113.5", "X-Forwarded-For", "198.51.100.1"}},
		{"custom header", "real_ip_header CF-Connecting-IP;", peer, "203.0.113.5 10.0.0.1 5555",
			[]string{"CF-Connecting-IP", "203.0.113.5"}},
		{"custom header uses the first line", "real_ip_header CF-Connecting-IP;", peer, "203.0.113.5 10.0.0.1 5555",
			[]string{"CF-Connecting-IP", "203.0.113.5", "CF-Connecting-IP", "6.6.6.6"}},
		{"header absent", "", peer, "10.0.0.1 10.0.0.1 5555", nil},
		{"untrusted peer", "", "192.0.2.9:5555", "192.0.2.9 192.0.2.9 5555",
			[]string{"X-Real-IP", "203.0.113.5"}},
		{"proxy_protocol trusts no header", "real_ip_header proxy_protocol;", peer, "10.0.0.1 10.0.0.1 5555",
			[]string{"X-Real-IP", "6.6.6.6", "Proxy_protocol", "6.6.6.6"}},
		{"ipv6 client", "", peer, "2001:db8::7 10.0.0.1 5555",
			[]string{"X-Real-IP", "2001:db8::7"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			inst := buildInstance(t, conf(c.directive))
			if got := send(inst, c.peer, "/", c.headers...).Body.String(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Without set_real_ip_from nothing is trusted, whatever the other directives.
func TestRealIPOffByDefault(t *testing.T) {
	inst := buildInstance(t, `http { access_log off; error_log stderr crit;
    real_ip_header X-Forwarded-For; real_ip_recursive on;
    server { listen 8080; location / { return 200 $remote_addr; } } }`)
	if got := send(inst, "127.0.0.1:1", "/", "X-Forwarded-For", "6.6.6.6", "X-Real-IP", "6.6.6.6").Body.String(); got != "127.0.0.1" {
		t.Errorf("got %q", got)
	}
}

func TestRealIPScopes(t *testing.T) {
	inst := buildInstance(t, `http { access_log off; error_log stderr crit;
    set_real_ip_from 10.0.0.0/8;
    real_ip_header X-Forwarded-For;
    server {
        listen 8080; server_name inherit.test;
        location / { return 200 $remote_addr; }
        location /late/ { set_real_ip_from 172.16.0.0/12; return 200 $remote_addr; }
        location /late/deny/ { set_real_ip_from 172.16.0.0/12; allow 203.0.113.5; deny all; return 200 ok; }
        location /once/ { real_ip_header X-Other; set_real_ip_from 0.0.0.0/0; return 200 $remote_addr; }
    }
    server {
        listen 8080; server_name recursive.test;
        real_ip_recursive on;
        location / { return 200 $remote_addr; }
    }
    server {
        listen 8080; server_name override.test;
        set_real_ip_from 192.0.2.0/24;
        location / { return 200 $remote_addr; }
    }
}`)
	const xff = "203.0.113.5, 10.9.9.9"
	for _, c := range []struct{ name, host, path, peer, want string }{
		{"http setting inherited", "inherit.test", "/", "10.0.0.1:1", "10.9.9.9"},
		{"recursive set alone", "recursive.test", "/", "10.0.0.1:1", "203.0.113.5"},
		{"server list replaces http list", "override.test", "/", "10.0.0.1:1", "10.0.0.1"},
		{"server list applies", "override.test", "/", "192.0.2.1:1", "10.9.9.9"},
		{"location list", "inherit.test", "/late/", "172.16.0.1:1", "10.9.9.9"},
		{"outside that location", "inherit.test", "/", "172.16.0.1:1", "172.16.0.1"},
		{"replaced only once", "inherit.test", "/once/", "10.0.0.1:1", "10.9.9.9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := send(inst, c.peer, "http://"+c.host+c.path, "X-Forwarded-For", xff, "X-Other", "6.6.6.6").Body.String()
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
	// the location's own access rules see the address it resolved
	for client, want := range map[string]int{"203.0.113.5": 200, "203.0.113.6": 403} {
		if rec := send(inst, "172.16.0.1:1", "http://inherit.test/late/deny/", "X-Forwarded-For", client); rec.Code != want {
			t.Errorf("client %s: %d, want %d", client, rec.Code, want)
		}
	}
}

// A client cannot escape a policy by sending the header itself.
func TestRealIPPolicies(t *testing.T) {
	inst := buildInstance(t, `http { access_log off; error_log stderr crit;
    set_real_ip_from 10.0.0.1;
    real_ip_header X-Forwarded-For;
    limit_req_zone $binary_remote_addr zone=perip:1m rate=1r/m;
    map $remote_addr $who { default other; 203.0.113.5 known; }
    server {
        listen 8080;
        location /acl/ { allow 203.0.113.5; deny all; return 200 ok; }
        location /limit/ { limit_req zone=perip; return 200 ok; }
        location /map/ { return 200 $who; }
    }
}`)
	check := func(what string, rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", what, rec.Code, want)
		}
	}
	const lb, direct = "10.0.0.1:1", "198.51.100.66:1"
	check("acl, allowed client", send(inst, lb, "/acl/", "X-Forwarded-For", "203.0.113.5"), 200)
	check("acl, other client", send(inst, lb, "/acl/", "X-Forwarded-For", "203.0.113.6"), 403)
	check("acl, forged entry behind the proxy", send(inst, lb, "/acl/", "X-Forwarded-For", "203.0.113.5, 203.0.113.6"), 403)
	check("acl, forged header without the proxy", send(inst, direct, "/acl/", "X-Forwarded-For", "203.0.113.5"), 403)

	check("limit, client A", send(inst, lb, "/limit/", "X-Forwarded-For", "203.0.113.10"), 200)
	check("limit, client B has its own budget", send(inst, lb, "/limit/", "X-Forwarded-For", "203.0.113.11"), 200)
	check("limit, client A again", send(inst, lb, "/limit/", "X-Forwarded-For", "203.0.113.10"), 503)
	check("limit, A behind a forged entry", send(inst, lb, "/limit/", "X-Forwarded-For", "6.6.6.6, 203.0.113.10"), 503)
	check("limit, A in mapped form", send(inst, lb, "/limit/", "X-Forwarded-For", "::ffff:203.0.113.10"), 503)
	check("limit, direct", send(inst, direct, "/limit/", "X-Forwarded-For", "203.0.113.20"), 200)
	check("limit, direct with another forged header", send(inst, direct, "/limit/", "X-Forwarded-For", "203.0.113.21"), 503)

	if got := send(inst, lb, "/map/", "X-Forwarded-For", "203.0.113.5").Body.String(); got != "known" {
		t.Errorf("map: %q", got)
	}
}

func TestRealIPUpstreamHeaders(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "real=%s xff=%s peer=%s", r.Header.Get("X-Real-IP"),
			strings.Join(r.Header.Values("X-Forwarded-For"), "|"), r.Header.Get("X-Peer"))
	}))
	defer backend.Close()
	bu, _ := url.Parse(backend.URL)
	inst := buildFromYAML(t, fmt.Sprintf(`
error_log: {path: stderr, level: crit}
http:
  access_log: {off: true}
  set_real_ip_from: [10.0.0.0/8]
  real_ip_header: X-Forwarded-For
  real_ip_recursive: true
  servers:
    - listen: [8080]
      locations:
        - path: /
          proxy_pass: http://%s
          proxy_set_header: {X-Peer: $realip_remote_addr}
`, bu.Host))
	// like nginx, $proxy_add_x_forwarded_for appends the replaced address
	got := send(inst, "10.0.0.1:1", "/", "X-Forwarded-For", "203.0.113.5, 10.0.0.2").Body.String()
	if want := "real=203.0.113.5 xff=203.0.113.5, 10.0.0.2, 203.0.113.5 peer=10.0.0.1"; got != want {
		t.Errorf("trusted:\n got %q\nwant %q", got, want)
	}
	got = send(inst, "192.0.2.9:1", "/", "X-Forwarded-For", "203.0.113.5", "X-Real-IP", "6.6.6.6").Body.String()
	if want := "real=192.0.2.9 xff=203.0.113.5, 192.0.2.9 peer=192.0.2.9"; got != want {
		t.Errorf("untrusted:\n got %q\nwant %q", got, want)
	}
}

func TestRealIPIPHash(t *testing.T) {
	var hosts []string
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("b%d", i)
		b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, name) }))
		defer b.Close()
		u, _ := url.Parse(b.URL)
		hosts = append(hosts, "server "+u.Host+";")
	}
	inst := buildInstance(t, fmt.Sprintf(`http { access_log off; error_log stderr crit;
    set_real_ip_from 10.0.0.1;
    upstream pool { ip_hash; %s }
    server { listen 8080; location / { proxy_pass http://pool; } } }`, strings.Join(hosts, " ")))

	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		client := fmt.Sprintf("203.0.%d.7", i)
		first := send(inst, "10.0.0.1:1", "/", "X-Real-IP", client).Body.String()
		if again := send(inst, "10.0.0.1:2", "/", "X-Real-IP", client).Body.String(); again != first {
			t.Fatalf("client %s moved from %s to %s", client, first, again)
		}
		seen[first] = true
	}
	if len(seen) < 2 {
		t.Errorf("all clients behind the proxy hit one backend: %v", seen)
	}
}

// The log must show the client for every outcome, including requests denied
// before location matching and error pages.
func TestRealIPAccessLog(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "404.html"), "gone")
	log := filepath.Join(dir, "access.log")
	inst := buildInstance(t, fmt.Sprintf(`http { error_log stderr crit;
    log_format f '$remote_addr $realip_remote_addr $status';
    access_log %q f;
    set_real_ip_from 10.0.0.1;
    server {
        listen 8080; root %q;
        error_page 404 /404.html;
        location /ok { return 200 ok; }
        location /late { set_real_ip_from 172.16.0.1; return 200 ok; }
    }
}`, filepath.ToSlash(log), filepath.ToSlash(dir)))
	send(inst, "10.0.0.1:1", "/ok", "X-Real-IP", "203.0.113.5")
	send(inst, "10.0.0.1:1", "/missing", "X-Real-IP", "203.0.113.6")
	send(inst, "10.0.0.1:1", "/actuator/health", "X-Real-IP", "203.0.113.7")
	send(inst, "172.16.0.1:1", "/late", "X-Real-IP", "203.0.113.8")
	send(inst, "192.0.2.9:1", "/ok", "X-Real-IP", "6.6.6.6")
	if err := inst.Reopen(); err != nil { // flush
		t.Fatal(err)
	}
	for _, al := range inst.accessLogs {
		defer al.Close()
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := `203.0.113.5 10.0.0.1 200
203.0.113.6 10.0.0.1 404
203.0.113.7 10.0.0.1 403
203.0.113.8 172.16.0.1 200
192.0.2.9 192.0.2.9 200
`
	if string(data) != want {
		t.Errorf("got:\n%swant:\n%s", data, want)
	}
}

func TestResolveRealIP(t *testing.T) {
	from := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	for _, c := range []struct {
		name   string
		scopes []config.RealIP
		on     bool
	}{
		{"nothing set", []config.RealIP{{}, {}}, false},
		{"header without a trusted proxy", []config.RealIP{{Header: "X-Forwarded-For"}, {}}, false},
		{"inherited", []config.RealIP{{}, {From: from}}, true},
		{"emptied by the inner scope", []config.RealIP{{From: []netip.Prefix{}}, {From: from}}, false},
		{"proxy_protocol", []config.RealIP{{Header: config.RealIPProxyProtocol}, {From: from}}, false},
		{"proxy_protocol overridden", []config.RealIP{{Header: "X-Real-IP"}, {From: from, Header: config.RealIPProxyProtocol}}, true},
	} {
		if got := resolveRealIP(c.scopes...) != nil; got != c.on {
			t.Errorf("%s: enabled=%v, want %v", c.name, got, c.on)
		}
	}
}
