package server

import (
	"net/http"
	"net/netip"
	"strings"

	"crest/internal/config"
	"crest/internal/vars"
)

const headerXFF = "X-Forwarded-For"

// realIP is a resolved set_real_ip_from / real_ip_header / real_ip_recursive
// policy, with the semantics of ngx_http_realip_module.
type realIP struct {
	from      []netip.Prefix
	header    string // canonical
	recursive bool
}

// resolveRealIP flattens the scopes, innermost first. nil means no proxy is
// trusted, which is the default.
func resolveRealIP(scopes ...config.RealIP) *realIP {
	var c config.RealIP
	for _, s := range scopes {
		c = c.Inherit(s)
	}
	if len(c.From) == 0 || c.Header == config.RealIPProxyProtocol {
		return nil
	}
	if c.Header == "" {
		c.Header = "X-Real-IP"
	}
	return &realIP{
		from:      c.From,
		header:    http.CanonicalHeaderKey(c.Header),
		recursive: boolOr(c.Recursive),
	}
}

// apply replaces r.RemoteAddr with the client address reported by a trusted
// proxy. Everything downstream (access log, limits, allow/deny, ip_hash, the
// proxied X-Real-IP / X-Forwarded-For, $remote_addr) reads that field. Like
// nginx, it happens at most once per request.
func (c *realIP) apply(r *http.Request) {
	if c == nil || vars.RealIPSet(r) {
		return
	}
	values := r.Header[c.header]
	if len(values) == 0 {
		return
	}
	if c.header != headerXFF {
		values = values[:1] // only X-Forwarded-For lines form one list
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return
	}
	if client, ok := c.client(peer.Addr(), values); ok {
		vars.SetRealIP(r, client.String())
	}
}

// client walks the reported addresses from the nearest hop outwards and stops
// at the first one that is not itself a trusted proxy (or after one step
// without real_ip_recursive). An unparsable entry or an empty line ends the
// walk at the last address found. The port is the reported one, or 0.
func (c *realIP) client(peer netip.Addr, values []string) (found netip.AddrPort, ok bool) {
	current := normalizeAddr(peer)
	for i := len(values) - 1; i >= 0; i-- {
		rest, token := lastToken(values[i])
		if token == "" {
			return found, ok
		}
		for ; token != ""; rest, token = lastToken(rest) {
			if !c.trusts(current) {
				return found, ok
			}
			ap, valid := parseForwardedAddr(token)
			if !valid {
				return found, ok
			}
			found, ok = ap, true
			current = ap.Addr()
			if !c.recursive {
				return found, ok
			}
		}
	}
	return found, ok
}

func (c *realIP) trusts(a netip.Addr) bool {
	for _, p := range c.from {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// lastToken splits the last address off a comma/space separated list.
func lastToken(s string) (rest, token string) {
	const sep = ", \t"
	s = strings.TrimRight(s, sep)
	i := strings.LastIndexAny(s, sep)
	return s[:i+1], s[i+1:]
}

// parseForwardedAddr accepts "ip", "ip:port" and "[ipv6]:port". IPv6 zones are
// rejected: they are meaningless off-link and may hold arbitrary text, which
// would end up in logs and upstream headers.
func parseForwardedAddr(s string) (netip.AddrPort, bool) {
	a, err := netip.ParseAddr(s)
	var port uint16
	if err != nil {
		ap, err := netip.ParseAddrPort(s)
		if err != nil || ap.Port() == 0 {
			return netip.AddrPort{}, false
		}
		a, port = ap.Addr(), ap.Port()
	}
	if a.Zone() != "" {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(normalizeAddr(a), port), true
}

// normalizeAddr gives each client one spelling, so limit keys and CIDR matches
// do not depend on an IPv4-mapped form or a zone.
func normalizeAddr(a netip.Addr) netip.Addr {
	return a.Unmap().WithZone("")
}

// wrapRealIP applies a location's own real_ip settings.
func wrapRealIP(h http.Handler, c *realIP) http.Handler {
	if c == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.apply(r)
		h.ServeHTTP(w, r)
	})
}
