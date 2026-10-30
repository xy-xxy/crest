package server

import (
	"net"
	"sync"
	"time"
)

// acceptQueueSize bounds raw TCP connections waiting for http.Server. Draining
// the listener independently avoids mixing accept with per-connection HTTP/TLS
// initialization during a short burst. It does not cap active HTTP requests.
const acceptQueueSize = 512

type accepted struct {
	conn net.Conn
	err  error
}
type queuedListener struct {
	net.Listener
	pending     chan accepted
	stop        chan struct{}
	finished    chan struct{}
	once        sync.Once
	closeErr    error
	terminalErr error // published before pending is closed
}

func queueListener(ln net.Listener, size int) *queuedListener {
	q := &queuedListener{Listener: ln, pending: make(chan accepted, size), stop: make(chan struct{}), finished: make(chan struct{})}
	go q.receive()
	return q
}
func (q *queuedListener) receive() {
	defer close(q.finished)
	defer close(q.pending)
	var delay time.Duration
	for {
		conn, err := q.Listener.Accept()
		temporary := false
		if e, ok := err.(net.Error); ok {
			temporary = e.Temporary()
		}
		if err != nil && !temporary {
			q.terminalErr = err
			return
		}
		select {
		case q.pending <- accepted{conn, err}:
		case <-q.stop:
			if conn != nil {
				conn.Close()
			}
			return
		}
		if temporary {
			if delay == 0 {
				delay = 5 * time.Millisecond
			} else {
				delay *= 2
			}
			if delay > time.Second {
				delay = time.Second
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-q.stop:
				timer.Stop()
				return
			}
		} else {
			delay = 0
		}
	}
}
func (q *queuedListener) Accept() (net.Conn, error) {
	select {
	case <-q.stop:
		return nil, net.ErrClosed
	default:
	}
	select {
	case <-q.stop:
		return nil, net.ErrClosed
	case a, ok := <-q.pending:
		if !ok {
			if q.terminalErr != nil {
				return nil, q.terminalErr
			}
			return nil, net.ErrClosed
		}
		select {
		case <-q.stop:
			if a.conn != nil {
				a.conn.Close()
			}
			return nil, net.ErrClosed
		default:
		}
		return a.conn, a.err
	}
}
func (q *queuedListener) Close() error {
	q.once.Do(func() {
		close(q.stop)
		q.closeErr = q.Listener.Close()
		<-q.finished
		for a := range q.pending {
			if a.conn != nil {
				a.conn.Close()
			}
		}
	})
	return q.closeErr
}
