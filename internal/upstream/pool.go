package upstream

import (
	"errors"
	"time"
)

// Backend is one upstream server.
type Backend struct {
	Addr        string
	Weight      int
	MaxFails    int           // failures within FailTimeout before marked down; 0 = 1
	FailTimeout time.Duration // both the failure window and the down duration; 0 = 10s
	Backup      bool          // only used when every primary is unavailable
	Down        bool          // administratively removed

	current   int // smooth WRR state
	active    int // in-flight requests, for least_conn
	fails     int
	firstFail time.Time
	downUntil time.Time
}

var ErrNoAvailableBackends = errors.New("upstream: no available backends")

// Pool selects backends for proxied requests. key carries the client address
// for hash-based balancers; other implementations ignore it.
type Pool interface {
	Next(key string) (*Backend, error)
	// Release marks a request finished, for balancers tracking active use.
	Release(*Backend)
	// MarkDown records a failure; enough failures within fail_timeout take
	// the backend out of rotation for fail_timeout.
	MarkDown(*Backend)
	// MarkUp resets the failure count after a successful exchange.
	MarkUp(*Backend)
	Backends() []*Backend
}

// failTimeout mirrors nginx's default fail_timeout=10s.
const failTimeout = 10 * time.Second

func (b *Backend) up(now time.Time) bool { return !now.Before(b.downUntil) }

func (b *Backend) failWindow() time.Duration {
	if b.FailTimeout > 0 {
		return b.FailTimeout
	}
	return failTimeout
}

func (b *Backend) fail(now time.Time) {
	if b.fails == 0 || now.Sub(b.firstFail) > b.failWindow() {
		b.fails = 0
		b.firstFail = now
	}
	b.fails++
	max := b.MaxFails
	if max <= 0 {
		max = 1
	}
	if b.fails >= max {
		b.downUntil = now.Add(b.failWindow())
		b.fails = 0
	}
}

func (b *Backend) succeed() { b.fails = 0 }

// available returns usable primaries, falling back to backups only when no
// primary is usable, like nginx.
func available(backends []*Backend, now time.Time) []*Backend {
	var prim, back []*Backend
	for _, b := range backends {
		if b.Down || !b.up(now) {
			continue
		}
		if b.Backup {
			back = append(back, b)
		} else {
			prim = append(prim, b)
		}
	}
	if len(prim) > 0 {
		return prim
	}
	return back
}

func normalizeWeights(backends []*Backend) {
	for _, b := range backends {
		if b.Weight <= 0 {
			b.Weight = 1
		}
	}
}
