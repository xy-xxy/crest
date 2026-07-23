package upstream

import (
	"sync"
	"time"
)

// leastConn picks the backend with the fewest active requests relative to
// its weight, like ngx_http_upstream_least_conn_module.
type leastConn struct {
	mu       sync.Mutex
	backends []*Backend
}

func NewLeastConn(backends []*Backend) Pool {
	normalizeWeights(backends)
	return &leastConn{backends: backends}
}

func (l *leastConn) Next(string) (*Backend, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var best *Backend
	for _, b := range available(l.backends, time.Now()) {
		// compare active/weight without division: b < best
		if best == nil || b.active*best.Weight < best.active*b.Weight {
			best = b
		}
	}
	if best == nil {
		return nil, ErrNoAvailableBackends
	}
	best.active++
	return best, nil
}

func (l *leastConn) Release(b *Backend) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b.active > 0 {
		b.active--
	}
}

func (l *leastConn) MarkDown(b *Backend) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b.fail(time.Now())
}

func (l *leastConn) MarkUp(b *Backend) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b.succeed()
}

func (l *leastConn) Backends() []*Backend { return l.backends }
