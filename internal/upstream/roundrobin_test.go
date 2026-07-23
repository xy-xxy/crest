package upstream

import "testing"

func TestSmoothWeightedRoundRobin(t *testing.T) {
	a := &Backend{Addr: "a", Weight: 3}
	b := &Backend{Addr: "b", Weight: 1}
	pool := NewRoundRobin([]*Backend{a, b})

	counts := map[string]int{}
	var order []string
	for i := 0; i < 8; i++ {
		bk, err := pool.Next("")
		if err != nil {
			t.Fatal(err)
		}
		counts[bk.Addr]++
		order = append(order, bk.Addr)
	}
	if counts["a"] != 6 || counts["b"] != 2 {
		t.Errorf("weight distribution wrong: %v (order %v)", counts, order)
	}
	// smooth WRR must not bunch the heavy backend: a a b a, not a a a b
	want := []string{"a", "a", "b", "a"}
	for i, w := range want {
		if order[i] != w {
			t.Errorf("smooth order wrong at %d: got %v want %v", i, order[:4], want)
			break
		}
	}
}

func TestMarkDownSkipsBackend(t *testing.T) {
	a := &Backend{Addr: "a"}
	b := &Backend{Addr: "b"}
	pool := NewRoundRobin([]*Backend{a, b})

	pool.MarkDown(a)
	for i := 0; i < 4; i++ {
		bk, err := pool.Next("")
		if err != nil {
			t.Fatal(err)
		}
		if bk.Addr != "b" {
			t.Fatalf("down backend was selected")
		}
	}
	pool.MarkDown(b)
	if _, err := pool.Next(""); err != ErrNoAvailableBackends {
		t.Errorf("want ErrNoAvailableBackends, got %v", err)
	}
}

func TestDefaultWeight(t *testing.T) {
	pool := NewRoundRobin([]*Backend{{Addr: "a", Weight: 0}})
	bk, err := pool.Next("")
	if err != nil || bk.Addr != "a" {
		t.Fatalf("zero weight should default to 1: %v %v", bk, err)
	}
}
