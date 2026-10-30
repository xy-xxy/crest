//go:build windows

package main

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"
)

type serviceExit struct {
	specific bool
	code     uint32
}

func TestWindowsServiceStopHandshake(t *testing.T) {
	requests := make(chan svc.ChangeRequest, 4)
	status := make(chan svc.Status, 8)
	done := make(chan serviceExit, 1)
	h := &windowsRuntime{serve: func(lc *serviceLifecycle) error { lc.started(nil); <-lc.stop; return nil }}
	go func() { specific, code := h.Execute(nil, requests, status); done <- serviceExit{specific, code} }()
	next := func(want svc.State) {
		t.Helper()
		select {
		case st := <-status:
			if st.State != want {
				t.Fatalf("state=%v want=%v", st, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing SCM status")
		}
	}
	next(svc.StartPending)
	next(svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Interrogate}
	next(svc.Running)
	requests <- svc.ChangeRequest{Cmd: svc.Stop}
	next(svc.StopPending)
	select {
	case exit := <-done:
		if exit.specific || exit.code != 0 {
			t.Fatalf("stop exit=%+v", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not join server shutdown")
	}
}
func TestWindowsServiceStartupFailure(t *testing.T) {
	status := make(chan svc.Status, 8)
	done := make(chan serviceExit, 1)
	h := &windowsRuntime{serve: func(lc *serviceLifecycle) error { err := errors.New("listen failed"); lc.started(err); return err }}
	go func() {
		specific, code := h.Execute(nil, make(chan svc.ChangeRequest), status)
		done <- serviceExit{specific, code}
	}()
	select {
	case exit := <-done:
		if !exit.specific || exit.code == 0 {
			t.Fatalf("startup failure hidden: %+v", exit)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("startup failure hung SCM")
	}
	for len(status) > 0 {
		if st := <-status; st.State == svc.Running {
			t.Fatal("failed service reported running")
		}
	}
}
