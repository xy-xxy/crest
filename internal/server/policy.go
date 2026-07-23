package server

import (
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"

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
