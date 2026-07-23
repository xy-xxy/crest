package upstream

import (
	"testing"
	"time"
)

func now() time.Time { return time.Now() }

func TestMaxFails(t *testing.T) {
	b := &Backend{Addr: "a", MaxFails: 3, FailTimeout: failTimeout}
	pool := NewRoundRobin([]*Backend{b, {Addr: "b"}})

	// two failures within the window keep it in rotation
	pool.MarkDown(b)
	pool.MarkDown(b)
	if !b.up(now()) {
		t.Error("backend should survive 2 failures with max_fails=3")
	}
	// third failure trips it
	pool.MarkDown(b)
	if b.up(now()) {
		t.Error("backend should be down after 3 failures")
	}
}

func TestMarkUpResetsFailures(t *testing.T) {
	b := &Backend{Addr: "a", MaxFails: 2, FailTimeout: failTimeout}
	pool := NewRoundRobin([]*Backend{b, {Addr: "b"}})
	pool.MarkDown(b)
	pool.MarkUp(b) // success clears the counter
	pool.MarkDown(b)
	if b.up(now()) == false {
		t.Error("one failure after a success should not trip max_fails=2")
	}
}

func TestBackup(t *testing.T) {
	primary := &Backend{Addr: "primary"}
	backup := &Backend{Addr: "backup", Backup: true}
	pool := NewRoundRobin([]*Backend{primary, backup})

	// backup is idle while the primary is up
	for i := 0; i < 5; i++ {
		b, _ := pool.Next("")
		if b.Addr != "primary" {
			t.Fatalf("backup used while primary is up: %s", b.Addr)
		}
	}
	// primary down → backup takes over
	pool.MarkDown(primary)
	b, err := pool.Next("")
	if err != nil || b.Addr != "backup" {
		t.Errorf("backup should serve when primary is down: %v %v", b, err)
	}
}

func TestDownFlag(t *testing.T) {
	pool := NewRoundRobin([]*Backend{{Addr: "a", Down: true}, {Addr: "b"}})
	for i := 0; i < 4; i++ {
		b, _ := pool.Next("")
		if b.Addr != "b" {
			t.Fatalf("administratively down backend selected: %s", b.Addr)
		}
	}
}
