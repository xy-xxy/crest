package limiter

import (
	"testing"
	"time"
)

func TestReqLimiterBurst(t *testing.T) {
	l := NewReq(1) // 1 r/s
	// first request creates the bucket and always passes
	if ok, _ := l.Take("k", 2); !ok {
		t.Fatal("first request must pass")
	}
	// back-to-back requests consume the burst: excess 1, 2 pass; 3 rejected
	ok1, d1 := l.Take("k", 2)
	ok2, d2 := l.Take("k", 2)
	ok3, _ := l.Take("k", 2)
	if !ok1 || !ok2 {
		t.Errorf("burst requests should pass: %v %v", ok1, ok2)
	}
	if ok3 {
		t.Error("request beyond burst must be rejected")
	}
	if d1 < 900*time.Millisecond || d2 < 1900*time.Millisecond {
		t.Errorf("delays should pace at the rate: %v %v", d1, d2)
	}
}

func TestReqLimiterZeroBurst(t *testing.T) {
	l := NewReq(1000)
	if ok, _ := l.Take("k", 0); !ok {
		t.Fatal("first request must pass")
	}
	if ok, _ := l.Take("k", 0); ok {
		t.Error("immediate second request must be rejected with burst=0")
	}
	time.Sleep(5 * time.Millisecond) // several 1/rate intervals at 1000 r/s
	if ok, _ := l.Take("k", 0); !ok {
		t.Error("request after the rate interval must pass")
	}
}

func TestReqLimiterKeysIsolated(t *testing.T) {
	l := NewReq(1)
	l.Take("a", 0)
	if ok, _ := l.Take("b", 0); !ok {
		t.Error("different keys must not share buckets")
	}
}

func TestConnLimiter(t *testing.T) {
	l := NewConn()
	if !l.Acquire("k", 2) || !l.Acquire("k", 2) {
		t.Fatal("acquires within limit must pass")
	}
	if l.Acquire("k", 2) {
		t.Error("acquire beyond limit must fail")
	}
	l.Release("k")
	if !l.Acquire("k", 2) {
		t.Error("acquire after release must pass")
	}
}
