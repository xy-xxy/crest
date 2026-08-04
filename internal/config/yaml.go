package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// LoadYAML parses gonx's native YAML configuration into the same typed
// *Config the nginx-syntax path produces, so the rest of the server is
// format-agnostic.
func LoadYAML(src []byte) (*Config, error) {
	var y yamlRoot
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	dec.KnownFields(true) // reject typos instead of silently ignoring them
	if err := dec.Decode(&y); err != nil {
		return nil, err
	}
	return y.toConfig()
}

// ---- YAML schema ----------------------------------------------------------

type yamlRoot struct {
	ErrorLog *yamlErrorLog `yaml:"error_log"`
	HTTP     *yamlHTTP     `yaml:"http"`
	Stream   *yamlStream   `yaml:"stream"`
	UI       *yamlUI       `yaml:"ui"`
}

type yamlUI struct {
	Listen string   `yaml:"listen"`
	Token  string   `yaml:"token"`
	Open   bool     `yaml:"open"`
	Allow  []string `yaml:"allow"`
}

type yamlErrorLog struct {
	Path  string `yaml:"path"`
	Level string `yaml:"level"`
}

// yamlCommon holds directives valid at http, server and location scope.
type yamlCommon struct {
	Root                string       `yaml:"root"`
	Index               []string     `yaml:"index"`
	Gzip                *yamlGzip    `yaml:"gzip"`
	AddHeaders          []yamlHeader `yaml:"add_headers"`
	Access              []yamlAccess `yaml:"access"`
	Auth                *yamlAuth    `yaml:"auth_basic"`
	Autoindex           *bool        `yaml:"autoindex"`
	ClientMaxBodySize   string       `yaml:"client_max_body_size"`
	ProxyConnectTimeout string       `yaml:"proxy_connect_timeout"`
	ProxyReadTimeout    string       `yaml:"proxy_read_timeout"`
	ProxySendTimeout    string       `yaml:"proxy_send_timeout"`
	Expires             string       `yaml:"expires"`
	LimitRate           string       `yaml:"limit_rate"`
	LimitRateAfter      string       `yaml:"limit_rate_after"`
	GzipStatic          *bool        `yaml:"gzip_static"`
}

type yamlHTTP struct {
	yamlCommon     `yaml:",inline"`
	AccessLog      *yamlAccessLog              `yaml:"access_log"`
	LogFormats     map[string]string           `yaml:"log_formats"`
	Upstreams      map[string]*yamlUpstream    `yaml:"upstreams"`
	LimitReqZones  map[string]yamlLimitReqZone `yaml:"limit_req_zones"`
	LimitConnZones map[string]string           `yaml:"limit_conn_zones"` // name → key
	CachePaths     map[string]string           `yaml:"cache_paths"`      // name → dir
	Maps           []yamlMap                   `yaml:"maps"`
	Servers        []*yamlServer               `yaml:"servers"`
	ServerTokens   *bool                       `yaml:"server_tokens"`
	SSLProtocols   []string                    `yaml:"ssl_protocols"`
}

type yamlAccessLog struct {
	Path   string `yaml:"path"`
	Format string `yaml:"format"`
	Off    bool   `yaml:"off"`
}

type yamlGzip struct {
	Enabled   *bool    `yaml:"enabled"`
	Types     []string `yaml:"types"`
	MinLength string   `yaml:"min_length"`
	Level     *int     `yaml:"level"`
}

type yamlHeader struct {
	Name   string `yaml:"name"`
	Value  string `yaml:"value"`
	Always bool   `yaml:"always"`
}

// yamlAccess is one allow/deny rule; exactly one field is set.
type yamlAccess struct {
	Allow string `yaml:"allow"`
	Deny  string `yaml:"deny"`
}

type yamlAuth struct {
	Realm    string `yaml:"realm"`
	UserFile string `yaml:"user_file"`
}

type yamlUpstream struct {
	Balancer  string               `yaml:"balancer"` // round_robin | least_conn | ip_hash
	Servers   []yamlUpstreamServer `yaml:"servers"`
	Keepalive int                  `yaml:"keepalive"`
}

type yamlUpstreamServer struct {
	Address     string `yaml:"address"`
	Weight      int    `yaml:"weight"`
	MaxFails    int    `yaml:"max_fails"`
	FailTimeout string `yaml:"fail_timeout"`
	Backup      bool   `yaml:"backup"`
	Down        bool   `yaml:"down"`
}

type yamlLimitReqZone struct {
	Key  string `yaml:"key"`
	Rate string `yaml:"rate"`
}

type yamlMap struct {
	Source string            `yaml:"source"`
	Target string            `yaml:"target"`
	Cases  map[string]string `yaml:"cases"`
}

type yamlServer struct {
	yamlCommon    `yaml:",inline"`
	Listen        []yamlListen     `yaml:"listen"`
	ServerName    []string         `yaml:"server_name"`
	AccessLog     *yamlAccessLog   `yaml:"access_log"`
	SSLCert       string           `yaml:"ssl_certificate"`
	SSLKey        string           `yaml:"ssl_certificate_key"`
	Rewrites      []yamlRewrite    `yaml:"rewrites"`
	ProxyCache    string           `yaml:"proxy_cache"`
	CacheValid    []yamlCacheValid `yaml:"proxy_cache_valid"`
	ErrorPages    []yamlErrorPage  `yaml:"error_pages"`
	LimitReq      *yamlLimitReq    `yaml:"limit_req"`
	LimitConn     *yamlLimitConn   `yaml:"limit_conn"`
	Locations     []*yamlLocation  `yaml:"locations"`
	ServerTokens  *bool            `yaml:"server_tokens"`
	SSLProtocols  []string         `yaml:"ssl_protocols"`
	ValidReferers []string         `yaml:"valid_referers"`
}

// yamlListen accepts either a bare port/address (scalar) or a mapping with
// flags, so `listen: [8080]` and `listen: [{address: 443, ssl: true}]` both work.
type yamlListen struct {
	Address string
	SSL     bool
	Default bool
}

func (l *yamlListen) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		// scalar form: a bare port/address, or nginx-style "443 ssl default"
		fields := strings.Fields(node.Value)
		if len(fields) == 0 {
			return fmt.Errorf("listen entry is empty")
		}
		l.Address = fields[0]
		for _, f := range fields[1:] {
			switch f {
			case "ssl":
				l.SSL = true
			case "default", "default_server":
				l.Default = true
			default:
				return fmt.Errorf("listen %q: unknown flag %q (only ssl / default)", node.Value, f)
			}
		}
		return nil
	}
	var m struct {
		Port    string `yaml:"port"`
		Address string `yaml:"address"` // 兼容旧写法
		SSL     bool   `yaml:"ssl"`
		Default bool   `yaml:"default"`
	}
	if err := node.Decode(&m); err != nil {
		return err
	}
	addr := m.Port
	if addr == "" {
		addr = m.Address
	}
	if addr == "" {
		return fmt.Errorf(`listen entry needs a port, e.g. {port: 443, ssl: true} or "443 ssl"`)
	}
	l.Address, l.SSL, l.Default = addr, m.SSL, m.Default
	return nil
}

type yamlRewrite struct {
	Pattern     string `yaml:"pattern"`
	Replacement string `yaml:"replacement"`
	Flag        string `yaml:"flag"`
}

type yamlCacheValid struct {
	Codes []int  `yaml:"codes"` // empty → nginx default (200,301,302); [-1] via "any: true"
	Any   bool   `yaml:"any"`
	Time  string `yaml:"time"`
}

type yamlErrorPage struct {
	Codes []int  `yaml:"codes"`
	URI   string `yaml:"uri"`
}

type yamlLimitReq struct {
	Zone    string `yaml:"zone"`
	Burst   int    `yaml:"burst"`
	NoDelay bool   `yaml:"nodelay"`
}

type yamlLimitConn struct {
	Zone string `yaml:"zone"`
	Max  int    `yaml:"max"`
}

type yamlLocation struct {
	yamlCommon    `yaml:",inline"`
	Path          string              `yaml:"path"`
	Match         string              `yaml:"match"` // exact | prefix | regex | regex_i | prefix_priority
	Alias         string              `yaml:"alias"`
	ProxyPass     string              `yaml:"proxy_pass"`
	ProxySetHead  map[string]string   `yaml:"proxy_set_header"`
	ProxyCookie   []yamlCookiePath    `yaml:"proxy_cookie_path"`
	Return        *yamlReturn         `yaml:"return"`
	Rewrites      []yamlRewrite       `yaml:"rewrites"`
	TryFiles      []string            `yaml:"try_files"`
	ProxyCache    string              `yaml:"proxy_cache"`
	CacheValid    []yamlCacheValid    `yaml:"proxy_cache_valid"`
	LimitReq      *yamlLimitReq       `yaml:"limit_req"`
	LimitConn     *yamlLimitConn      `yaml:"limit_conn"`
	ProxyRedirect []yamlProxyRedirect `yaml:"proxy_redirect"`
	HideHeaders   []string            `yaml:"proxy_hide_header"`
	PassHeaders   []string            `yaml:"proxy_pass_header"`
	ValidReferers []string            `yaml:"valid_referers"`
}

// yamlProxyRedirect accepts "off" / "default" scalars or {from, to} mappings.
type yamlProxyRedirect struct {
	From, To string
}

func (p *yamlProxyRedirect) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		if node.Value != "off" && node.Value != "default" {
			return fmt.Errorf(`proxy_redirect: scalar entry must be "off" or "default"`)
		}
		p.From = node.Value
		return nil
	}
	var m struct {
		From string `yaml:"from"`
		To   string `yaml:"to"`
	}
	if err := node.Decode(&m); err != nil {
		return err
	}
	if m.From == "" {
		return fmt.Errorf("proxy_redirect entry needs from/to")
	}
	p.From, p.To = m.From, m.To
	return nil
}

type yamlCookiePath struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type yamlReturn struct {
	Code int    `yaml:"code"`
	Body string `yaml:"body"` // text or redirect URL
}

type yamlStream struct {
	Upstreams map[string]*yamlUpstream `yaml:"upstreams"`
	Servers   []*yamlStreamServer      `yaml:"servers"`
}

type yamlStreamServer struct {
	Listen         string `yaml:"listen"`
	UDP            bool   `yaml:"udp"`
	ProxyPass      string `yaml:"proxy_pass"`
	ProxyTimeout   string `yaml:"proxy_timeout"`
	ConnectTimeout string `yaml:"proxy_connect_timeout"`
	ProxyResponses *int   `yaml:"proxy_responses"`
}

// ---- conversion to *Config ------------------------------------------------

func (y *yamlRoot) toConfig() (*Config, error) {
	cfg := &Config{}
	if y.HTTP != nil {
		h, err := y.HTTP.convert(cfg)
		if err != nil {
			return nil, err
		}
		if y.ErrorLog != nil {
			h.ErrorLog = y.ErrorLog.Path
			if y.ErrorLog.Level != "" {
				h.ErrorLevel = y.ErrorLog.Level
			}
		}
		cfg.HTTP = h
	}
	if y.Stream != nil {
		st, err := y.Stream.convert()
		if err != nil {
			return nil, err
		}
		cfg.Stream = st
	}
	if y.UI != nil {
		cfg.UI = &UIConfig{Listen: y.UI.Listen, Token: y.UI.Token, Open: y.UI.Open, Allow: y.UI.Allow}
	}
	if cfg.HTTP == nil && cfg.Stream == nil {
		return nil, fmt.Errorf("configuration has neither an http nor a stream section")
	}
	return cfg, nil
}

func (yh *yamlHTTP) convert(cfg *Config) (*HTTP, error) {
	h := &HTTP{
		Upstreams:       map[string]*Upstream{},
		LogFormats:      map[string]string{"combined": CombinedFormat},
		AccessLogFormat: "combined",
		ErrorLevel:      "error",
		LimitReqZones:   map[string]LimitReqZone{},
		LimitConnZones:  map[string]LimitConnZone{},
		CachePaths:      map[string]string{},
	}
	for k, v := range yh.LogFormats {
		h.LogFormats[k] = v
	}
	if yh.AccessLog != nil {
		if yh.AccessLog.Off {
			h.AccessLog = "off"
		} else {
			h.AccessLog = yh.AccessLog.Path
			if yh.AccessLog.Format != "" {
				h.AccessLogFormat = yh.AccessLog.Format
			}
		}
	}
	if err := applyCommonToHTTP(h, &yh.yamlCommon); err != nil {
		return nil, err
	}
	h.ServerTokens = yh.ServerTokens
	if len(yh.SSLProtocols) > 0 {
		if err := validateSSLProtocols(yh.SSLProtocols); err != nil {
			return nil, err
		}
		h.SSLProtocols = yh.SSLProtocols
	}
	for name, yu := range yh.Upstreams {
		up, err := yu.convert(name)
		if err != nil {
			return nil, err
		}
		h.Upstreams[name] = up
	}
	for name, z := range yh.LimitReqZones {
		rate, err := parseRateStr(z.Rate)
		if err != nil {
			return nil, fmt.Errorf("limit_req_zone %q: %w", name, err)
		}
		if z.Key == "" {
			return nil, fmt.Errorf("limit_req_zone %q: missing key", name)
		}
		h.LimitReqZones[name] = LimitReqZone{Key: z.Key, Rate: rate}
	}
	for name, key := range yh.LimitConnZones {
		h.LimitConnZones[name] = LimitConnZone{Key: key}
	}
	for name, dir := range yh.CachePaths {
		h.CachePaths[name] = dir
	}
	for _, ym := range yh.Maps {
		m, err := ym.convert()
		if err != nil {
			return nil, err
		}
		h.Maps = append(h.Maps, m)
	}
	for _, ys := range yh.Servers {
		s, err := ys.convert()
		if err != nil {
			return nil, err
		}
		h.Servers = append(h.Servers, s)
	}
	if len(h.Servers) == 0 {
		return nil, fmt.Errorf("http section has no servers")
	}
	return h, nil
}

func (ys *yamlServer) convert() (*Server, error) {
	s := &Server{ServerNames: ys.ServerName, SSLCert: ys.SSLCert, SSLKey: ys.SSLKey, ProxyCache: ys.ProxyCache, ValidReferers: ys.ValidReferers}
	for _, l := range ys.Listen {
		addr, err := normalizeListenAddr(l.Address)
		if err != nil {
			return nil, err
		}
		s.Listens = append(s.Listens, Listen{Addr: addr, Default: l.Default, SSL: l.SSL})
	}
	if len(s.Listens) == 0 {
		s.Listens = []Listen{{Addr: ":80"}}
	}
	if ys.AccessLog != nil {
		if ys.AccessLog.Off {
			s.AccessLog = "off"
		} else {
			s.AccessLog = ys.AccessLog.Path
			s.AccessLogFormat = ys.AccessLog.Format
		}
	}
	if err := applyCommonToServer(s, &ys.yamlCommon); err != nil {
		return nil, err
	}
	s.ServerTokens = ys.ServerTokens
	if len(ys.SSLProtocols) > 0 {
		if err := validateSSLProtocols(ys.SSLProtocols); err != nil {
			return nil, err
		}
		s.SSLProtocols = ys.SSLProtocols
	}
	for _, r := range ys.Rewrites {
		rw, err := r.convert()
		if err != nil {
			return nil, err
		}
		s.Rewrites = append(s.Rewrites, rw)
	}
	for _, cv := range ys.CacheValid {
		v, err := cv.convert()
		if err != nil {
			return nil, err
		}
		s.CacheValids = append(s.CacheValids, v)
	}
	for _, ep := range ys.ErrorPages {
		if ep.URI == "" || len(ep.Codes) == 0 {
			return nil, fmt.Errorf("error_pages entry needs codes and a uri")
		}
		s.ErrorPages = append(s.ErrorPages, ErrorPage{Codes: ep.Codes, URI: ep.URI})
	}
	if ys.LimitReq != nil {
		s.LimitReq = ys.LimitReq.convert()
	}
	if ys.LimitConn != nil {
		s.LimitConn = &LimitConnUse{Zone: ys.LimitConn.Zone, Max: ys.LimitConn.Max}
	}
	for _, yl := range ys.Locations {
		loc, err := yl.convert()
		if err != nil {
			return nil, err
		}
		s.Locations = append(s.Locations, loc)
	}
	return s, nil
}

func (yl *yamlLocation) convert() (*Location, error) {
	loc := &Location{
		Pattern: yl.Path, Alias: yl.Alias, ProxyPass: yl.ProxyPass,
		TryFiles: yl.TryFiles, ProxyCache: yl.ProxyCache,
	}
	switch yl.Match {
	case "", "prefix":
		loc.Modifier = ""
	case "exact":
		loc.Modifier = "="
	case "regex":
		loc.Modifier = "~"
	case "regex_i":
		loc.Modifier = "~*"
	case "prefix_priority":
		loc.Modifier = "^~"
	default:
		return nil, fmt.Errorf("location %q: unknown match type %q", yl.Path, yl.Match)
	}
	if yl.Path == "" {
		return nil, fmt.Errorf("location needs a path")
	}
	if yl.Return != nil {
		loc.ReturnCode = yl.Return.Code
		loc.ReturnArg = yl.Return.Body
	}
	for _, pr := range yl.ProxyRedirect {
		loc.ProxyRedirects = append(loc.ProxyRedirects, ProxyRedirect{From: pr.From, To: pr.To})
	}
	loc.HideHeaders = yl.HideHeaders
	loc.ValidReferers = yl.ValidReferers
	loc.PassHeaders = yl.PassHeaders
	for k, v := range yl.ProxySetHead {
		loc.ProxyHeaders = append(loc.ProxyHeaders, ProxyHeader{Name: k, Value: v})
	}
	for _, cp := range yl.ProxyCookie {
		loc.CookiePaths = append(loc.CookiePaths, CookiePath{From: cp.From, To: cp.To})
	}
	for _, r := range yl.Rewrites {
		rw, err := r.convert()
		if err != nil {
			return nil, err
		}
		loc.Rewrites = append(loc.Rewrites, rw)
	}
	for _, cv := range yl.CacheValid {
		v, err := cv.convert()
		if err != nil {
			return nil, err
		}
		loc.CacheValids = append(loc.CacheValids, v)
	}
	if yl.LimitReq != nil {
		loc.LimitReq = yl.LimitReq.convert()
	}
	if yl.LimitConn != nil {
		loc.LimitConn = &LimitConnUse{Zone: yl.LimitConn.Zone, Max: yl.LimitConn.Max}
	}
	if err := applyCommonToLocation(loc, &yl.yamlCommon); err != nil {
		return nil, err
	}
	return loc, nil
}

func (yu *yamlUpstream) convert(name string) (*Upstream, error) {
	up := &Upstream{Name: name, Keepalive: yu.Keepalive}
	switch yu.Balancer {
	case "", "round_robin":
	case "least_conn", "ip_hash":
		up.Balancer = yu.Balancer
	default:
		return nil, fmt.Errorf("upstream %q: unknown balancer %q", name, yu.Balancer)
	}
	for _, ys := range yu.Servers {
		if ys.Address == "" {
			return nil, fmt.Errorf("upstream %q: server needs an address", name)
		}
		addr := ys.Address
		if !strings.Contains(addr, ":") {
			addr += ":80"
		}
		us := UpstreamServer{Addr: addr, Weight: ys.Weight, MaxFails: ys.MaxFails, Backup: ys.Backup, Down: ys.Down}
		if us.Weight == 0 {
			us.Weight = 1
		}
		if ys.FailTimeout != "" {
			t, err := parseNginxTime(ys.FailTimeout)
			if err != nil {
				return nil, fmt.Errorf("upstream %q: %w", name, err)
			}
			us.FailTimeout = t
		}
		up.Servers = append(up.Servers, us)
	}
	if len(up.Servers) == 0 {
		return nil, fmt.Errorf("upstream %q has no servers", name)
	}
	return up, nil
}

func (ym *yamlMap) convert() (MapVar, error) {
	if !strings.HasPrefix(ym.Source, "$") || !strings.HasPrefix(ym.Target, "$") {
		return MapVar{}, fmt.Errorf("map source and target must be $variables")
	}
	m := MapVar{Source: ym.Source[1:], Target: ym.Target[1:]}
	for key, val := range ym.Cases {
		c := MapCase{Key: key, Value: val}
		switch {
		case key == "default":
			c.IsDefault = true
		case strings.HasPrefix(key, "~*"):
			c.IsRegex, c.CaseIns, c.Key = true, true, key[2:]
		case strings.HasPrefix(key, "~"):
			c.IsRegex, c.Key = true, key[1:]
		}
		m.Cases = append(m.Cases, c)
	}
	return m, nil
}

func (yr yamlRewrite) convert() (Rewrite, error) {
	if yr.Pattern == "" || yr.Replacement == "" {
		return Rewrite{}, fmt.Errorf("rewrite needs a pattern and a replacement")
	}
	switch yr.Flag {
	case "", "last", "break", "redirect", "permanent":
	default:
		return Rewrite{}, fmt.Errorf("rewrite: unknown flag %q", yr.Flag)
	}
	return Rewrite{Regex: yr.Pattern, Replacement: yr.Replacement, Flag: yr.Flag}, nil
}

func (cv yamlCacheValid) convert() (CacheValid, error) {
	if cv.Time == "" {
		return CacheValid{}, fmt.Errorf("proxy_cache_valid needs a time")
	}
	ttl, err := parseNginxTime(cv.Time)
	if err != nil {
		return CacheValid{}, err
	}
	v := CacheValid{TTL: ttl}
	switch {
	case cv.Any:
		v.Codes = []int{-1}
	case len(cv.Codes) > 0:
		v.Codes = cv.Codes
	default:
		v.Codes = []int{200, 301, 302}
	}
	return v, nil
}

func (lr *yamlLimitReq) convert() *LimitReqUse {
	return &LimitReqUse{Zone: lr.Zone, Burst: float64(lr.Burst), NoDelay: lr.NoDelay}
}

// applyCommon* map the shared directive block into each scope, converting
// size/timeout literals and allow/deny ordering.

func applyCommonToHTTP(h *HTTP, c *yamlCommon) error {
	h.Root, h.Index = c.Root, c.Index
	g, err := c.gzip()
	if err != nil {
		return err
	}
	h.Gzip = g
	h.AddHeaders = c.headers()
	h.Access = c.access()
	h.Auth = c.auth()
	h.Autoindex = c.Autoindex
	if h.Expires, err = c.expires(); err != nil {
		return err
	}
	if err = c.rateAndStatic(&h.LimitRate, &h.LimitRateAfter, &h.GzipStatic); err != nil {
		return err
	}
	return c.sizeAndTimeouts(&h.ClientMaxBody, &h.ProxyConnectTimeout, &h.ProxyReadTimeout, &h.ProxySendTimeout)
}

func applyCommonToServer(s *Server, c *yamlCommon) error {
	s.Root, s.Index = c.Root, c.Index
	g, err := c.gzip()
	if err != nil {
		return err
	}
	s.Gzip = g
	s.AddHeaders = c.headers()
	s.Access = c.access()
	s.Auth = c.auth()
	s.Autoindex = c.Autoindex
	if s.Expires, err = c.expires(); err != nil {
		return err
	}
	if err = c.rateAndStatic(&s.LimitRate, &s.LimitRateAfter, &s.GzipStatic); err != nil {
		return err
	}
	return c.sizeAndTimeouts(&s.ClientMaxBody, &s.ProxyConnectTimeout, &s.ProxyReadTimeout, &s.ProxySendTimeout)
}

func applyCommonToLocation(loc *Location, c *yamlCommon) error {
	if loc.Root == "" {
		loc.Root = c.Root
	}
	if len(loc.Index) == 0 {
		loc.Index = c.Index
	}
	g, err := c.gzip()
	if err != nil {
		return err
	}
	loc.Gzip = g
	loc.AddHeaders = c.headers()
	loc.Access = c.access()
	loc.Auth = c.auth()
	loc.Autoindex = c.Autoindex
	if loc.Expires, err = c.expires(); err != nil {
		return err
	}
	if err = c.rateAndStatic(&loc.LimitRate, &loc.LimitRateAfter, &loc.GzipStatic); err != nil {
		return err
	}
	return c.sizeAndTimeouts(&loc.ClientMaxBody, &loc.ProxyConnectTimeout, &loc.ProxyReadTimeout, &loc.ProxySendTimeout)
}

// rateAndStatic converts limit_rate / limit_rate_after / gzip_static.
func (c *yamlCommon) rateAndStatic(rate, after **int64, gzstatic **bool) error {
	if c.LimitRate != "" {
		n, err := parseSizeStr(c.LimitRate)
		if err != nil {
			return fmt.Errorf("limit_rate: %w", err)
		}
		*rate = &n
	}
	if c.LimitRateAfter != "" {
		n, err := parseSizeStr(c.LimitRateAfter)
		if err != nil {
			return fmt.Errorf("limit_rate_after: %w", err)
		}
		*after = &n
	}
	*gzstatic = c.GzipStatic
	return nil
}

func (c *yamlCommon) expires() (*Expires, error) {
	if c.Expires == "" {
		return nil, nil
	}
	e, err := parseExpiresValue(c.Expires)
	if err != nil {
		return nil, fmt.Errorf("expires: %w", err)
	}
	return e, nil
}

func (c *yamlCommon) gzip() (GzipSettings, error) {
	g := GzipSettings{}
	if c.Gzip == nil {
		return g, nil
	}
	g.On = c.Gzip.Enabled
	g.Types = c.Gzip.Types
	g.Level = c.Gzip.Level
	if c.Gzip.MinLength != "" {
		n, err := parseSizeStr(c.Gzip.MinLength)
		if err != nil {
			return g, fmt.Errorf("gzip min_length: %w", err)
		}
		g.MinLength = &n
	}
	return g, nil
}

func (c *yamlCommon) headers() []AddHeader {
	var out []AddHeader
	for _, h := range c.AddHeaders {
		out = append(out, AddHeader{Name: h.Name, Value: h.Value, Always: h.Always})
	}
	return out
}

func (c *yamlCommon) access() []AccessRule {
	var out []AccessRule
	for _, a := range c.Access {
		switch {
		case a.Allow != "":
			out = append(out, AccessRule{Allow: true, Cidr: a.Allow})
		case a.Deny != "":
			out = append(out, AccessRule{Allow: false, Cidr: a.Deny})
		}
	}
	return out
}

func (c *yamlCommon) auth() *AuthBasic {
	if c.Auth == nil {
		return nil
	}
	return &AuthBasic{Realm: c.Auth.Realm, UserFile: c.Auth.UserFile}
}

func (c *yamlCommon) sizeAndTimeouts(body **int64, connectT, readT, sendT **time.Duration) error {
	if c.ClientMaxBodySize != "" {
		n, err := parseSizeStr(c.ClientMaxBodySize)
		if err != nil {
			return fmt.Errorf("client_max_body_size: %w", err)
		}
		*body = &n
	}
	for _, tc := range []struct {
		name string
		val  string
		dst  **time.Duration
	}{
		{"proxy_connect_timeout", c.ProxyConnectTimeout, connectT},
		{"proxy_read_timeout", c.ProxyReadTimeout, readT},
		{"proxy_send_timeout", c.ProxySendTimeout, sendT},
	} {
		if tc.val == "" {
			continue
		}
		t, err := parseNginxTime(tc.val)
		if err != nil {
			return fmt.Errorf("%s: %w", tc.name, err)
		}
		*tc.dst = &t
	}
	return nil
}

func (ys *yamlStream) convert() (*Stream, error) {
	st := &Stream{Upstreams: map[string]*Upstream{}}
	for name, yu := range ys.Upstreams {
		up, err := yu.convert(name)
		if err != nil {
			return nil, err
		}
		st.Upstreams[name] = up
	}
	for _, yss := range ys.Servers {
		ss, err := yss.convert()
		if err != nil {
			return nil, err
		}
		st.Servers = append(st.Servers, ss)
	}
	if len(st.Servers) == 0 {
		return nil, fmt.Errorf("stream section has no servers")
	}
	return st, nil
}

func (yss *yamlStreamServer) convert() (*StreamServer, error) {
	if yss.Listen == "" {
		return nil, fmt.Errorf("stream server needs a listen address")
	}
	if yss.ProxyPass == "" {
		return nil, fmt.Errorf("stream server needs proxy_pass")
	}
	addr, err := normalizeListenAddr(yss.Listen)
	if err != nil {
		return nil, err
	}
	s := &StreamServer{
		Addr: addr, UDP: yss.UDP, ProxyPass: yss.ProxyPass,
		ProxyTimeout: 10 * time.Minute, ConnectTimeout: 60 * time.Second, ProxyResponses: -1,
	}
	if yss.ProxyTimeout != "" {
		if s.ProxyTimeout, err = parseNginxTime(yss.ProxyTimeout); err != nil {
			return nil, err
		}
	}
	if yss.ConnectTimeout != "" {
		if s.ConnectTimeout, err = parseNginxTime(yss.ConnectTimeout); err != nil {
			return nil, err
		}
	}
	if yss.ProxyResponses != nil {
		s.ProxyResponses = *yss.ProxyResponses
	}
	return s, nil
}

// normalizeListenAddr turns "8080", "*:8080" or "1.2.3.4:80" into a dial
// address, matching the nginx path's listen handling.
func normalizeListenAddr(addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("empty listen address")
	}
	switch {
	case strings.HasPrefix(addr, "*:"):
		return addr[1:], nil
	case !strings.Contains(addr, ":"):
		return ":" + addr, nil
	}
	return addr, nil
}
