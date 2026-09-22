package server

import "testing"

func TestInstanceReadyLifecycle(t *testing.T) {
	var inst Instance
	if inst.Ready() {
		t.Fatal("zero instance must not be ready")
	}
	inst.ready.Store(true)
	if !inst.Ready() {
		t.Fatal("a successfully bound instance should be ready")
	}
	inst.closed.Store(true)
	if inst.Ready() {
		t.Fatal("a shutting down instance must not be ready")
	}
}
