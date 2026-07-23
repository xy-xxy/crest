package server

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gonx/internal/cache"
	"gonx/internal/config"
	"gonx/internal/handler"
	"gonx/internal/limiter"
	"gonx/internal/logging"
	"gonx/internal/stream"
	"gonx/internal/upstream"
	"gonx/internal/vars"
)

// Instance is a fully wired gonx server: one AddrServer per listen address.
type Instance struct {
	Addrs []*AddrServer

	cfg           *config.Config
	opts          Options
	errLog        *logging.ErrorLog
	defaultAccess logging.AccessLogger
	accessLogs    map[string]*logging.AccessLog
	reqLimiters   map[string]*limiter.ReqLimiter
	reqZoneKeys   map[string]*vars.Template
	connLimiters  map[string]*limiter.ConnLimiter
	connZoneKeys  map[string]*vars.Template
	caches        map[string]*cache.Cache
	streams       []*stream.Server
	maps          []compiledMap

	started                             time.Time
	stTotal, st2xx, st3xx, st4xx, st5xx int64 // atomic request counters
}

// Stats is a snapshot of request counters since this instance started.
type Stats struct {
	Started time.Time
	Total   int64
	C2xx    int64 // includes 1xx
	C3xx    int64
	C4xx    int64
	C5xx    int64
}

func (i *Instance) Stats() Stats {
	return Stats{
		Started: i.started,
		Total:   atomic.LoadInt64(&i.stTotal),
		C2xx:    atomic.LoadInt64(&i.st2xx),
		C3xx:    atomic.LoadInt64(&i.st3xx),
		C4xx:    atomic.LoadInt64(&i.st4xx),
		C5xx:    atomic.LoadInt64(&i.st5xx),
	}
}

// StreamAddrs lists the L4 listener addresses for status display.
func (i *Instance) StreamAddrs() []string {
	out := make([]string, 0, len(i.streams))
	for _, s := range i.streams {
		out = append(out, s.Addr())
	}
	return out
}

// AddrServer dispatches requests on one listen address to its virtual hosts.
type AddrServer struct {
	Addr    string
	group   *hostGroup
	inst    *Instance
	http    *http.Server
	tlsConf *tls.Config
}

// Options configures runtime log routing. When set and the config does not
// specify its own paths, gonx splits access records into success/fail logs
// and writes process/error messages to Console.
type Options struct {
	// LogDir is the directory for the default success.log / fail.log split.
	// Empty disables the split (access logs go to stdout as before).
	LogDir string
	// Console receives process messages and the error log (typically a tee of
	// stderr and console.log). Nil keeps the error log on stderr.
	Console io.Writer
}

func New(cfg *config.Config) (*Instance, error) {
	return NewWithOptions(cfg, Options{})
}

func NewWithOptions(cfg *config.Config, opts Options) (*Instance, error) {
	h := cfg.HTTP
	errPath, errLevel := "", "error"
	if h != nil {
		errPath, errLevel = h.ErrorLog, h.ErrorLevel
	}
	level, ok := logging.ParseLevel(errLevel)
	if !ok {
		level = logging.LevelError
	}
	var errLog *logging.ErrorLog
	var err error
	if errPath == "" && opts.Console != nil {
		errLog = logging.OpenErrorWriter(opts.Console, level)
	} else if errLog, err = logging.OpenError(errPath, level); err != nil {
		return nil, err
	}
	inst := &Instance{cfg: cfg, opts: opts, errLog: errLog, accessLogs: map[string]*logging.AccessLog{}, started: time.Now()}
	if h != nil {
		if _, err := inst.setupHTTP(h); err != nil {
			return nil, err
		}
	}
	if cfg.Stream != nil {
		if err := inst.setupStream(cfg.Stream); err != nil {
			return nil, err
		}
	}
	return inst, nil
}

func (inst *Instance) setupHTTP(h *config.HTTP) (*Instance, error) {
	var err error
	inst.maps, err = compileMaps(h.Maps)
	if err != nil {
		return nil, err
	}
	if h.AccessLog != "off" {
		if h.AccessLog == "" && inst.opts.LogDir != "" {
			// no explicit access_log: split into success.log / error.log
			format := inst.cfg.HTTP.LogFormats[h.AccessLogFormat]
			sal, alErr := logging.OpenSplit(
				filepath.Join(inst.opts.LogDir, "success.log"),
				filepath.Join(inst.opts.LogDir, "error.log"),
				format)
			if alErr != nil {
				return nil, alErr
			}
			inst.defaultAccess = sal
		} else {
			al, alErr := inst.openAccessLog(h.AccessLog, h.AccessLogFormat)
			if alErr != nil {
				return nil, alErr
			}
			inst.defaultAccess = al
		}
	}

	pools := buildPools(h.Upstreams)

	inst.reqLimiters = map[string]*limiter.ReqLimiter{}
	inst.reqZoneKeys = map[string]*vars.Template{}
	for name, z := range h.LimitReqZones {
		inst.reqLimiters[name] = limiter.NewReq(z.Rate)
		inst.reqZoneKeys[name] = vars.Parse(z.Key)
	}
	inst.connLimiters = map[string]*limiter.ConnLimiter{}
	inst.connZoneKeys = map[string]*vars.Template{}
	for name, z := range h.LimitConnZones {
		inst.connLimiters[name] = limiter.NewConn()
		inst.connZoneKeys[name] = vars.Parse(z.Key)
	}
	inst.caches = map[string]*cache.Cache{}
	for name, dir := range h.CachePaths {
		c, err := cache.Open(dir)
		if err != nil {
			return nil, fmt.Errorf("proxy_cache_path %q: %w", dir, err)
		}
		inst.caches[name] = c
	}
	compiledMaps, err := compileMaps(h.Maps)
	if err != nil {
		return nil, err
	}
	inst.maps = compiledMaps

	groups := map[string]*hostGroup{}
	sslAddr := map[string]bool{}
	for _, sv := range h.Servers {
		v, err := inst.buildVHost(sv, pools)
		if err != nil {
			return nil, fmt.Errorf("server (line %d): %w", sv.Line, err)
		}
		for _, ln := range sv.Listens {
			if prev, seen := sslAddr[ln.Addr]; seen && prev != ln.SSL {
				return nil, fmt.Errorf("listen %s: mixing ssl and plain listeners on one address is not supported", ln.Addr)
			}
			sslAddr[ln.Addr] = ln.SSL
			g := groups[ln.Addr]
			if g == nil {
				g = &hostGroup{}
				groups[ln.Addr] = g
			}
			g.vhosts = append(g.vhosts, v)
			if ln.Default && g.def == nil {
				g.def = v
			}
		}
	}
	addrs := make([]string, 0, len(groups))
	for addr := range groups {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)
	for _, addr := range addrs {
		g := groups[addr]
		if g.def == nil {
			g.def = g.vhosts[0]
		}
		a := &AddrServer{Addr: addr, group: g, inst: inst}
		if sslAddr[addr] {
			for _, v := range g.vhosts {
				if v.cert == nil {
					return nil, fmt.Errorf("listen %s: server (line %d) has no ssl_certificate", addr, v.cfg.Line)
				}
			}
			a.tlsConf = &tls.Config{
				GetCertificate: func(chi *tls.ClientHelloInfo) (*tls.Certificate, error) {
					if v := g.match(chi.ServerName); v != nil && v.cert != nil {
						return v.cert, nil
					}
					return g.def.cert, nil
				},
			}
		}
		inst.Addrs = append(inst.Addrs, a)
	}
	return inst, nil
}

func (i *Instance) buildVHost(sv *config.Server, pools map[string]upstream.Pool) (*vhost, error) {
	h := i.cfg.HTTP
	root := sv.Root
	if root == "" {
		root = h.Root
	}
	index := sv.Index
	if len(index) == 0 {
		index = h.Index
	}
	if len(index) == 0 {
		index = []string{"index.html"}
	}

	var routes []*route
	named := map[string]*route{}
	hasPlainRoot := false
	for _, loc := range sv.Locations {
		rt, err := i.buildRoute(loc, sv, pools, root, index)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(loc.Pattern, "@") {
			named[loc.Pattern] = rt
			continue
		}
		if loc.Modifier == "" && loc.Pattern == "/" {
			hasPlainRoot = true
		}
		routes = append(routes, rt)
	}
	if !hasPlainRoot {
		// nginx serves unmatched requests from the server context
		st := &handler.Static{Root: root, Prefix: "/", Index: index, Autoindex: boolOr(sv.Autoindex, i.cfg.HTTP.Autoindex), ErrLog: i.errLog}
		var hd http.Handler = handler.WrapGzip(st, resolveGzip(i.cfg.HTTP.Gzip, sv.Gzip, config.GzipSettings{}))
		hd = wrapAddHeaders(hd, pickHeaders(i.cfg.HTTP.AddHeaders, sv.AddHeaders, nil))
		hd, err := i.wrapLimits(hd, sv, nil, nil)
		if err != nil {
			return nil, err
		}
		if n := resolveMaxBody(i.cfg.HTTP, sv, nil); n > 0 {
			hd = wrapBodyLimit(hd, n)
		}
		if hd, err = i.applyPolicies(hd, sv, nil); err != nil {
			return nil, err
		}
		routes = append(routes, &route{pattern: "/", static: st, handler: hd})
	}
	locs, err := newLocationSet(routes)
	if err != nil {
		return nil, err
	}
	locs.named = named
	names, err := buildNameMatchers(sv.ServerNames)
	if err != nil {
		return nil, err
	}
	v := &vhost{cfg: sv, locs: locs, names: names}
	v.rewrites, err = compileRewrites(sv.Rewrites)
	if err != nil {
		return nil, err
	}
	if len(sv.ErrorPages) > 0 {
		v.errorPages = map[int]string{}
		for _, ep := range sv.ErrorPages {
			for _, c := range ep.Codes {
				v.errorPages[c] = ep.URI
			}
		}
	}
	if sv.SSLCert != "" || sv.SSLKey != "" {
		if sv.SSLCert == "" || sv.SSLKey == "" {
			return nil, fmt.Errorf("both ssl_certificate and ssl_certificate_key are required")
		}
		cert, err := tls.LoadX509KeyPair(sv.SSLCert, sv.SSLKey)
		if err != nil {
			return nil, fmt.Errorf("loading ssl certificate: %w", err)
		}
		v.cert = &cert
	}
	switch sv.AccessLog {
	case "":
	case "off":
		v.accessSet = true
	default:
		format := sv.AccessLogFormat
		if format == "" {
			format = h.AccessLogFormat
		}
		al, err := i.openAccessLog(sv.AccessLog, format)
		if err != nil {
			return nil, err
		}
		v.accessSet = true
		v.accessLog = al
	}
	return v, nil
}

// buildRoute assembles one location's full middleware chain.
func (i *Instance) buildRoute(loc *config.Location, sv *config.Server, pools map[string]upstream.Pool, root string, index []string) (*route, error) {
	hd, st, err := i.buildHandler(loc, sv, pools, root, index)
	if err != nil {
		return nil, fmt.Errorf("location %q (line %d): %w", loc.Pattern, loc.Line, err)
	}
	rt := &route{pattern: loc.Pattern, modifier: loc.Modifier, static: st, isProxy: loc.ProxyPass != ""}
	rt.rewrites, err = compileRewrites(loc.Rewrites)
	if err != nil {
		return nil, fmt.Errorf("location %q: %w", loc.Pattern, err)
	}
	if len(loc.TryFiles) > 0 {
		if st == nil {
			i.errLog.Logf(logging.LevelWarn,
				"location %q (line %d): try_files needs a static context, ignored", loc.Pattern, loc.Line)
		} else {
			for _, tf := range loc.TryFiles {
				rt.tryFiles = append(rt.tryFiles, vars.Parse(tf))
			}
		}
	}
	if loc.ProxyPass != "" {
		name := loc.ProxyCache
		if name == "" {
			name = sv.ProxyCache
		}
		if name != "" && name != "off" {
			c := i.caches[name]
			if c == nil {
				return nil, fmt.Errorf("location %q: unknown proxy_cache zone %q", loc.Pattern, name)
			}
			valids := loc.CacheValids
			if len(valids) == 0 {
				valids = sv.CacheValids
			}
			hd = handler.WrapCache(hd, c, convertValids(valids))
		}
	}
	hd = handler.WrapGzip(hd, resolveGzip(i.cfg.HTTP.Gzip, sv.Gzip, loc.Gzip))
	hd = wrapAddHeaders(hd, pickHeaders(i.cfg.HTTP.AddHeaders, sv.AddHeaders, loc.AddHeaders))
	hd, err = i.wrapLimits(hd, sv, loc.LimitReq, loc.LimitConn)
	if err != nil {
		return nil, fmt.Errorf("location %q: %w", loc.Pattern, err)
	}
	if n := resolveMaxBody(i.cfg.HTTP, sv, loc); n > 0 {
		hd = wrapBodyLimit(hd, n)
	}
	if hd, err = i.applyPolicies(hd, sv, loc); err != nil {
		return nil, fmt.Errorf("location %q: %w", loc.Pattern, err)
	}
	rt.handler = hd
	return rt, nil
}

// applyPolicies applies auth_basic and allow/deny, resolving location-over-
// server inheritance. These run outermost so denied requests skip the body.
func (i *Instance) applyPolicies(h http.Handler, sv *config.Server, loc *config.Location) (http.Handler, error) {
	auth := sv.Auth
	access := sv.Access
	if loc != nil {
		if loc.Auth != nil {
			auth = loc.Auth
		}
		if len(loc.Access) > 0 {
			access = loc.Access
		}
	}
	var err error
	if h, err = i.wrapAuth(h, auth); err != nil {
		return nil, err
	}
	if h, err = i.wrapAccess(h, access); err != nil {
		return nil, err
	}
	return h, nil
}

func (i *Instance) buildHandler(loc *config.Location, sv *config.Server, pools map[string]upstream.Pool, root string, index []string) (http.Handler, *handler.Static, error) {
	if loc.ReturnCode != 0 {
		var arg *vars.Template
		if loc.ReturnArg != "" {
			arg = vars.Parse(loc.ReturnArg)
		}
		return &returnHandler{code: loc.ReturnCode, arg: arg}, nil, nil
	}
	if loc.ProxyPass != "" {
		prefix := ""
		if loc.Modifier == "" || loc.Modifier == "^~" || loc.Modifier == "=" {
			prefix = loc.Pattern
		}
		headers := make([]handler.HeaderPair, 0, len(loc.ProxyHeaders))
		for _, ph := range loc.ProxyHeaders {
			headers = append(headers, handler.HeaderPair{Name: ph.Name, Value: vars.Parse(ph.Value)})
		}
		cookies := make([]handler.CookieRewrite, 0, len(loc.CookiePaths))
		for _, cp := range loc.CookiePaths {
			cookies = append(cookies, handler.CookieRewrite{From: cp.From, To: cp.To})
		}
		opts := handler.ProxyOptions{Prefix: prefix, Headers: headers, Cookies: cookies}
		if t := durOr(loc.ProxyConnectTimeout, sv.ProxyConnectTimeout, i.cfg.HTTP.ProxyConnectTimeout); t > 0 {
			opts.ConnectTimeout = t
		}
		if t := durOr(loc.ProxyReadTimeout, sv.ProxyReadTimeout, i.cfg.HTTP.ProxyReadTimeout); t > 0 {
			opts.ReadTimeout = t
		}
		h, err := handler.NewProxy(loc.ProxyPass, opts, pools, i.errLog)
		return h, nil, err
	}
	r := loc.Root
	if r == "" {
		r = root
	}
	idx := loc.Index
	if len(idx) == 0 {
		idx = index
	}
	ai := boolOr(loc.Autoindex, sv.Autoindex, i.cfg.HTTP.Autoindex)
	st := &handler.Static{Root: r, Alias: loc.Alias, Prefix: loc.Pattern, Index: idx, Autoindex: ai, ErrLog: i.errLog}
	return st, st, nil
}

func boolOr(vals ...*bool) bool {
	for _, v := range vals {
		if v != nil {
			return *v
		}
	}
	return false
}

func durOr(vals ...*time.Duration) time.Duration {
	for _, v := range vals {
		if v != nil {
			return *v
		}
	}
	return 0
}

func resolveMaxBody(h *config.HTTP, sv *config.Server, loc *config.Location) int64 {
	v := h.ClientMaxBody
	if sv.ClientMaxBody != nil {
		v = sv.ClientMaxBody
	}
	if loc != nil && loc.ClientMaxBody != nil {
		v = loc.ClientMaxBody
	}
	if v == nil {
		return 0 // 0 = unlimited, like nginx
	}
	return *v
}

func wrapBodyLimit(h http.Handler, max int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > max {
			http.Error(w, "413 Request Entity Too Large", http.StatusRequestEntityTooLarge)
			return
		}
		// chunked bodies have no Content-Length; enforce while reading
		r.Body = http.MaxBytesReader(w, r.Body, max)
		h.ServeHTTP(w, r)
	})
}

func buildPools(ups map[string]*config.Upstream) map[string]upstream.Pool {
	pools := map[string]upstream.Pool{}
	for name, up := range ups {
		bs := make([]*upstream.Backend, 0, len(up.Servers))
		for _, s := range up.Servers {
			bs = append(bs, &upstream.Backend{
				Addr: s.Addr, Weight: s.Weight, MaxFails: s.MaxFails,
				FailTimeout: s.FailTimeout, Backup: s.Backup, Down: s.Down,
			})
		}
		switch up.Balancer {
		case "least_conn":
			pools[name] = upstream.NewLeastConn(bs)
		case "ip_hash":
			pools[name] = upstream.NewIPHash(bs)
		default:
			pools[name] = upstream.NewRoundRobin(bs)
		}
	}
	return pools
}

func (inst *Instance) setupStream(st *config.Stream) error {
	pools := buildPools(st.Upstreams)
	for _, ss := range st.Servers {
		srv, err := stream.NewServer(ss, pools, inst.errLog)
		if err != nil {
			return err
		}
		inst.streams = append(inst.streams, srv)
	}
	return nil
}

// wrapLimits applies limit_req and limit_conn, letting a location override
// its server's setting like nginx directive inheritance.
func (i *Instance) wrapLimits(h http.Handler, sv *config.Server, lr *config.LimitReqUse, lc *config.LimitConnUse) (http.Handler, error) {
	if lr == nil {
		lr = sv.LimitReq
	}
	if lc == nil {
		lc = sv.LimitConn
	}
	var err error
	if lr != nil {
		if h, err = i.wrapLimitReq(h, lr); err != nil {
			return nil, err
		}
	}
	if lc != nil {
		if h, err = i.wrapLimitConn(h, lc); err != nil {
			return nil, err
		}
	}
	return h, nil
}

func convertValids(vs []config.CacheValid) []handler.CacheValid {
	out := make([]handler.CacheValid, 0, len(vs))
	for _, v := range vs {
		codes := map[int]bool{}
		for _, c := range v.Codes {
			codes[c] = true
		}
		out = append(out, handler.CacheValid{Codes: codes, TTL: v.TTL})
	}
	return out
}

// resolveGzip merges http < server < location settings; nil means disabled.
func resolveGzip(h, s, l config.GzipSettings) *handler.GzipConfig {
	on := false
	for _, g := range []config.GzipSettings{h, s, l} {
		if g.On != nil {
			on = *g.On
		}
	}
	if !on {
		return nil
	}
	cfg := &handler.GzipConfig{MinLength: 20, Level: gzip.DefaultCompression, Types: map[string]bool{}}
	types := h.Types
	if len(s.Types) > 0 {
		types = s.Types
	}
	if len(l.Types) > 0 {
		types = l.Types
	}
	for _, t := range types {
		cfg.Types[t] = true
	}
	for _, g := range []config.GzipSettings{h, s, l} {
		if g.MinLength != nil {
			cfg.MinLength = *g.MinLength
		}
		if g.Level != nil {
			cfg.Level = *g.Level
		}
	}
	return cfg
}

func (i *Instance) openAccessLog(path, formatName string) (*logging.AccessLog, error) {
	format, ok := i.cfg.HTTP.LogFormats[formatName]
	if !ok {
		return nil, fmt.Errorf("unknown log_format %q", formatName)
	}
	key := path + "\x00" + formatName
	if al, ok := i.accessLogs[key]; ok {
		return al, nil
	}
	al, err := logging.OpenAccess(path, format)
	if err != nil {
		return nil, err
	}
	i.accessLogs[key] = al
	return al, nil
}

func (a *AddrServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &respWriter{ResponseWriter: w}
	if len(a.inst.maps) > 0 {
		r = vars.WithExtras(r, func(name string) (string, bool) { return a.inst.lookupMap(name, r) })
	} else {
		r = vars.WithExtras(r, nil)
	}
	v := a.group.match(r.Host)
	switch {
	case v == nil:
		http.NotFound(rw, r)
	case len(v.errorPages) > 0:
		ew := &errorPageWriter{ResponseWriter: rw, pages: v.errorPages}
		a.dispatch(v, ew, r)
		if code := ew.intercepted; code != 0 {
			a.serveErrorPage(v, rw, r, code)
		}
	default:
		a.dispatch(v, rw, r)
	}

	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	atomic.AddInt64(&a.inst.stTotal, 1)
	switch {
	case status < 300:
		atomic.AddInt64(&a.inst.st2xx, 1)
	case status < 400:
		atomic.AddInt64(&a.inst.st3xx, 1)
	case status < 500:
		atomic.AddInt64(&a.inst.st4xx, 1)
	default:
		atomic.AddInt64(&a.inst.st5xx, 1)
	}
	al := a.inst.defaultAccess
	if v != nil && v.accessSet {
		al = v.accessLog
	}
	if al == nil {
		return // access logging disabled at this scope
	}
	user := ""
	if u, _, ok := r.BasicAuth(); ok {
		user = u
	}
	al.Log(logging.AccessRecord{
		RemoteAddr:   hostOnly(r.RemoteAddr),
		User:         user,
		Time:         time.Now(),
		Request:      r.Method + " " + r.RequestURI + " " + r.Proto,
		Status:       status,
		BodyBytes:    rw.bytes,
		Referer:      r.Referer(),
		UserAgent:    r.UserAgent(),
		Host:         hostOnly(r.Host),
		Duration:     time.Since(start),
		UpstreamAddr: vars.UpstreamAddr(r),
		Extra: func(name string) (string, bool) {
			if len(a.inst.maps) > 0 {
				if v, ok := a.inst.lookupMap(name, r); ok {
					return v, true
				}
			}
			// other request-side vars ($arg_*, $http_*, $uri, …)
			return vars.Value(r, name), true
		},
	})
}

// dispatch runs the nginx request phases: server rewrites, then a bounded
// loop of location match → location rewrites → try_files → handler, where
// "last" rewrites and try_files fallbacks re-enter location matching.
func (a *AddrServer) dispatch(v *vhost, w http.ResponseWriter, r *http.Request) {
	if applyRewrites(v.rewrites, w, r) == rwDone {
		return
	}
	for cycle := 0; cycle < 10; cycle++ {
		rt := v.locs.match(r.URL.Path)
		if rt == nil {
			http.NotFound(w, r)
			return
		}
		if ew, ok := w.(*errorPageWriter); ok {
			ew.proxyRoute = rt.isProxy
		}
		switch applyRewrites(rt.rewrites, w, r) {
		case rwDone:
			return
		case rwChanged:
			continue
		}
		if len(rt.tryFiles) > 0 {
			uri, code, redo, named := rt.resolveTryFiles(r)
			if named != "" {
				if nr := v.locs.named[named]; nr != nil {
					nr.handler.ServeHTTP(w, r)
					return
				}
				a.inst.errLog.Logf(logging.LevelError, "try_files references unknown named location %q", named)
				http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
				return
			}
			if code != 0 {
				http.Error(w, fmt.Sprintf("%d %s", code, http.StatusText(code)), code)
				return
			}
			r.URL.Path = uri
			if redo {
				continue
			}
		}
		rt.handler.ServeHTTP(w, r)
		return
	}
	a.inst.errLog.Logf(logging.LevelError, "rewrite or internal redirection cycle for %q", r.RequestURI)
	http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
}

// serveErrorPage internally redirects an intercepted error to its
// error_page URI, forcing the original status code onto the response.
func (a *AddrServer) serveErrorPage(v *vhost, w http.ResponseWriter, r *http.Request, code int) {
	uri := v.errorPages[code]
	r2 := r.Clone(r.Context())
	r2.URL.Path = uri
	r2.URL.RawQuery = ""
	r2.RequestURI = uri
	sw := &statusOverrideWriter{ResponseWriter: w, code: code}
	if rt := v.locs.match(uri); rt != nil {
		rt.handler.ServeHTTP(sw, r2)
		return
	}
	http.Error(w, fmt.Sprintf("%d %s", code, http.StatusText(code)), code)
}

// Start launches all listeners; fatal listen errors arrive on the channel.
func (i *Instance) Start() <-chan error {
	errc := make(chan error, len(i.Addrs)+len(i.streams))
	for _, st := range i.streams {
		if err := st.Start(); err != nil {
			errc <- fmt.Errorf("stream listen %s: %w", st.Addr(), err)
		}
	}
	for _, a := range i.Addrs {
		a.http = &http.Server{Addr: a.Addr, Handler: a, TLSConfig: a.tlsConf}
		// Bind IPv4-only by default (like nginx's "listen <port>"), so a
		// loopback client does not arrive as ::1 and get forwarded upstream
		// as an X-Real-IP that IPv4-only code cannot parse. An explicit IPv6
		// literal (e.g. "[::]:80") still binds IPv6.
		network := listenNetwork(a.Addr)
		ln, err := net.Listen(network, a.Addr)
		if err != nil {
			errc <- fmt.Errorf("listen %s: %w", a.Addr, err)
			continue
		}
		go func(a *AddrServer, ln net.Listener) {
			var err error
			if a.tlsConf != nil {
				i.errLog.Logf(logging.LevelInfo, "listening on %s (ssl)", a.Addr)
				err = a.http.ServeTLS(ln, "", "")
			} else {
				i.errLog.Logf(logging.LevelInfo, "listening on %s", a.Addr)
				err = a.http.Serve(ln)
			}
			if err != nil && err != http.ErrServerClosed {
				errc <- fmt.Errorf("listen %s: %w", a.Addr, err)
			}
		}(a, ln)
	}
	return errc
}

// listenNetwork chooses the socket family for a listen address. A bare port
// (":80"), an unspecified host, or an IPv4 literal binds IPv4-only, matching
// nginx's default; an explicit IPv6 literal binds IPv6.
func listenNetwork(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "tcp4"
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "tcp6"
	}
	return "tcp4"
}

// Reopen reopens all log files for rotation, like nginx's reopen signal.
func (i *Instance) Reopen() error {
	var first error
	set := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	set(i.errLog.Reopen())
	if i.defaultAccess != nil {
		set(i.defaultAccess.Reopen())
	}
	for _, al := range i.accessLogs {
		set(al.Reopen())
	}
	return first
}

func (i *Instance) Shutdown(ctx context.Context) error {
	var first error
	for _, st := range i.streams {
		st.Close()
	}
	for _, a := range i.Addrs {
		if a.http == nil {
			continue
		}
		if err := a.http.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	for _, al := range i.accessLogs {
		al.Close()
	}
	if i.defaultAccess != nil {
		// the split log is not in accessLogs; closing a cached single log
		// twice is harmless
		i.defaultAccess.Close()
	}
	i.errLog.Close()
	return first
}
