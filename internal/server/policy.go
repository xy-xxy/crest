package server

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"gonx/internal/auth"
	"gonx/internal/config"
	"gonx/internal/logging"
	"gonx/internal/vars"
)

// pickHeaders implements nginx inheritance: a level with its own add_header
// directives replaces everything from outer levels.
func pickHeaders(h, s []config.AddHeader, l []config.AddHeader) []config.AddHeader {
	if len(l) > 0 {
		return l
	}
	if len(s) > 0 {
		return s
	}
	return h
}

type compiledHeader struct {
	name   string
	value  *vars.Template
	always bool
}

// addHeaderStatuses are the codes add_header applies to without "always".
var addHeaderStatuses = map[int]bool{
	200: true, 201: true, 204: true, 206: true,
	301: true, 302: true, 303: true, 304: true, 307: true, 308: true,
}

func wrapAddHeaders(h http.Handler, headers []config.AddHeader) http.Handler {
	if len(headers) == 0 {
		return h
	}
	compiled := make([]compiledHeader, 0, len(headers))
	for _, ah := range headers {
		compiled = append(compiled, compiledHeader{ah.Name, vars.Parse(ah.Value), ah.Always})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&headerAdder{ResponseWriter: w, headers: compiled, req: r}, r)
	})
}

type headerAdder struct {
	http.ResponseWriter
	headers []compiledHeader
	req     *http.Request
	done    bool
}

func (w *headerAdder) WriteHeader(code int) {
	if !w.done {
		w.done = true
		for _, ch := range w.headers {
			if ch.always || addHeaderStatuses[code] {
				w.Header().Add(ch.name, ch.value.Expand(vars.Request(w.req)))
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *headerAdder) Write(b []byte) (int, error) {
	if !w.done {
		w.WriteHeader(http.StatusOK)
		return w.ResponseWriter.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *headerAdder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passthrough keeps Upgrade (websocket) proxying working through an
// add_header wrapper.
func (w *headerAdder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

type accessRule struct {
	allow  bool
	all    bool
	prefix netip.Prefix
}

func compileAccess(rules []config.AccessRule) ([]accessRule, error) {
	out := make([]accessRule, 0, len(rules))
	for _, r := range rules {
		ar := accessRule{allow: r.Allow}
		switch {
		case r.Cidr == "all":
			ar.all = true
		case strings.Contains(r.Cidr, "/"):
			p, err := netip.ParsePrefix(r.Cidr)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q: %v", r.Cidr, err)
			}
			ar.prefix = p.Masked()
		default:
			a, err := netip.ParseAddr(r.Cidr)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q: %v", r.Cidr, err)
			}
			ar.prefix = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, ar)
	}
	return out, nil
}

func (i *Instance) wrapAccess(h http.Handler, rules []config.AccessRule) (http.Handler, error) {
	if len(rules) == 0 {
		return h, nil
	}
	compiled, err := compileAccess(rules)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, err := netip.ParseAddr(hostOnly(r.RemoteAddr))
		if err == nil {
			addr = addr.Unmap()
			for _, rule := range compiled {
				if rule.all || (rule.prefix.Addr().Is4() == addr.Is4() && rule.prefix.Contains(addr)) {
					if rule.allow {
						break
					}
					i.errLog.Logf(logging.LevelWarn, "access forbidden by rule, client %s", addr)
					http.Error(w, "403 Forbidden", http.StatusForbidden)
					return
				}
			}
		}
		h.ServeHTTP(w, r)
	}), nil
}

func (i *Instance) wrapAuth(h http.Handler, ab *config.AuthBasic) (http.Handler, error) {
	if ab == nil || ab.Realm == "" || ab.Realm == "off" {
		return h, nil
	}
	if ab.UserFile == "" {
		return nil, fmt.Errorf(`auth_basic %q has no auth_basic_user_file`, ab.Realm)
	}
	f, err := auth.Load(ab.UserFile)
	if err != nil {
		return nil, fmt.Errorf("auth_basic_user_file: %w", err)
	}
	challenge := fmt.Sprintf("Basic realm=%q", ab.Realm)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !f.Verify(user, pass) {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "401 Authorization Required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}), nil
}

// wrapExpires applies the expires directive: Cache-Control + Expires headers
// on cacheable statuses, like ngx_http_headers_module.
func wrapExpires(h http.Handler, e *config.Expires) http.Handler {
	if e == nil || e.Mode == "off" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&expiresWriter{ResponseWriter: w, e: e}, r)
	})
}

type expiresWriter struct {
	http.ResponseWriter
	e    *config.Expires
	done bool
}

func (w *expiresWriter) WriteHeader(code int) {
	if !w.done {
		w.done = true
		if addHeaderStatuses[code] {
			hd := w.Header()
			switch w.e.Mode {
			case "duration":
				hd.Set("Expires", time.Now().Add(w.e.Duration).UTC().Format(http.TimeFormat))
				hd.Set("Cache-Control", fmt.Sprintf("max-age=%d", int(w.e.Duration.Seconds())))
			case "nocache": // expires -1 等负值
				hd.Set("Expires", time.Now().Add(w.e.Duration).UTC().Format(http.TimeFormat))
				hd.Set("Cache-Control", "no-cache")
			case "epoch":
				hd.Set("Expires", "Thu, 01 Jan 1970 00:00:01 GMT")
				hd.Set("Cache-Control", "no-cache")
			case "max":
				hd.Set("Expires", "Thu, 31 Dec 2037 23:55:55 GMT")
				hd.Set("Cache-Control", "max-age=315360000")
			}
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *expiresWriter) Write(b []byte) (int, error) {
	if !w.done {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *expiresWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passthrough keeps Upgrade (websocket) proxying working through an
// expires wrapper.
func (w *expiresWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("hijacking not supported")
}

// refererCheck holds compiled valid_referers rules for one location/server.
type refererCheck struct {
	none        bool // "none" — no Referer header
	blocked     bool // "blocked" — Referer present but scheme stripped
	serverNames bool // "server_names" — match this server's names
	exact       map[string]bool
	wildPrefix  []string // ".example.com" form → suffix match on host
	wildSuffix  []string // "www." form → prefix match on host
	regexes     []*regexp.Regexp
	names       []string // the vhost's server_name values (for server_names)
}

func compileReferers(rules, serverNames []string) (*refererCheck, error) {
	rc := &refererCheck{exact: map[string]bool{}, names: serverNames}
	for _, r := range rules {
		switch {
		case r == "none":
			rc.none = true
		case r == "blocked":
			rc.blocked = true
		case r == "server_names":
			rc.serverNames = true
		case strings.HasPrefix(r, "~"):
			re, err := regexp.Compile(r[1:])
			if err != nil {
				return nil, fmt.Errorf("valid_referers: invalid regex %q: %v", r, err)
			}
			rc.regexes = append(rc.regexes, re)
		case strings.HasPrefix(r, "*."):
			rc.wildPrefix = append(rc.wildPrefix, r[1:]) // ".example.com"
		case strings.HasSuffix(r, ".*"):
			rc.wildSuffix = append(rc.wildSuffix, r[:len(r)-1]) // "www."
		default:
			rc.exact[strings.ToLower(r)] = true
		}
	}
	return rc, nil
}

// valid reports whether the request's Referer is allowed.
func (rc *refererCheck) valid(r *http.Request) bool {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return rc.none
	}
	rest := ref
	if s := strings.TrimPrefix(strings.TrimPrefix(ref, "https://"), "http://"); s != ref {
		rest = s
	} else {
		// no scheme → "blocked"
		return rc.blocked
	}
	host := rest
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(host)
	for _, re := range rc.regexes {
		if re.MatchString(ref) {
			return true
		}
	}
	if rc.exact[host] {
		return true
	}
	for _, suf := range rc.wildPrefix {
		if host == suf[1:] || strings.HasSuffix(host, suf) {
			return true
		}
	}
	for _, pre := range rc.wildSuffix {
		if strings.HasPrefix(host, pre) {
			return true
		}
	}
	if rc.serverNames {
		for _, n := range rc.names {
			if strings.EqualFold(strings.TrimPrefix(n, "*."), host) ||
				(strings.HasPrefix(n, "*.") && strings.HasSuffix(host, n[1:])) {
				return true
			}
		}
	}
	return false
}

// wrapValidReferers rejects requests whose Referer fails the rules with 403,
// the common anti-hotlinking use of valid_referers + $invalid_referer.
func (i *Instance) wrapValidReferers(h http.Handler, rules, serverNames []string) (http.Handler, error) {
	if len(rules) == 0 {
		return h, nil
	}
	rc, err := compileReferers(rules, serverNames)
	if err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rc.valid(r) {
			http.Error(w, "403 Forbidden", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	}), nil
}

// compiledMap resolves one http-level map block per request.
type compiledMap struct {
	src, dst string
	def      string
	exact    map[string]string
	regexes  []mapRegex
}

type mapRegex struct {
	re    *regexp.Regexp
	value string
}

func compileMaps(ms []config.MapVar) ([]compiledMap, error) {
	out := make([]compiledMap, 0, len(ms))
	for _, m := range ms {
		cm := compiledMap{src: m.Source, dst: m.Target, exact: map[string]string{}}
		for _, c := range m.Cases {
			switch {
			case c.IsDefault:
				cm.def = c.Value
			case c.IsRegex:
				pat := c.Key
				if c.CaseIns {
					pat = "(?i)" + pat
				}
				re, err := regexp.Compile(pat)
				if err != nil {
					return nil, fmt.Errorf("map (line %d): invalid regex %q: %v", m.Line, c.Key, err)
				}
				cm.regexes = append(cm.regexes, mapRegex{re, c.Value})
			default:
				cm.exact[c.Key] = c.Value
			}
		}
		out = append(out, cm)
	}
	return out, nil
}

// lookupMap resolves map target variables; ok=false means "not a map var".
func (i *Instance) lookupMap(name string, r *http.Request) (string, bool) {
	for _, m := range i.maps {
		if m.dst != name {
			continue
		}
		v := vars.Value(r, m.src)
		if val, ok := m.exact[v]; ok {
			return val, true
		}
		for _, mr := range m.regexes {
			if mr.re.MatchString(v) {
				return mr.value, true
			}
		}
		return m.def, true
	}
	return "", false
}
