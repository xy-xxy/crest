package logging

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
)

// AccessRecord holds the per-request values access log variables resolve from.
type AccessRecord struct {
	RemoteAddr   string
	User         string
	Time         time.Time
	Request      string // "GET /x HTTP/1.1"
	Status       int
	BodyBytes    int64
	Referer      string
	UserAgent    string
	Host         string
	Duration     time.Duration
	UpstreamAddr string
	// Extra resolves variables the record does not model directly (e.g. map
	// targets, $arg_*); ok=false falls through to "-".
	Extra func(name string) (string, bool)
}

type segment struct {
	literal string
	varName string
}

// AccessLogger is the access-log sink; both a single log and a
// success/fail split satisfy it.
type AccessLogger interface {
	Log(AccessRecord)
	Close()
	Reopen() error
}

type AccessLog struct {
	mu     sync.Mutex
	w      io.Writer
	closer io.Closer
	path   string // "" for std streams; file path otherwise
	segs   []segment
}

// OpenAccess creates an access log. Path "" or "stdout" writes to stdout.
func OpenAccess(path, format string) (*AccessLog, error) {
	l := &AccessLog{segs: parseFormat(format)}
	switch path {
	case "", "-", "stdout", "/dev/stdout":
		l.w = os.Stdout
	case "stderr", "/dev/stderr":
		l.w = os.Stderr
	default:
		f, err := openLogFile(path)
		if err != nil {
			return nil, err
		}
		l.w = f
		l.closer = f
		l.path = path
	}
	return l, nil
}

// Reopen closes and reopens the backing file so an external rotator can move
// it aside, mirroring nginx's "reopen" signal. No-op for std streams.
func (l *AccessLog) Reopen() error {
	if l == nil || l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closer != nil {
		l.closer.Close()
	}
	f, err := openLogFile(l.path)
	if err != nil {
		return err
	}
	l.w = f
	l.closer = f
	return nil
}

func (l *AccessLog) Log(r AccessRecord) {
	if l == nil {
		return
	}
	buf := make([]byte, 0, 256)
	for _, s := range l.segs {
		if s.varName == "" {
			buf = append(buf, s.literal...)
		} else {
			buf = append(buf, resolve(&r, s.varName)...)
		}
	}
	buf = append(buf, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w.Write(buf)
}

func (l *AccessLog) Close() {
	if l == nil || l.closer == nil {
		return
	}
	l.closer.Close()
}

// SplitAccessLog routes successful responses (status < 400) and failures
// (>= 400) to separate logs.
type SplitAccessLog struct {
	success *AccessLog
	fail    *AccessLog
}

func OpenSplit(successPath, failPath, format string) (*SplitAccessLog, error) {
	s, err := OpenAccess(successPath, format)
	if err != nil {
		return nil, err
	}
	f, err := OpenAccess(failPath, format)
	if err != nil {
		s.Close()
		return nil, err
	}
	return &SplitAccessLog{success: s, fail: f}, nil
}

func (s *SplitAccessLog) Log(r AccessRecord) {
	if r.Status >= 400 {
		s.fail.Log(r)
	} else {
		s.success.Log(r)
	}
}

func (s *SplitAccessLog) Close() {
	s.success.Close()
	s.fail.Close()
}

func (s *SplitAccessLog) Reopen() error {
	if err := s.success.Reopen(); err != nil {
		return err
	}
	return s.fail.Reopen()
}

func resolve(r *AccessRecord, name string) string {
	switch name {
	case "remote_addr":
		return dash(r.RemoteAddr)
	case "remote_user":
		return dash(r.User)
	case "time_local":
		return r.Time.Format("02/Jan/2006:15:04:05 -0700")
	case "request":
		return dash(r.Request)
	case "status":
		return strconv.Itoa(r.Status)
	case "body_bytes_sent":
		return strconv.FormatInt(r.BodyBytes, 10)
	case "http_referer":
		return dash(r.Referer)
	case "http_user_agent":
		return dash(r.UserAgent)
	case "host":
		return dash(r.Host)
	case "request_time":
		return fmt.Sprintf("%.3f", r.Duration.Seconds())
	case "upstream_addr":
		return dash(r.UpstreamAddr)
	}
	if r.Extra != nil {
		if v, ok := r.Extra(name); ok {
			return dash(v)
		}
	}
	return "-"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func parseFormat(f string) []segment {
	var segs []segment
	var lit []byte
	i := 0
	for i < len(f) {
		if f[i] == '$' {
			j := i + 1
			for j < len(f) && isVarChar(f[j]) {
				j++
			}
			if j > i+1 {
				if len(lit) > 0 {
					segs = append(segs, segment{literal: string(lit)})
					lit = nil
				}
				segs = append(segs, segment{varName: f[i+1 : j]})
				i = j
				continue
			}
		}
		lit = append(lit, f[i])
		i++
	}
	if len(lit) > 0 {
		segs = append(segs, segment{literal: string(lit)})
	}
	return segs
}

func isVarChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}
