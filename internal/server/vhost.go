package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"regexp"
	"strings"

	"gonx/internal/config"
	"gonx/internal/logging"
)

type vhost struct {
	cfg        *config.Server
	locs       *locationSet
	names      []nameMatcher
	accessLog  logging.AccessLogger
	accessSet  bool // server-level access_log present (incl. "off")
	rewrites   []*rewriteRule
	cert       *tls.Certificate
	errorPages map[int]string
}

type nameKind int

const (
	nameExact      nameKind = iota
	nameWildPrefix          // *.example.com or .example.com
	nameWildSuffix          // www.*
	nameRegex               // ~pattern
)

type nameMatcher struct {
	kind  nameKind
	value string // exact: full name; wildPrefix: ".example.com"; wildSuffix: "www."
	exact string // ".example.com" form also matches the bare domain
	re    *regexp.Regexp
}

func buildNameMatchers(names []string) ([]nameMatcher, error) {
	var ms []nameMatcher
	for _, n := range names {
		n = strings.ToLower(n)
		switch {
		case strings.HasPrefix(n, "~"):
			re, err := regexp.Compile(n[1:])
			if err != nil {
				return nil, fmt.Errorf("invalid server_name regex %q: %v", n, err)
			}
			ms = append(ms, nameMatcher{kind: nameRegex, re: re})
		case strings.HasPrefix(n, "*."):
			ms = append(ms, nameMatcher{kind: nameWildPrefix, value: n[1:]})
		case strings.HasPrefix(n, "."):
			ms = append(ms, nameMatcher{kind: nameWildPrefix, value: n, exact: n[1:]})
		case strings.HasSuffix(n, ".*"):
			ms = append(ms, nameMatcher{kind: nameWildSuffix, value: n[:len(n)-1]})
		default:
			ms = append(ms, nameMatcher{kind: nameExact, value: n})
		}
	}
	return ms, nil
}

type hostGroup struct {
	vhosts []*vhost
	def    *vhost
}

// match follows nginx server selection: exact name, longest leading-wildcard,
// longest trailing-wildcard, first matching regex, then the default server.
func (g *hostGroup) match(host string) *vhost {
	host = strings.ToLower(hostOnly(host))

	for _, v := range g.vhosts {
		for _, m := range v.names {
			if m.kind == nameExact && m.value == host {
				return v
			}
			if m.kind == nameWildPrefix && m.exact != "" && m.exact == host {
				return v
			}
		}
	}
	var best *vhost
	bestLen := 0
	for _, v := range g.vhosts {
		for _, m := range v.names {
			if m.kind == nameWildPrefix && strings.HasSuffix(host, m.value) && len(m.value) > bestLen {
				best, bestLen = v, len(m.value)
			}
		}
	}
	if best != nil {
		return best
	}
	best, bestLen = nil, 0
	for _, v := range g.vhosts {
		for _, m := range v.names {
			if m.kind == nameWildSuffix && strings.HasPrefix(host, m.value) && len(m.value) > bestLen {
				best, bestLen = v, len(m.value)
			}
		}
	}
	if best != nil {
		return best
	}
	for _, v := range g.vhosts {
		for _, m := range v.names {
			if m.kind == nameRegex && m.re.MatchString(host) {
				return v
			}
		}
	}
	return g.def
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
