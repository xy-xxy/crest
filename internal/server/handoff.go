package server

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"

	"crest/internal/cache"
	"crest/internal/config"
	"crest/internal/limiter"
	"crest/internal/stream"
)

// socketKey identifies a listening socket by family ("tcp4", "udp6" …) and its
// canonical address. A reload hands a socket to the next Instance when both
// configs listen on the same key.
type socketKey struct{ network, addr string }

// addrKey canonicalizes addr so spellings of one socket share a key: ":80",
// "*:80" and "0.0.0.0:80" on tcp4, or "[::0]:80" and "[::]:80" on tcp6.
// Hostnames are compared as written.
func addrKey(network, addr string) socketKey {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return socketKey{network, addr}
	}
	switch ip := net.ParseIP(host); {
	case host == "" || host == "*":
		host = "0.0.0.0"
		if strings.HasSuffix(network, "6") {
			host = "::"
		}
	case ip != nil:
		host = ip.String()
	}
	return socketKey{network, net.JoinHostPort(host, port)}
}

func httpKey(a *AddrServer) socketKey {
	// IPv4-only by default (like nginx's "listen <port>"), so a loopback
	// client does not arrive as ::1 and get forwarded upstream as an
	// X-Real-IP that IPv4-only code cannot parse. An explicit IPv6 literal
	// (e.g. "[::]:80") still binds IPv6.
	return addrKey(listenNetwork(a.Addr), a.Addr)
}

func streamKey(st *stream.Server) socketKey { return addrKey(st.Network(), st.Addr()) }

// boundSocket is a listening socket owned by exactly one Instance at a time.
type boundSocket struct {
	ln *queuedListener // TCP: HTTP or stream
	pc net.PacketConn  // UDP stream
}

func bindSocket(k socketKey) (*boundSocket, error) {
	if strings.HasPrefix(k.network, "udp") {
		pc, err := net.ListenPacket(k.network, k.addr)
		if err != nil {
			return nil, err
		}
		return &boundSocket{pc: pc}, nil
	}
	ln, err := net.Listen(k.network, k.addr)
	if err != nil {
		return nil, err
	}
	return &boundSocket{ln: queueListener(ln, acceptQueueSize)}, nil
}

func (b *boundSocket) close() {
	if b.ln != nil {
		b.ln.Close()
	}
	if b.pc != nil {
		b.pc.Close()
	}
}

type socketNeed struct {
	key   socketKey
	label string // error prefix: "listen" or "stream listen"
	addr  string // as configured, for messages
}

func (n socketNeed) errorf(format string, args ...interface{}) error {
	return fmt.Errorf("%s %s: "+format, append([]interface{}{n.label, n.addr}, args...)...)
}

func (i *Instance) socketNeeds() ([]socketNeed, error) {
	needs := make([]socketNeed, 0, len(i.streams)+len(i.Addrs))
	for _, st := range i.streams {
		needs = append(needs, socketNeed{streamKey(st), "stream listen", st.Addr()})
	}
	for _, a := range i.Addrs {
		needs = append(needs, socketNeed{httpKey(a), "listen", a.Addr})
	}
	seen := make(map[socketKey]bool, len(needs))
	for _, n := range needs {
		if seen[n.key] {
			return nil, n.errorf("address is used by more than one server")
		}
		seen[n.key] = true
	}
	return needs, nil
}

// stopAcceptingLocked marks the instance closed and stops every server from
// accepting, without closing sockets. Serve errors after this are expected and
// are not reported. The caller holds i.handoffMu.
func (i *Instance) stopAcceptingLocked() {
	i.closed.Store(true)
	for _, st := range i.streams {
		st.Close()
	}
	for _, a := range i.Addrs {
		if a.view != nil {
			a.view.Close()
		}
	}
}

// Zones are kept across a reload like nginx's shared memory zones: a zone with
// the same name and key (limit_req, limit_conn) or the same directory
// (proxy_cache_path) keeps its state; a changed definition starts empty.
// nginx rejects such a reload instead; crest starts that one zone fresh.
// A limit_req rate change keeps the buckets and drains them at the new rate.
// Each helper returns nil when p is nil or has no matching zone.

func (p *Instance) prevHTTP() *config.HTTP {
	if p == nil || p.cfg == nil {
		return nil
	}
	return p.cfg.HTTP
}

func (p *Instance) keptReqZone(name string, z config.LimitReqZone) *limiter.ReqLimiter {
	h := p.prevHTTP()
	if h == nil {
		return nil
	}
	if old, ok := h.LimitReqZones[name]; !ok || old.Key != z.Key {
		return nil
	}
	return p.reqLimiters[name]
}

func (p *Instance) keptConnZone(name string, z config.LimitConnZone) *limiter.ConnLimiter {
	h := p.prevHTTP()
	if h == nil {
		return nil
	}
	if old, ok := h.LimitConnZones[name]; !ok || old.Key != z.Key {
		return nil
	}
	return p.connLimiters[name]
}

func (p *Instance) keptCache(name, dir string) *cache.Cache {
	h := p.prevHTTP()
	if h == nil {
		return nil
	}
	if old, ok := h.CachePaths[name]; !ok || filepath.Clean(old) != filepath.Clean(dir) {
		return nil
	}
	p.handoffMu.Lock()
	defer p.handoffMu.Unlock()
	return p.caches[name] // nil once p is shut down or handed it on
}
