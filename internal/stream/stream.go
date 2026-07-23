// Package stream implements L4 (TCP/UDP) proxying like ngx_stream_module.
package stream

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gonx/internal/config"
	"gonx/internal/logging"
	"gonx/internal/upstream"
)

// Server proxies one listen address at L4.
type Server struct {
	cfg    *config.StreamServer
	pool   upstream.Pool // nil when proxying to a literal address
	target string
	errlog *logging.ErrorLog

	ln     net.Listener
	pc     net.PacketConn
	closed atomic.Bool
}

func NewServer(cfg *config.StreamServer, pools map[string]upstream.Pool, errlog *logging.ErrorLog) (*Server, error) {
	s := &Server{cfg: cfg, errlog: errlog}
	if p, ok := pools[cfg.ProxyPass]; ok {
		s.pool = p
	} else {
		if !strings.Contains(cfg.ProxyPass, ":") {
			return nil, fmt.Errorf("stream server (line %d): proxy_pass %q is neither an upstream nor host:port",
				cfg.Line, cfg.ProxyPass)
		}
		s.target = cfg.ProxyPass
	}
	return s, nil
}

func (s *Server) Addr() string { return s.cfg.Addr }

// netFamily forces IPv4 for a bare port or IPv4 host (matching nginx's stream
// listen default) and IPv6 only for an explicit IPv6 literal. proto is
// "tcp" or "udp".
func netFamily(proto, addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return proto + "4"
	}
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return proto + "6"
	}
	return proto + "4"
}

func (s *Server) Start() error {
	if s.cfg.UDP {
		pc, err := net.ListenPacket(netFamily("udp", s.cfg.Addr), s.cfg.Addr)
		if err != nil {
			return err
		}
		s.pc = pc
		s.errlog.Logf(logging.LevelInfo, "stream: listening on %s (udp)", s.cfg.Addr)
		go s.serveUDP()
		return nil
	}
	ln, err := net.Listen(netFamily("tcp", s.cfg.Addr), s.cfg.Addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.errlog.Logf(logging.LevelInfo, "stream: listening on %s (tcp)", s.cfg.Addr)
	go s.serveTCP()
	return nil
}

func (s *Server) Close() {
	s.closed.Store(true)
	if s.ln != nil {
		s.ln.Close()
	}
	if s.pc != nil {
		s.pc.Close()
	}
}

func (s *Server) dial(proto string) (net.Conn, *upstream.Backend, error) {
	if s.pool == nil {
		c, err := net.DialTimeout(proto, s.target, s.cfg.ConnectTimeout)
		return c, nil, err
	}
	tries := len(s.pool.Backends())
	var lastErr error
	for i := 0; i < tries; i++ {
		b, err := s.pool.Next("")
		if err != nil {
			if lastErr != nil {
				return nil, nil, lastErr
			}
			return nil, nil, err
		}
		c, err := net.DialTimeout(proto, b.Addr, s.cfg.ConnectTimeout)
		if err == nil {
			return c, b, nil
		}
		s.pool.Release(b)
		s.pool.MarkDown(b)
		lastErr = err
		s.errlog.Logf(logging.LevelWarn, "stream: upstream %s failed: %v", b.Addr, err)
	}
	return nil, nil, lastErr
}

func (s *Server) serveTCP() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return
			}
			s.errlog.Logf(logging.LevelError, "stream: accept on %s: %v", s.cfg.Addr, err)
			continue
		}
		go s.handleTCP(c)
	}
}

func (s *Server) handleTCP(client net.Conn) {
	defer client.Close()
	up, b, err := s.dial("tcp")
	if err != nil {
		s.errlog.Logf(logging.LevelError, "stream: connect failed for %s: %v", s.cfg.Addr, err)
		return
	}
	defer up.Close()
	if b != nil {
		defer s.pool.Release(b)
	}
	done := make(chan struct{}, 2)
	pump := func(dst, src net.Conn) {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			// proxy_timeout: idle limit between successive operations
			src.SetReadDeadline(time.Now().Add(s.cfg.ProxyTimeout))
			n, err := src.Read(buf)
			if n > 0 {
				dst.SetWriteDeadline(time.Now().Add(s.cfg.ProxyTimeout))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	go pump(up, client)
	go pump(client, up)
	<-done
	// returning closes both conns, which unblocks the other pump
}

type udpSession struct {
	up        net.Conn
	remaining int32 // expected responses; -1 = unlimited
}

func (s *Server) serveUDP() {
	var mu sync.Mutex
	sessions := map[string]*udpSession{}
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := s.pc.ReadFrom(buf)
		if err != nil {
			if s.closed.Load() {
				return
			}
			s.errlog.Logf(logging.LevelError, "stream: read on %s: %v", s.cfg.Addr, err)
			continue
		}
		key := addr.String()
		mu.Lock()
		sess := sessions[key]
		if sess == nil {
			up, b, err := s.dial("udp")
			if err != nil {
				mu.Unlock()
				s.errlog.Logf(logging.LevelError, "stream: connect failed for %s: %v", s.cfg.Addr, err)
				continue
			}
			sess = &udpSession{up: up, remaining: -1}
			sessions[key] = sess
			go s.sessionReadLoop(sess, addr, func() {
				mu.Lock()
				delete(sessions, key)
				mu.Unlock()
				if b != nil {
					s.pool.Release(b)
				}
			})
		}
		if s.cfg.ProxyResponses >= 0 {
			atomic.StoreInt32(&sess.remaining, int32(s.cfg.ProxyResponses))
		}
		mu.Unlock()
		sess.up.SetWriteDeadline(time.Now().Add(s.cfg.ProxyTimeout))
		if _, err := sess.up.Write(buf[:n]); err != nil {
			s.errlog.Logf(logging.LevelWarn, "stream: write to upstream: %v", err)
		}
	}
}

// sessionReadLoop relays upstream datagrams back to one client until the
// session times out or the expected response count is reached.
func (s *Server) sessionReadLoop(sess *udpSession, client net.Addr, done func()) {
	defer done()
	defer sess.up.Close()
	buf := make([]byte, 64*1024)
	for {
		sess.up.SetReadDeadline(time.Now().Add(s.cfg.ProxyTimeout))
		n, err := sess.up.Read(buf)
		if err != nil {
			return
		}
		s.pc.WriteTo(buf[:n], client)
		if atomic.LoadInt32(&sess.remaining) > 0 {
			if atomic.AddInt32(&sess.remaining, -1) == 0 {
				return
			}
		}
	}
}
