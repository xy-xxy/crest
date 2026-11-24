package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// RealIPProxyProtocol is the real_ip_header value that takes the client
// address from the PROXY protocol instead of a request header.
const RealIPProxyProtocol = "proxy_protocol"

// RealIP holds one scope's ngx_http_realip_module directives. As in nginx,
// each of the three inherits from the enclosing scope on its own.
type RealIP struct {
	From      []netip.Prefix // set_real_ip_from; nil = inherit, empty = trust nobody
	Header    string         // real_ip_header; "" = inherit, default X-Real-IP
	Recursive *bool          // real_ip_recursive; nil = inherit, default off
}

// IsZero reports that the scope sets none of the directives.
func (r RealIP) IsZero() bool {
	return r.From == nil && r.Header == "" && r.Recursive == nil
}

// Inherit fills what r leaves unset from the enclosing scope.
func (r RealIP) Inherit(outer RealIP) RealIP {
	if r.From == nil {
		r.From = outer.From
	}
	if r.Header == "" {
		r.Header = outer.Header
	}
	if r.Recursive == nil {
		r.Recursive = outer.Recursive
	}
	return r
}

// parseRealIP applies one set_real_ip_from / real_ip_header /
// real_ip_recursive directive to a scope.
func parseRealIP(cfg *Config, r *RealIP, d *Directive) error {
	if d.Name == "real_ip_recursive" {
		b, err := parseOnOff(d)
		if err != nil {
			return err
		}
		r.Recursive = b
		return nil
	}
	if len(d.Args) != 1 {
		return errDirective(d, "needs exactly one argument")
	}
	arg := d.Args[0]
	if d.Name == "real_ip_header" {
		if err := validateRealIPHeader(arg); err != nil {
			return errDirective(d, err.Error())
		}
		if arg == RealIPProxyProtocol {
			// Ignoring the directive would fall back to X-Real-IP, which a
			// PROXY protocol balancer passes through from the client unchecked.
			cfg.warnf(d, "real_ip_header proxy_protocol is not supported; client addresses are not replaced in this scope")
		}
		r.Header = arg
		return nil
	}
	if r.From == nil {
		r.From = []netip.Prefix{}
	}
	if arg == "unix:" {
		cfg.warnf(d, "set_real_ip_from unix: has no effect, crest does not listen on unix sockets (ignored)")
		return nil
	}
	p, err := parseRealIPFrom(arg)
	if err != nil {
		return errDirective(d, err.Error())
	}
	r.From = append(r.From, p)
	return nil
}

// parseRealIPFrom parses a trusted proxy: an IP or a CIDR. nginx also resolves
// hostnames here; crest does not do DNS lookups while loading a config.
func parseRealIPFrom(s string) (netip.Prefix, error) {
	var p netip.Prefix
	if strings.Contains(s, "/") {
		if c, err := netip.ParsePrefix(s); err == nil {
			p = c.Masked()
		}
	} else if a, err := netip.ParseAddr(s); err == nil {
		p = netip.PrefixFrom(a, a.BitLen())
	}
	if !p.IsValid() {
		return p, fmt.Errorf("invalid address %q (need an IP or a CIDR)", s)
	}
	if p.Addr().Is4In6() {
		// Addresses are compared in plain IPv4 form, so this would trust nobody.
		return netip.Prefix{}, fmt.Errorf("IPv4-mapped address %q never matches, write it as plain IPv4", s)
	}
	return p, nil
}

// validateRealIPHeader rejects names that could never match a request header,
// such as a $variable: the policy would silently do nothing.
func validateRealIPHeader(name string) error {
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return fmt.Errorf("invalid header name %q", name)
		}
	}
	if name == "" {
		return fmt.Errorf("empty header name")
	}
	return nil
}

// realIP converts the YAML fields. An explicit empty set_real_ip_from list
// trusts nobody, overriding the enclosing scope.
func (c *yamlCommon) realIP() (RealIP, error) {
	r := RealIP{Header: c.RealIPHeader, Recursive: c.RealIPRecursive}
	if c.RealIPHeader != "" {
		if err := validateRealIPHeader(c.RealIPHeader); err != nil {
			return r, fmt.Errorf("real_ip_header: %w", err)
		}
		if c.RealIPHeader == RealIPProxyProtocol {
			return r, fmt.Errorf("real_ip_header: proxy_protocol is not supported")
		}
	}
	if c.SetRealIPFrom != nil {
		r.From = []netip.Prefix{}
	}
	for _, s := range c.SetRealIPFrom {
		p, err := parseRealIPFrom(s)
		if err != nil {
			return r, fmt.Errorf("set_real_ip_from: %w", err)
		}
		r.From = append(r.From, p)
	}
	return r, nil
}
