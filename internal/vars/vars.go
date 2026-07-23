// Package vars evaluates nginx-style $variables against a request.
package vars

import (
	"context"
	"net"
	"net/http"
	"strings"
)

type ctxKey int

const (
	resolverKey ctxKey = iota
	upstreamKey
)

type upstreamSlot struct{ addr string }

// WithExtras attaches an optional per-request resolver (e.g. map variables)
// and a mutable slot for $upstream_addr to the request.
func WithExtras(r *http.Request, resolve func(string) (string, bool)) *http.Request {
	ctx := r.Context()
	if resolve != nil {
		ctx = context.WithValue(ctx, resolverKey, resolve)
	}
	ctx = context.WithValue(ctx, upstreamKey, &upstreamSlot{})
	return r.WithContext(ctx)
}

// RecordUpstream stores the backend address a proxied request went to.
func RecordUpstream(r *http.Request, addr string) {
	if s, ok := r.Context().Value(upstreamKey).(*upstreamSlot); ok {
		s.addr = addr
	}
}

// UpstreamAddr returns the recorded backend address, if any.
func UpstreamAddr(r *http.Request) string {
	if s, ok := r.Context().Value(upstreamKey).(*upstreamSlot); ok {
		return s.addr
	}
	return ""
}

// Value resolves one variable name (without the $) against a request.
// Unknown variables resolve to "", matching nginx's unset behavior.
func Value(r *http.Request, name string) string {
	if fn, ok := r.Context().Value(resolverKey).(func(string) (string, bool)); ok {
		if v, hit := fn(name); hit {
			return v
		}
	}
	switch name {
	case "upstream_addr":
		return UpstreamAddr(r)
	case "uri", "document_uri":
		return r.URL.Path
	case "request_uri":
		// server requests carry origin-form ("/x?y"); fall back for
		// client-style requests where RequestURI is an absolute URL
		if strings.HasPrefix(r.RequestURI, "/") {
			return r.RequestURI
		}
		uri := r.URL.Path
		if r.URL.RawQuery != "" {
			uri += "?" + r.URL.RawQuery
		}
		return uri
	case "args", "query_string":
		return r.URL.RawQuery
	case "is_args":
		if r.URL.RawQuery != "" {
			return "?"
		}
		return ""
	case "host":
		return strings.ToLower(hostOnly(r.Host))
	case "http_host":
		return r.Host
	case "remote_addr", "binary_remote_addr":
		return hostOnly(r.RemoteAddr)
	case "request_method":
		return r.Method
	case "scheme":
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case "server_protocol":
		return r.Proto
	case "proxy_add_x_forwarded_for":
		ip := hostOnly(r.RemoteAddr)
		if prior := r.Header.Get("X-Forwarded-For"); prior != "" {
			return prior + ", " + ip
		}
		return ip
	}
	switch {
	case strings.HasPrefix(name, "http_"):
		return r.Header.Get(strings.ReplaceAll(name[len("http_"):], "_", "-"))
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(name[len("arg_"):])
	case strings.HasPrefix(name, "cookie_"):
		if c, err := r.Cookie(name[len("cookie_"):]); err == nil {
			return c.Value
		}
	}
	return ""
}

// Request adapts a request into a resolver for Template.Expand.
func Request(r *http.Request) func(string) string {
	return func(name string) string { return Value(r, name) }
}

type seg struct {
	lit string
	v   string
}

// Template is a string with $var / ${var} references pre-split for expansion.
type Template struct {
	segs []seg
	raw  string
}

func Parse(s string) *Template {
	t := &Template{raw: s}
	var lit []byte
	i := 0
	for i < len(s) {
		if s[i] == '$' && i+1 < len(s) {
			name, next := scanName(s, i+1)
			if name != "" {
				if len(lit) > 0 {
					t.segs = append(t.segs, seg{lit: string(lit)})
					lit = nil
				}
				t.segs = append(t.segs, seg{v: name})
				i = next
				continue
			}
		}
		lit = append(lit, s[i])
		i++
	}
	if len(lit) > 0 {
		t.segs = append(t.segs, seg{lit: string(lit)})
	}
	return t
}

func scanName(s string, i int) (name string, next int) {
	if s[i] == '{' {
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			return "", i
		}
		return s[i+1 : i+j], i + j + 1
	}
	j := i
	for j < len(s) && isNameChar(s[j]) {
		j++
	}
	if j == i {
		return "", i
	}
	return s[i:j], j
}

func isNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func (t *Template) Expand(resolve func(string) string) string {
	var b strings.Builder
	for _, s := range t.segs {
		if s.v == "" {
			b.WriteString(s.lit)
		} else {
			b.WriteString(resolve(s.v))
		}
	}
	return b.String()
}

func (t *Template) Raw() string { return t.raw }

func hostOnly(hp string) string {
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return h
	}
	return hp
}
