package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CombinedFormat is nginx's built-in "combined" access log format.
const CombinedFormat = `$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent"`

// Config is the fully loaded, typed configuration.
type Config struct {
	HTTP     *HTTP
	Stream   *Stream
	UI       *UIConfig
	Warnings []string
}

// UIConfig pins the admin console / control channel. All fields optional:
// zero values keep the default random loopback port + per-start token.
type UIConfig struct {
	Listen string   // fixed address, e.g. "127.0.0.1:8899"
	Token  string   // fixed token; "" = random per start
	Open   bool     // auto-open the browser on startup
	Allow  []string // IP/CIDR allow list; empty = no IP restriction
}

// Stream is the L4 proxy configuration.
type Stream struct {
	Upstreams map[string]*Upstream
	Servers   []*StreamServer
}

type StreamServer struct {
	Addr           string
	UDP            bool
	ProxyPass      string // upstream name or host:port
	ProxyTimeout   time.Duration
	ConnectTimeout time.Duration
	ProxyResponses int // UDP: datagrams expected per request; -1 = unlimited
	Line           int
}

type HTTP struct {
	Servers             []*Server
	Upstreams           map[string]*Upstream
	LogFormats          map[string]string
	AccessLog           string // "" = stdout, "off" = disabled
	AccessLogFormat     string
	ErrorLog            string // "" = stderr
	ErrorLevel          string
	Root                string
	Index               []string
	Gzip                GzipSettings
	LimitReqZones       map[string]LimitReqZone
	LimitConnZones      map[string]LimitConnZone
	CachePaths          map[string]string // keys_zone name → directory
	ClientMaxBody       *int64
	AddHeaders          []AddHeader
	Access              []AccessRule
	Auth                *AuthBasic
	Autoindex           *bool
	Maps                []MapVar
	ProxyConnectTimeout *time.Duration
	ProxyReadTimeout    *time.Duration
	Expires             *Expires
	ServerTokens        *bool
	SSLProtocols        []string
}

// Expires is a parsed "expires" directive (ngx_http_headers_module).
type Expires struct {
	Mode     string        // "off", "epoch", "max", "duration", "nocache"
	Duration time.Duration // for Mode "duration"
}

// ProxyRedirect is one proxy_redirect rule. From "off" disables rewriting,
// From "default" maps the proxy_pass URL back to the location prefix.
type ProxyRedirect struct {
	From, To string
}

type Upstream struct {
	Name     string
	Servers  []UpstreamServer
	Balancer string // "", "least_conn", "ip_hash"
	Line     int
}

type LimitReqZone struct {
	Key  string  // variable template, e.g. $binary_remote_addr
	Rate float64 // requests per second
}

type LimitReqUse struct {
	Zone    string
	Burst   float64
	NoDelay bool
}

type LimitConnZone struct {
	Key string
}

type LimitConnUse struct {
	Zone string
	Max  int
}

type CacheValid struct {
	Codes []int // -1 = any
	TTL   time.Duration
}

type UpstreamServer struct {
	Addr        string
	Weight      int
	MaxFails    int
	FailTimeout time.Duration
	Backup      bool
	Down        bool
}

type Listen struct {
	Addr    string
	Default bool
	SSL     bool
}

type Rewrite struct {
	Regex       string
	Replacement string
	Flag        string // "", "last", "break", "redirect", "permanent"
	Line        int
}

// GzipSettings uses pointers so unset values inherit from the outer context.
type GzipSettings struct {
	On        *bool
	Types     []string
	MinLength *int64
	Level     *int
}

type Server struct {
	Listens             []Listen
	ServerNames         []string
	Root                string
	Index               []string
	AccessLog           string
	AccessLogFormat     string
	Locations           []*Location
	Rewrites            []Rewrite
	SSLCert             string
	SSLKey              string
	Gzip                GzipSettings
	LimitReq            *LimitReqUse
	LimitConn           *LimitConnUse
	ProxyCache          string // "" = unset, "off" = disabled
	CacheValids         []CacheValid
	ErrorPages          []ErrorPage
	ClientMaxBody       *int64
	AddHeaders          []AddHeader
	Access              []AccessRule
	Auth                *AuthBasic
	Autoindex           *bool
	ProxyConnectTimeout *time.Duration
	ProxyReadTimeout    *time.Duration
	Expires             *Expires
	ServerTokens        *bool
	SSLProtocols        []string
	Line                int
}

type ErrorPage struct {
	Codes []int
	URI   string
}

type Location struct {
	Modifier            string // "", "=", "~", "~*", "^~"
	Pattern             string
	Root                string
	Alias               string
	Index               []string
	ProxyPass           string
	ReturnCode          int
	ReturnArg           string
	Rewrites            []Rewrite
	TryFiles            []string
	Gzip                GzipSettings
	LimitReq            *LimitReqUse
	LimitConn           *LimitConnUse
	ProxyCache          string
	CacheValids         []CacheValid
	ProxyHeaders        []ProxyHeader
	CookiePaths         []CookiePath
	ClientMaxBody       *int64
	AddHeaders          []AddHeader
	Access              []AccessRule
	Auth                *AuthBasic
	Autoindex           *bool
	ProxyConnectTimeout *time.Duration
	ProxyReadTimeout    *time.Duration
	Expires             *Expires
	ProxyRedirects      []ProxyRedirect
	HideHeaders         []string
	PassHeaders         []string
	Line                int
}

type ProxyHeader struct {
	Name  string
	Value string // variable template
}

type CookiePath struct {
	From, To string
}

type AddHeader struct {
	Name   string
	Value  string // variable template
	Always bool
}

type AccessRule struct {
	Allow bool
	Cidr  string // IP, CIDR, or "all"
}

// AuthBasic is auth_basic + auth_basic_user_file. Realm "off" disables.
type AuthBasic struct {
	Realm    string
	UserFile string
}

// MapVar is one http-level map block: $Source → $Target.
type MapVar struct {
	Source string // without $
	Target string // without $
	Cases  []MapCase
	Line   int
}

type MapCase struct {
	Key       string
	Value     string
	IsRegex   bool // ~pattern or ~*pattern
	CaseIns   bool
	IsDefault bool
}

// Load translates a parsed directive tree into typed configuration.
// Unknown directives are collected as warnings rather than errors so real
// nginx.conf files degrade gracefully.
func Load(ds []Directive) (*Config, error) {
	cfg := &Config{}
	for i := range ds {
		d := &ds[i]
		switch d.Name {
		case "http":
			if d.Block == nil {
				return nil, errDirective(d, "requires a block")
			}
			if cfg.HTTP != nil {
				return nil, errDirective(d, `duplicate "http" block`)
			}
			h, err := loadHTTP(cfg, d.Block)
			if err != nil {
				return nil, err
			}
			cfg.HTTP = h
		case "stream":
			if d.Block == nil {
				return nil, errDirective(d, "requires a block")
			}
			if cfg.Stream != nil {
				return nil, errDirective(d, `duplicate "stream" block`)
			}
			st, err := loadStream(cfg, d.Block)
			if err != nil {
				return nil, err
			}
			cfg.Stream = st
		case "ui":
			if d.Block == nil {
				return nil, errDirective(d, "requires a block")
			}
			u := &UIConfig{}
			for j := range d.Block {
				e := &d.Block[j]
				switch e.Name {
				case "listen":
					if len(e.Args) != 1 {
						return nil, errDirective(e, "needs exactly one argument")
					}
					u.Listen = e.Args[0]
				case "token":
					if len(e.Args) != 1 {
						return nil, errDirective(e, "needs exactly one argument")
					}
					u.Token = e.Args[0]
				case "open":
					b, err := parseOnOff(e)
					if err != nil {
						return nil, err
					}
					u.Open = *b
				case "allow":
					if len(e.Args) == 0 {
						return nil, errDirective(e, "needs at least one address")
					}
					u.Allow = append(u.Allow, e.Args...)
				default:
					cfg.warnf(e, "unknown directive %q in ui block (ignored)", e.Name)
				}
			}
			cfg.UI = u
		case "events", "worker_processes", "worker_connections", "user", "pid", "daemon", "error_log":
			// accepted for compatibility with real nginx.conf files
		case "include":
			cfg.warnf(d, `"include" is not supported yet (ignored)`)
		default:
			cfg.warnf(d, "unknown directive %q (ignored)", d.Name)
		}
	}
	if cfg.HTTP == nil && cfg.Stream == nil {
		return nil, fmt.Errorf(`missing "http" or "stream" block`)
	}
	return cfg, nil
}

func loadStream(cfg *Config, block []Directive) (*Stream, error) {
	st := &Stream{Upstreams: map[string]*Upstream{}}
	for i := range block {
		d := &block[i]
		switch d.Name {
		case "upstream":
			if d.Block == nil || len(d.Args) != 1 {
				return nil, errDirective(d, "needs a name and a block")
			}
			up, err := loadUpstream(cfg, d)
			if err != nil {
				return nil, err
			}
			st.Upstreams[up.Name] = up
		case "server":
			if d.Block == nil {
				return nil, errDirective(d, "requires a block")
			}
			ss, err := loadStreamServer(cfg, d)
			if err != nil {
				return nil, err
			}
			st.Servers = append(st.Servers, ss)
		default:
			cfg.warnf(d, "unknown directive %q in stream block (ignored)", d.Name)
		}
	}
	if len(st.Servers) == 0 {
		return nil, fmt.Errorf(`"stream" block contains no "server" blocks`)
	}
	return st, nil
}

func loadStreamServer(cfg *Config, sd *Directive) (*StreamServer, error) {
	s := &StreamServer{
		ProxyTimeout:   10 * time.Minute, // nginx stream default
		ConnectTimeout: 60 * time.Second,
		ProxyResponses: -1,
		Line:           sd.Line,
	}
	for i := range sd.Block {
		d := &sd.Block[i]
		switch d.Name {
		case "listen":
			if len(d.Args) == 0 {
				return nil, errDirective(d, "needs an address")
			}
			addr := d.Args[0]
			if strings.HasPrefix(addr, "*:") {
				addr = addr[1:]
			} else if _, err := strconv.Atoi(addr); err == nil {
				addr = ":" + addr
			}
			s.Addr = addr
			for _, f := range d.Args[1:] {
				if f == "udp" {
					s.UDP = true
				} else {
					cfg.warnf(d, "listen flag %q not supported in stream (ignored)", f)
				}
			}
		case "proxy_pass":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			s.ProxyPass = d.Args[0]
		case "proxy_timeout":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			t, err := parseNginxTime(d.Args[0])
			if err != nil {
				return nil, errDirective(d, err.Error())
			}
			s.ProxyTimeout = t
		case "proxy_connect_timeout":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			t, err := parseNginxTime(d.Args[0])
			if err != nil {
				return nil, errDirective(d, err.Error())
			}
			s.ConnectTimeout = t
		case "proxy_responses":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			n, err := strconv.Atoi(d.Args[0])
			if err != nil || n < 0 {
				return nil, errDirective(d, fmt.Sprintf("invalid number %q", d.Args[0]))
			}
			s.ProxyResponses = n
		default:
			cfg.warnf(d, "unknown directive %q in stream server block (ignored)", d.Name)
		}
	}
	if s.Addr == "" {
		return nil, errDirective(sd, `stream server needs a "listen" directive`)
	}
	if s.ProxyPass == "" {
		return nil, errDirective(sd, `stream server needs a "proxy_pass" directive`)
	}
	return s, nil
}

func loadHTTP(cfg *Config, block []Directive) (*HTTP, error) {
	h := &HTTP{
		Upstreams:       map[string]*Upstream{},
		LogFormats:      map[string]string{"combined": CombinedFormat},
		AccessLogFormat: "combined",
		ErrorLevel:      "error",
		LimitReqZones:   map[string]LimitReqZone{},
		LimitConnZones:  map[string]LimitConnZone{},
		CachePaths:      map[string]string{},
	}
	for i := range block {
		d := &block[i]
		switch d.Name {
		case "server":
			if d.Block == nil {
				return nil, errDirective(d, "requires a block")
			}
			s, err := loadServer(cfg, d)
			if err != nil {
				return nil, err
			}
			h.Servers = append(h.Servers, s)
		case "upstream":
			if d.Block == nil || len(d.Args) != 1 {
				return nil, errDirective(d, "needs a name and a block")
			}
			up, err := loadUpstream(cfg, d)
			if err != nil {
				return nil, err
			}
			if _, dup := h.Upstreams[up.Name]; dup {
				return nil, errDirective(d, fmt.Sprintf("duplicate upstream %q", up.Name))
			}
			h.Upstreams[up.Name] = up
		case "log_format":
			if len(d.Args) < 2 {
				return nil, errDirective(d, "needs a name and a format string")
			}
			// nginx concatenates consecutive quoted strings
			h.LogFormats[d.Args[0]] = strings.Join(d.Args[1:], "")
		case "access_log":
			path, format, err := parseAccessLog(d)
			if err != nil {
				return nil, err
			}
			h.AccessLog = path
			if format != "" {
				h.AccessLogFormat = format
			}
		case "error_log":
			if len(d.Args) == 0 {
				return nil, errDirective(d, "needs a path")
			}
			h.ErrorLog = d.Args[0]
			if len(d.Args) > 1 {
				h.ErrorLevel = d.Args[1]
			}
		case "root":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			h.Root = d.Args[0]
		case "index":
			h.Index = d.Args
		case "gzip", "gzip_types", "gzip_min_length", "gzip_comp_level":
			if err := parseGzip(&h.Gzip, d); err != nil {
				return nil, err
			}
		case "limit_req_zone":
			name, z, err := parseLimitReqZone(d)
			if err != nil {
				return nil, err
			}
			h.LimitReqZones[name] = z
		case "limit_conn_zone":
			name, z, err := parseLimitConnZone(d)
			if err != nil {
				return nil, err
			}
			h.LimitConnZones[name] = z
		case "proxy_cache_path":
			name, dir, err := parseCachePath(cfg, d)
			if err != nil {
				return nil, err
			}
			h.CachePaths[name] = dir
		case "client_max_body_size":
			n, err := parseBodySize(d)
			if err != nil {
				return nil, err
			}
			h.ClientMaxBody = &n
		case "add_header":
			ah, err := parseAddHeader(d)
			if err != nil {
				return nil, err
			}
			h.AddHeaders = append(h.AddHeaders, ah)
		case "allow", "deny":
			ar, err := parseAccessRule(d)
			if err != nil {
				return nil, err
			}
			h.Access = append(h.Access, ar)
		case "auth_basic":
			if err := parseAuthBasic(&h.Auth, d); err != nil {
				return nil, err
			}
		case "auth_basic_user_file":
			if err := parseAuthBasic(&h.Auth, d); err != nil {
				return nil, err
			}
		case "autoindex":
			b, err := parseOnOff(d)
			if err != nil {
				return nil, err
			}
			h.Autoindex = b
		case "proxy_connect_timeout", "proxy_read_timeout":
			t, err := parseDirectiveTime(d)
			if err != nil {
				return nil, err
			}
			if d.Name == "proxy_connect_timeout" {
				h.ProxyConnectTimeout = t
			} else {
				h.ProxyReadTimeout = t
			}
		case "map":
			m, err := parseMap(d)
			if err != nil {
				return nil, err
			}
			h.Maps = append(h.Maps, m)
		case "expires":
			e, err := parseExpiresDirective(d)
			if err != nil {
				return nil, err
			}
			h.Expires = e
		case "server_tokens":
			b, err := parseOnOff(d)
			if err != nil {
				return nil, err
			}
			h.ServerTokens = b
		case "ssl_protocols":
			if err := validateSSLProtocols(d.Args); err != nil {
				return nil, errDirective(d, err.Error())
			}
			h.SSLProtocols = d.Args
		case "proxy_send_timeout", "default_type", "sendfile", "keepalive_timeout",
			"tcp_nopush", "tcp_nodelay", "types_hash_max_size":
			// tuning knobs Go's HTTP stack handles on its own; silently accepted
		default:
			cfg.warnf(d, "unknown directive %q in http block (ignored)", d.Name)
		}
	}
	if len(h.Servers) == 0 {
		return nil, fmt.Errorf(`"http" block contains no "server" blocks`)
	}
	return h, nil
}

func loadServer(cfg *Config, sd *Directive) (*Server, error) {
	s := &Server{Line: sd.Line}
	for i := range sd.Block {
		d := &sd.Block[i]
		switch d.Name {
		case "listen":
			l, err := parseListen(cfg, d)
			if err != nil {
				return nil, err
			}
			s.Listens = append(s.Listens, l)
		case "server_name":
			s.ServerNames = append(s.ServerNames, d.Args...)
		case "root":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			s.Root = d.Args[0]
		case "index":
			s.Index = d.Args
		case "access_log":
			path, format, err := parseAccessLog(d)
			if err != nil {
				return nil, err
			}
			s.AccessLog = path
			s.AccessLogFormat = format
		case "location":
			if d.Block == nil {
				return nil, errDirective(d, "requires a block")
			}
			locs, err := loadLocation(cfg, d)
			if err != nil {
				return nil, err
			}
			s.Locations = append(s.Locations, locs...)
		case "rewrite":
			rw, err := parseRewrite(d)
			if err != nil {
				return nil, err
			}
			s.Rewrites = append(s.Rewrites, rw)
		case "ssl_certificate":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			s.SSLCert = d.Args[0]
		case "ssl_certificate_key":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			s.SSLKey = d.Args[0]
		case "gzip", "gzip_types", "gzip_min_length", "gzip_comp_level":
			if err := parseGzip(&s.Gzip, d); err != nil {
				return nil, err
			}
		case "limit_req":
			u, err := parseLimitReqUse(d)
			if err != nil {
				return nil, err
			}
			s.LimitReq = u
		case "limit_conn":
			u, err := parseLimitConnUse(d)
			if err != nil {
				return nil, err
			}
			s.LimitConn = u
		case "proxy_cache":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			s.ProxyCache = d.Args[0]
		case "proxy_cache_valid":
			v, err := parseCacheValid(d)
			if err != nil {
				return nil, err
			}
			s.CacheValids = append(s.CacheValids, v)
		case "error_page":
			if len(d.Args) < 2 {
				return nil, errDirective(d, "needs at least one code and a uri")
			}
			ep := ErrorPage{URI: d.Args[len(d.Args)-1]}
			for _, cs := range d.Args[:len(d.Args)-1] {
				if strings.HasPrefix(cs, "=") {
					cfg.warnf(d, "error_page response code change %q not supported (ignored)", cs)
					continue
				}
				n, err := strconv.Atoi(cs)
				if err != nil || n < 300 || n > 599 {
					return nil, errDirective(d, fmt.Sprintf("invalid code %q", cs))
				}
				ep.Codes = append(ep.Codes, n)
			}
			if len(ep.Codes) > 0 {
				s.ErrorPages = append(s.ErrorPages, ep)
			}
		case "client_max_body_size":
			n, err := parseBodySize(d)
			if err != nil {
				return nil, err
			}
			s.ClientMaxBody = &n
		case "add_header":
			ah, err := parseAddHeader(d)
			if err != nil {
				return nil, err
			}
			s.AddHeaders = append(s.AddHeaders, ah)
		case "allow", "deny":
			ar, err := parseAccessRule(d)
			if err != nil {
				return nil, err
			}
			s.Access = append(s.Access, ar)
		case "auth_basic", "auth_basic_user_file":
			if err := parseAuthBasic(&s.Auth, d); err != nil {
				return nil, err
			}
		case "autoindex":
			b, err := parseOnOff(d)
			if err != nil {
				return nil, err
			}
			s.Autoindex = b
		case "proxy_connect_timeout", "proxy_read_timeout":
			t, err := parseDirectiveTime(d)
			if err != nil {
				return nil, err
			}
			if d.Name == "proxy_connect_timeout" {
				s.ProxyConnectTimeout = t
			} else {
				s.ProxyReadTimeout = t
			}
		case "expires":
			e, err := parseExpiresDirective(d)
			if err != nil {
				return nil, err
			}
			s.Expires = e
		case "server_tokens":
			b, err := parseOnOff(d)
			if err != nil {
				return nil, err
			}
			s.ServerTokens = b
		case "ssl_protocols":
			if err := validateSSLProtocols(d.Args); err != nil {
				return nil, errDirective(d, err.Error())
			}
			s.SSLProtocols = d.Args
		case "proxy_send_timeout", "keepalive_timeout", "tcp_nopush", "tcp_nodelay",
			"sendfile", "client_body_buffer_size":
			// tuning knobs Go's HTTP stack handles on its own; silently accepted
		default:
			cfg.warnf(d, "unknown directive %q in server block (ignored)", d.Name)
		}
	}
	if len(s.Listens) == 0 {
		s.Listens = []Listen{{Addr: ":80"}}
	}
	return s, nil
}

// loadLocation parses one location block, flattening nested locations into
// the returned slice. Nested locations inherit root/index from their parent
// when unset. The first element is always the location itself.
func loadLocation(cfg *Config, ld *Directive) ([]*Location, error) {
	loc := &Location{Line: ld.Line}
	switch len(ld.Args) {
	case 1:
		loc.Pattern = ld.Args[0]
	case 2:
		switch ld.Args[0] {
		case "=", "~", "~*", "^~":
			loc.Modifier, loc.Pattern = ld.Args[0], ld.Args[1]
		default:
			return nil, errDirective(ld, fmt.Sprintf("invalid location modifier %q", ld.Args[0]))
		}
	default:
		return nil, errDirective(ld, "invalid number of arguments")
	}
	if strings.HasPrefix(loc.Pattern, "@") && loc.Modifier != "" {
		return nil, errDirective(ld, "named locations take no modifier")
	}
	var nested []*Directive
	for i := range ld.Block {
		d := &ld.Block[i]
		switch d.Name {
		case "root":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			loc.Root = d.Args[0]
		case "alias":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			loc.Alias = d.Args[0]
		case "index":
			loc.Index = d.Args
		case "proxy_pass":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			u, err := url.Parse(d.Args[0])
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, errDirective(d, fmt.Sprintf("invalid proxy_pass URL %q", d.Args[0]))
			}
			loc.ProxyPass = d.Args[0]
		case "return":
			code, arg, err := parseReturn(d)
			if err != nil {
				return nil, err
			}
			loc.ReturnCode, loc.ReturnArg = code, arg
		case "rewrite":
			rw, err := parseRewrite(d)
			if err != nil {
				return nil, err
			}
			loc.Rewrites = append(loc.Rewrites, rw)
		case "try_files":
			if len(d.Args) < 2 {
				return nil, errDirective(d, "needs at least two arguments")
			}
			loc.TryFiles = d.Args
		case "gzip", "gzip_types", "gzip_min_length", "gzip_comp_level":
			if err := parseGzip(&loc.Gzip, d); err != nil {
				return nil, err
			}
		case "limit_req":
			u, err := parseLimitReqUse(d)
			if err != nil {
				return nil, err
			}
			loc.LimitReq = u
		case "limit_conn":
			u, err := parseLimitConnUse(d)
			if err != nil {
				return nil, err
			}
			loc.LimitConn = u
		case "proxy_cache":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			loc.ProxyCache = d.Args[0]
		case "proxy_cache_valid":
			v, err := parseCacheValid(d)
			if err != nil {
				return nil, err
			}
			loc.CacheValids = append(loc.CacheValids, v)
		case "proxy_set_header":
			if len(d.Args) != 2 {
				return nil, errDirective(d, "needs a name and a value")
			}
			loc.ProxyHeaders = append(loc.ProxyHeaders, ProxyHeader{Name: d.Args[0], Value: d.Args[1]})
		case "proxy_cookie_path":
			if len(d.Args) != 2 {
				return nil, errDirective(d, "needs two arguments")
			}
			loc.CookiePaths = append(loc.CookiePaths, CookiePath{From: d.Args[0], To: d.Args[1]})
		case "location":
			if d.Block == nil {
				return nil, errDirective(d, "requires a block")
			}
			nested = append(nested, d)
		case "client_max_body_size":
			n, err := parseBodySize(d)
			if err != nil {
				return nil, err
			}
			loc.ClientMaxBody = &n
		case "add_header":
			ah, err := parseAddHeader(d)
			if err != nil {
				return nil, err
			}
			loc.AddHeaders = append(loc.AddHeaders, ah)
		case "allow", "deny":
			ar, err := parseAccessRule(d)
			if err != nil {
				return nil, err
			}
			loc.Access = append(loc.Access, ar)
		case "auth_basic", "auth_basic_user_file":
			if err := parseAuthBasic(&loc.Auth, d); err != nil {
				return nil, err
			}
		case "autoindex":
			b, err := parseOnOff(d)
			if err != nil {
				return nil, err
			}
			loc.Autoindex = b
		case "proxy_connect_timeout", "proxy_read_timeout":
			t, err := parseDirectiveTime(d)
			if err != nil {
				return nil, err
			}
			if d.Name == "proxy_connect_timeout" {
				loc.ProxyConnectTimeout = t
			} else {
				loc.ProxyReadTimeout = t
			}
		case "expires":
			e, err := parseExpiresDirective(d)
			if err != nil {
				return nil, err
			}
			loc.Expires = e
		case "proxy_redirect":
			switch len(d.Args) {
			case 1:
				if d.Args[0] != "off" && d.Args[0] != "default" {
					return nil, errDirective(d, `single argument must be "off" or "default"`)
				}
				loc.ProxyRedirects = append(loc.ProxyRedirects, ProxyRedirect{From: d.Args[0]})
			case 2:
				loc.ProxyRedirects = append(loc.ProxyRedirects, ProxyRedirect{From: d.Args[0], To: d.Args[1]})
			default:
				return nil, errDirective(d, "invalid number of arguments")
			}
		case "proxy_hide_header":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			loc.HideHeaders = append(loc.HideHeaders, d.Args[0])
		case "proxy_pass_header":
			if len(d.Args) != 1 {
				return nil, errDirective(d, "needs exactly one argument")
			}
			loc.PassHeaders = append(loc.PassHeaders, d.Args[0])
		case "proxy_send_timeout", "proxy_buffering", "proxy_http_version", "chunked_transfer_encoding",
			"tcp_nopush", "tcp_nodelay", "keepalive_timeout", "client_body_buffer_size":
			// tuning knobs Go's HTTP stack handles on its own; silently accepted
		default:
			cfg.warnf(d, "unknown directive %q in location block (ignored)", d.Name)
		}
	}
	flat := []*Location{loc}
	for _, nd := range nested {
		kids, err := loadLocation(cfg, nd)
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			if k.Root == "" && k.Alias == "" {
				k.Root = loc.Root
			}
			if len(k.Index) == 0 {
				k.Index = loc.Index
			}
		}
		flat = append(flat, kids...)
	}
	return flat, nil
}

func loadUpstream(cfg *Config, ud *Directive) (*Upstream, error) {
	up := &Upstream{Name: ud.Args[0], Line: ud.Line}
	for i := range ud.Block {
		d := &ud.Block[i]
		switch d.Name {
		case "server":
			if len(d.Args) == 0 {
				return nil, errDirective(d, "needs an address")
			}
			us := UpstreamServer{Addr: d.Args[0], Weight: 1}
			if !strings.Contains(us.Addr, ":") {
				us.Addr += ":80"
			}
			for _, f := range d.Args[1:] {
				if w, ok := strings.CutPrefix(f, "weight="); ok {
					n, err := strconv.Atoi(w)
					if err != nil || n < 1 {
						return nil, errDirective(d, fmt.Sprintf("invalid weight %q", w))
					}
					us.Weight = n
				} else if w, ok := strings.CutPrefix(f, "max_fails="); ok {
					n, err := strconv.Atoi(w)
					if err != nil || n < 0 {
						return nil, errDirective(d, fmt.Sprintf("invalid max_fails %q", w))
					}
					us.MaxFails = n
				} else if w, ok := strings.CutPrefix(f, "fail_timeout="); ok {
					t, err := parseNginxTime(w)
					if err != nil {
						return nil, errDirective(d, err.Error())
					}
					us.FailTimeout = t
				} else if f == "backup" {
					us.Backup = true
				} else if f == "down" {
					us.Down = true
				} else {
					cfg.warnf(d, "upstream server flag %q not supported (ignored)", f)
				}
			}
			up.Servers = append(up.Servers, us)
		case "least_conn", "ip_hash":
			up.Balancer = d.Name
		default:
			cfg.warnf(d, "unknown directive %q in upstream block (ignored)", d.Name)
		}
	}
	if len(up.Servers) == 0 {
		return nil, errDirective(ud, fmt.Sprintf("upstream %q has no servers", up.Name))
	}
	return up, nil
}

func parseListen(cfg *Config, d *Directive) (Listen, error) {
	if len(d.Args) == 0 {
		return Listen{}, errDirective(d, "needs an address")
	}
	l := Listen{}
	addr := d.Args[0]
	for _, f := range d.Args[1:] {
		switch f {
		case "default_server", "default":
			l.Default = true
		case "ssl":
			l.SSL = true
		default:
			cfg.warnf(d, "listen flag %q not supported yet (ignored)", f)
		}
	}
	switch {
	case strings.HasPrefix(addr, "*:"):
		addr = addr[1:]
	case !strings.Contains(addr, ":"):
		if _, err := strconv.Atoi(addr); err == nil {
			addr = ":" + addr
		} else {
			addr += ":80"
		}
	}
	l.Addr = addr
	return l, nil
}

func parseAccessLog(d *Directive) (path, format string, err error) {
	if len(d.Args) == 0 {
		return "", "", errDirective(d, "needs a path")
	}
	path = d.Args[0]
	if len(d.Args) > 1 {
		format = d.Args[1]
	}
	return path, format, nil
}

func parseReturn(d *Directive) (int, string, error) {
	switch len(d.Args) {
	case 1:
		a := d.Args[0]
		if strings.HasPrefix(a, "http://") || strings.HasPrefix(a, "https://") {
			return 302, a, nil
		}
		code, err := strconv.Atoi(a)
		if err != nil || code < 100 || code > 599 {
			return 0, "", errDirective(d, fmt.Sprintf("invalid return code %q", a))
		}
		return code, "", nil
	case 2:
		code, err := strconv.Atoi(d.Args[0])
		if err != nil || code < 100 || code > 599 {
			return 0, "", errDirective(d, fmt.Sprintf("invalid return code %q", d.Args[0]))
		}
		return code, d.Args[1], nil
	}
	return 0, "", errDirective(d, "invalid number of arguments")
}

// parseNginxTime parses nginx durations: 30, 30s, 10m, 2h, 1d.
func parseNginxTime(s string) (time.Duration, error) {
	unit := time.Second
	num := s
	switch {
	case strings.HasSuffix(s, "ms"):
		unit, num = time.Millisecond, s[:len(s)-2]
	case strings.HasSuffix(s, "s"):
		num = s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		unit, num = time.Minute, s[:len(s)-1]
	case strings.HasSuffix(s, "h"):
		unit, num = time.Hour, s[:len(s)-1]
	case strings.HasSuffix(s, "d"):
		unit, num = 24*time.Hour, s[:len(s)-1]
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid time %q", s)
	}
	return time.Duration(n) * unit, nil
}

// parseExpiresValue parses an expires value: off / epoch / max / 30d / -1.
func parseExpiresValue(s string) (*Expires, error) {
	switch s {
	case "off":
		return &Expires{Mode: "off"}, nil
	case "epoch":
		return &Expires{Mode: "epoch"}, nil
	case "max":
		return &Expires{Mode: "max"}, nil
	}
	neg := strings.HasPrefix(s, "-")
	t, err := parseNginxTime(strings.TrimPrefix(s, "-"))
	if err != nil {
		return nil, fmt.Errorf("invalid expires value %q", s)
	}
	if neg {
		return &Expires{Mode: "nocache", Duration: -t}, nil
	}
	return &Expires{Mode: "duration", Duration: t}, nil
}

func parseExpiresDirective(d *Directive) (*Expires, error) {
	if len(d.Args) != 1 {
		return nil, errDirective(d, "needs exactly one argument")
	}
	e, err := parseExpiresValue(d.Args[0])
	if err != nil {
		return nil, errDirective(d, err.Error())
	}
	return e, nil
}

// SSLProtocolVersions maps nginx ssl_protocols names to TLS version codes
// (crypto/tls constants). Exposed for the server package.
var SSLProtocolVersions = map[string]uint16{
	"TLSv1":   0x0301,
	"TLSv1.1": 0x0302,
	"TLSv1.2": 0x0303,
	"TLSv1.3": 0x0304,
}

func validateSSLProtocols(list []string) error {
	if len(list) == 0 {
		return fmt.Errorf("ssl_protocols needs at least one protocol")
	}
	for _, p := range list {
		if _, ok := SSLProtocolVersions[p]; !ok {
			return fmt.Errorf("unknown protocol %q (use TLSv1, TLSv1.1, TLSv1.2, TLSv1.3)", p)
		}
	}
	return nil
}

func parseAddHeader(d *Directive) (AddHeader, error) {
	if len(d.Args) < 2 || len(d.Args) > 3 {
		return AddHeader{}, errDirective(d, "needs a name and a value")
	}
	ah := AddHeader{Name: d.Args[0], Value: d.Args[1]}
	if len(d.Args) == 3 {
		if d.Args[2] != "always" {
			return AddHeader{}, errDirective(d, fmt.Sprintf("unknown parameter %q", d.Args[2]))
		}
		ah.Always = true
	}
	return ah, nil
}

func parseAccessRule(d *Directive) (AccessRule, error) {
	if len(d.Args) != 1 {
		return AccessRule{}, errDirective(d, "needs exactly one argument")
	}
	return AccessRule{Allow: d.Name == "allow", Cidr: d.Args[0]}, nil
}

func parseAuthBasic(pp **AuthBasic, d *Directive) error {
	if len(d.Args) != 1 {
		return errDirective(d, "needs exactly one argument")
	}
	if *pp == nil {
		*pp = &AuthBasic{}
	}
	if d.Name == "auth_basic" {
		(*pp).Realm = d.Args[0]
	} else {
		(*pp).UserFile = d.Args[0]
	}
	return nil
}

func parseOnOff(d *Directive) (*bool, error) {
	if len(d.Args) != 1 || (d.Args[0] != "on" && d.Args[0] != "off") {
		return nil, errDirective(d, `needs "on" or "off"`)
	}
	b := d.Args[0] == "on"
	return &b, nil
}

func parseDirectiveTime(d *Directive) (*time.Duration, error) {
	if len(d.Args) != 1 {
		return nil, errDirective(d, "needs exactly one argument")
	}
	t, err := parseNginxTime(d.Args[0])
	if err != nil {
		return nil, errDirective(d, err.Error())
	}
	return &t, nil
}

func parseMap(d *Directive) (MapVar, error) {
	if d.Block == nil || len(d.Args) != 2 ||
		!strings.HasPrefix(d.Args[0], "$") || !strings.HasPrefix(d.Args[1], "$") {
		return MapVar{}, errDirective(d, "needs $source and $target variables and a block")
	}
	m := MapVar{Source: d.Args[0][1:], Target: d.Args[1][1:], Line: d.Line}
	for i := range d.Block {
		e := &d.Block[i]
		if len(e.Args) != 1 {
			return MapVar{}, errDirective(e, "map entry needs exactly one value")
		}
		c := MapCase{Key: e.Name, Value: e.Args[0]}
		switch {
		case e.Name == "default":
			c.IsDefault = true
		case strings.HasPrefix(e.Name, "~*"):
			c.IsRegex, c.CaseIns, c.Key = true, true, e.Name[2:]
		case strings.HasPrefix(e.Name, "~"):
			c.IsRegex, c.Key = true, e.Name[1:]
		case e.Name == "hostnames", e.Name == "volatile":
			continue // accepted, not needed
		}
		m.Cases = append(m.Cases, c)
	}
	return m, nil
}

// parseBodySize parses nginx sizes: 200, 512k, 200m, 1g. 0 = unlimited.
func parseBodySize(d *Directive) (int64, error) {
	if len(d.Args) != 1 {
		return 0, errDirective(d, "needs exactly one argument")
	}
	n, err := parseSizeStr(d.Args[0])
	if err != nil {
		return 0, errDirective(d, err.Error())
	}
	return n, nil
}

// parseSizeStr parses an nginx size literal (200, 512k, 200m, 1g).
func parseSizeStr(s string) (int64, error) {
	mult := int64(1)
	num := s
	if len(s) > 0 {
		switch s[len(s)-1] {
		case 'k', 'K':
			mult, num = 1<<10, s[:len(s)-1]
		case 'm', 'M':
			mult, num = 1<<20, s[:len(s)-1]
		case 'g', 'G':
			mult, num = 1<<30, s[:len(s)-1]
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}

// parseRateStr parses an nginx limit_req rate literal (10r/s, 60r/m).
func parseRateStr(s string) (float64, error) {
	perMinute := false
	v := s
	if r, ok := strings.CutSuffix(v, "r/s"); ok {
		v = r
	} else if r, ok := strings.CutSuffix(v, "r/m"); ok {
		v, perMinute = r, true
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid rate %q", s)
	}
	if perMinute {
		n /= 60
	}
	return n, nil
}

func cutZoneName(arg string) (string, bool) {
	v, ok := strings.CutPrefix(arg, "zone=")
	if !ok {
		return "", false
	}
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[:i] // the shared-memory size is meaningless here
	}
	return v, v != ""
}

func parseLimitReqZone(d *Directive) (string, LimitReqZone, error) {
	if len(d.Args) < 3 {
		return "", LimitReqZone{}, errDirective(d, "needs a key, zone= and rate=")
	}
	z := LimitReqZone{Key: d.Args[0]}
	var name string
	for _, a := range d.Args[1:] {
		if n, ok := cutZoneName(a); ok {
			name = n
		} else if v, ok := strings.CutPrefix(a, "rate="); ok {
			perMinute := false
			if r, ok := strings.CutSuffix(v, "r/s"); ok {
				v = r
			} else if r, ok := strings.CutSuffix(v, "r/m"); ok {
				v, perMinute = r, true
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || n <= 0 {
				return "", z, errDirective(d, fmt.Sprintf("invalid rate %q", a))
			}
			if perMinute {
				n /= 60
			}
			z.Rate = n
		}
	}
	if name == "" || z.Rate == 0 {
		return "", z, errDirective(d, "needs zone= and rate=")
	}
	return name, z, nil
}

func parseLimitReqUse(d *Directive) (*LimitReqUse, error) {
	u := &LimitReqUse{}
	for _, a := range d.Args {
		if n, ok := cutZoneName(a); ok {
			u.Zone = n
		} else if v, ok := strings.CutPrefix(a, "burst="); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return nil, errDirective(d, fmt.Sprintf("invalid burst %q", a))
			}
			u.Burst = float64(n)
		} else if a == "nodelay" {
			u.NoDelay = true
		} else {
			return nil, errDirective(d, fmt.Sprintf("unknown parameter %q", a))
		}
	}
	if u.Zone == "" {
		return nil, errDirective(d, "needs zone=")
	}
	return u, nil
}

func parseLimitConnZone(d *Directive) (string, LimitConnZone, error) {
	if len(d.Args) != 2 {
		return "", LimitConnZone{}, errDirective(d, "needs a key and zone=")
	}
	name, ok := cutZoneName(d.Args[1])
	if !ok {
		return "", LimitConnZone{}, errDirective(d, "needs zone=")
	}
	return name, LimitConnZone{Key: d.Args[0]}, nil
}

func parseLimitConnUse(d *Directive) (*LimitConnUse, error) {
	if len(d.Args) != 2 {
		return nil, errDirective(d, "needs a zone name and a number")
	}
	n, err := strconv.Atoi(d.Args[1])
	if err != nil || n < 1 {
		return nil, errDirective(d, fmt.Sprintf("invalid number %q", d.Args[1]))
	}
	return &LimitConnUse{Zone: d.Args[0], Max: n}, nil
}

func parseCachePath(cfg *Config, d *Directive) (name, dir string, err error) {
	if len(d.Args) < 2 {
		return "", "", errDirective(d, "needs a path and keys_zone=")
	}
	dir = d.Args[0]
	for _, a := range d.Args[1:] {
		if v, ok := strings.CutPrefix(a, "keys_zone="); ok {
			if i := strings.IndexByte(v, ':'); i >= 0 {
				v = v[:i]
			}
			name = v
		} else {
			cfg.warnf(d, "proxy_cache_path parameter %q not supported (ignored)", a)
		}
	}
	if name == "" {
		return "", "", errDirective(d, "needs keys_zone=")
	}
	return name, dir, nil
}

func parseCacheValid(d *Directive) (CacheValid, error) {
	if len(d.Args) == 0 {
		return CacheValid{}, errDirective(d, "needs a time")
	}
	ttl, err := parseNginxTime(d.Args[len(d.Args)-1])
	if err != nil {
		return CacheValid{}, errDirective(d, err.Error())
	}
	v := CacheValid{TTL: ttl}
	codes := d.Args[:len(d.Args)-1]
	if len(codes) == 0 {
		// nginx default: only 200, 301 and 302 responses
		v.Codes = []int{200, 301, 302}
		return v, nil
	}
	for _, cs := range codes {
		if cs == "any" {
			v.Codes = append(v.Codes, -1)
			continue
		}
		n, err := strconv.Atoi(cs)
		if err != nil || n < 100 || n > 599 {
			return CacheValid{}, errDirective(d, fmt.Sprintf("invalid status code %q", cs))
		}
		v.Codes = append(v.Codes, n)
	}
	return v, nil
}

func parseRewrite(d *Directive) (Rewrite, error) {
	if len(d.Args) < 2 || len(d.Args) > 3 {
		return Rewrite{}, errDirective(d, "needs a regex and a replacement")
	}
	rw := Rewrite{Regex: d.Args[0], Replacement: d.Args[1], Line: d.Line}
	if len(d.Args) == 3 {
		switch d.Args[2] {
		case "last", "break", "redirect", "permanent":
			rw.Flag = d.Args[2]
		default:
			return Rewrite{}, errDirective(d, fmt.Sprintf("unknown flag %q", d.Args[2]))
		}
	}
	return rw, nil
}

func parseGzip(g *GzipSettings, d *Directive) error {
	switch d.Name {
	case "gzip":
		if len(d.Args) != 1 || (d.Args[0] != "on" && d.Args[0] != "off") {
			return errDirective(d, `needs "on" or "off"`)
		}
		on := d.Args[0] == "on"
		g.On = &on
	case "gzip_types":
		g.Types = append(g.Types, d.Args...)
	case "gzip_min_length":
		if len(d.Args) != 1 {
			return errDirective(d, "needs exactly one argument")
		}
		n, err := strconv.ParseInt(d.Args[0], 10, 64)
		if err != nil || n < 0 {
			return errDirective(d, fmt.Sprintf("invalid length %q", d.Args[0]))
		}
		g.MinLength = &n
	case "gzip_comp_level":
		if len(d.Args) != 1 {
			return errDirective(d, "needs exactly one argument")
		}
		n, err := strconv.Atoi(d.Args[0])
		if err != nil || n < 1 || n > 9 {
			return errDirective(d, fmt.Sprintf("invalid level %q", d.Args[0]))
		}
		g.Level = &n
	}
	return nil
}

func (c *Config) warnf(d *Directive, format string, args ...any) {
	c.Warnings = append(c.Warnings, fmt.Sprintf("line %d: %s", d.Line, fmt.Sprintf(format, args...)))
}

func errDirective(d *Directive, msg string) error {
	return fmt.Errorf("line %d: directive %q %s", d.Line, d.Name, msg)
}
