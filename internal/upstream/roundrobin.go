package upstream

import (
	"sync"
	"time"
)

// roundRobin implements nginx's smooth weighted round-robin algorithm.
type roundRobin struct {
	mu       sync.Mutex
	backends []*Backend
}

func NewRoundRobin(backends []*Backend) Pool {
	normalizeWeights(backends)
	return &roundRobin{backends: backends}
}

func (r *roundRobin) Next(string) (*Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var best *Backend
	total := 0
	for _, b := range available(r.backends, time.Now()) {
		b.current += b.Weight
		total += b.Weight
		if best == nil || b.current > best.current {
			best = b
		}
	}
	if best == nil {
		return nil, ErrNoAvailableBackends
	}
	best.current -= total
	return best, nil
}

func (r *roundRobin) Release(*Backend) {}

func (r *roundRobin) MarkDown(b *Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b.fail(time.Now())
}

func (r *roundRobin) MarkUp(b *Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b.succeed()
}

func (r *roundRobin) Backends() []*Backend { return r.backends }
