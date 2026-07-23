package upstream

import "testing"

func TestLeastConnPicksIdle(t *testing.T) {
	a := &Backend{Addr: "a"}
	b := &Backend{Addr: "b"}
	pool := NewLeastConn([]*Backend{a, b})

	x, _ := pool.Next("") // a and b tied → first
	y, _ := pool.Next("") // x now busy → the other
	if x == y {
		t.Fatal("least_conn should spread across idle backends")
	}
	pool.Release(x)
	z, _ := pool.Next("") // x released, y still busy → x again
	if z != x {
		t.Errorf("released backend should be preferred: got %s", z.Addr)
	}
}

func TestLeastConnRespectsWeight(t *testing.T) {
	a := &Backend{Addr: "a", Weight: 2}
	b := &Backend{Addr: "b", Weight: 1}
	pool := NewLeastConn([]*Backend{a, b})

	// a with 1 active at weight 2 (ratio 0.5) still beats b with 1 active
	// at weight 1 (ratio 1.0)
	pool.Next("") // a (tie → first)
	pool.Next("") // b? active: a=1,b=0 → 1*1 vs 0*2 → b
	got, _ := pool.Next("")
	if got != a {
		t.Errorf("weighted least_conn: got %s, want a", got.Addr)
	}
}

func TestIPHashSticky(t *testing.T) {
	a := &Backend{Addr: "a"}
	b := &Backend{Addr: "b"}
	c := &Backend{Addr: "c"}
	pool := NewIPHash([]*Backend{a, b, c})

	first, err := pool.Next("10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		got, _ := pool.Next("10.0.0.1")
		if got != first {
			t.Fatal("same key must stick to the same backend")
		}
	}
	// a down backend is skipped, deterministically
	pool.MarkDown(first)
	alt, err := pool.Next("10.0.0.1")
	if err != nil || alt == first {
		t.Fatalf("down backend must be skipped: %v %v", alt, err)
	}
	got, _ := pool.Next("10.0.0.1")
	if got != alt {
		t.Error("fallback backend should also be sticky")
	}
}
