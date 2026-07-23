package upstream

import (
	"hash/fnv"
	"sync"
	"time"
)

// ipHash pins each client (by hash key) to one backend, walking forward
// when that backend is down.
type ipHash struct {
	mu       sync.Mutex
	backends []*Backend
}

func NewIPHash(backends []*Backend) Pool {
	normalizeWeights(backends)
	return &ipHash{backends: backends}
}

func (p *ipHash) Next(key string) (*Backend, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cands := available(p.backends, time.Now())
	if len(cands) == 0 {
		return nil, ErrNoAvailableBackends
	}
	h := fnv.New32a()
	h.Write([]byte(key))
	return cands[int(h.Sum32()%uint32(len(cands)))], nil
}

func (p *ipHash) Release(*Backend) {}

func (p *ipHash) MarkDown(b *Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b.fail(time.Now())
}

func (p *ipHash) MarkUp(b *Backend) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b.succeed()
}

func (p *ipHash) Backends() []*Backend { return p.backends }
