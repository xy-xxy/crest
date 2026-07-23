// Package limiter implements nginx's limit_req / limit_conn algorithms.
package limiter

import (
	"sync"
	"time"
)

// ReqLimiter is a per-key leaky bucket matching ngx_http_limit_req_module:
// each request adds one to the key's excess, which drains at the zone rate.
type ReqLimiter struct {
	mu      sync.Mutex
	rate    float64 // requests per second
	buckets map[string]*reqBucket
}

type reqBucket struct {
	excess float64
	last   time.Time
}

func NewReq(rate float64) *ReqLimiter {
	return &ReqLimiter{rate: rate, buckets: map[string]*reqBucket{}}
}

// Take reports whether a request under key passes with the given burst, and
// the pacing delay to apply when the caller is not in nodelay mode.
func (l *ReqLimiter) Take(key string, burst float64) (ok bool, delay time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[key]
	if b == nil {
		l.sweep(now)
		l.buckets[key] = &reqBucket{last: now}
		return true, 0
	}
	excess := b.excess - l.rate*now.Sub(b.last).Seconds() + 1
	if excess < 0 {
		excess = 0
	}
	if excess > burst {
		// rejected requests do not consume the bucket
		return false, 0
	}
	b.excess = excess
	b.last = now
	return true, time.Duration(excess / l.rate * float64(time.Second))
}

// sweep drops idle buckets so the map cannot grow unbounded.
func (l *ReqLimiter) sweep(now time.Time) {
	if len(l.buckets) < 8192 {
		return
	}
	for k, b := range l.buckets {
		if now.Sub(b.last) > time.Minute {
			delete(l.buckets, k)
		}
	}
}

// ConnLimiter counts concurrent requests per key for limit_conn.
type ConnLimiter struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewConn() *ConnLimiter { return &ConnLimiter{counts: map[string]int{}} }

func (l *ConnLimiter) Acquire(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] >= max {
		return false
	}
	l.counts[key]++
	return true
}

func (l *ConnLimiter) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.counts[key] <= 1 {
		delete(l.counts, key)
	} else {
		l.counts[key]--
	}
}
