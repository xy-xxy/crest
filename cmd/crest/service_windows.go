//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

type windowsServiceManager struct{}

func newServiceManager() serviceManager { return windowsServiceManager{} }
func (windowsServiceManager) Install(spec serviceSpec) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	existing, err := m.OpenService(serviceName)
	if err == nil {
		existing.Close()
		return fmt.Errorf("service %s already exists; uninstall it before changing its registration", serviceName)
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	// CreateService quotes executable/arguments using the Windows command-line
	// rules, including C:\Program Files paths. No cmd.exe/sc.exe shell is used.
	s, err := m.CreateService(serviceName, spec.Executable, mgr.Config{
		DisplayName: "crest", Description: "crest web server and reverse proxy",
		StartType: mgr.StartAutomatic, ErrorControl: mgr.ErrorNormal,
	}, spec.Arguments...)
	if err != nil {
		return err
	}
	return s.Close()
}
func withWindowsService(access uint32, fn func(*mgr.Service) error) error {
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer windows.CloseServiceHandle(m)
	name, err := windows.UTF16PtrFromString(serviceName)
	if err != nil {
		return err
	}
	h, err := windows.OpenService(m, name, access)
	if err != nil {
		return err
	}
	s := &mgr.Service{Name: serviceName, Handle: h}
	defer s.Close()
	return fn(s)
}
func waitWindowsState(s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if want == svc.Running && st.State == svc.Stopped {
			return fmt.Errorf("service stopped during startup (Windows exit=%d, service exit=%d); inspect log/console.log", st.Win32ExitCode, st.ServiceSpecificExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for service state %d (current %d)", want, st.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
func stopWindowsService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if st.State != svc.StopPending {
		if _, err = s.Control(svc.Stop); err != nil {
			return err
		}
	}
	return waitWindowsState(s, svc.Stopped)
}
func (windowsServiceManager) Uninstall() error {
	return withWindowsService(windows.DELETE|windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS, func(s *mgr.Service) error {
		if err := stopWindowsService(s); err != nil {
			return err
		}
		return s.Delete()
	})
}
func (windowsServiceManager) Start() error {
	return withWindowsService(windows.SERVICE_START|windows.SERVICE_QUERY_STATUS, func(s *mgr.Service) error {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Running {
			return nil
		}
		if st.State != svc.StartPending {
			if err = s.Start(); err != nil {
				return err
			}
		}
		return waitWindowsState(s, svc.Running)
	})
}
func (windowsServiceManager) Stop() error {
	return withWindowsService(windows.SERVICE_STOP|windows.SERVICE_QUERY_STATUS, stopWindowsService)
}
func (m windowsServiceManager) Restart() error {
	if err := m.Stop(); err != nil {
		return err
	}
	return m.Start()
}
func (windowsServiceManager) Status() (string, error) {
	var text string
	err := withWindowsService(windows.SERVICE_QUERY_STATUS, func(s *mgr.Service) error {
		st, err := s.Query()
		if err != nil {
			return err
		}
		switch st.State {
		case svc.Running:
			text = "running"
		case svc.Stopped:
			text = "stopped"
		case svc.StartPending:
			text = "starting"
		case svc.StopPending:
			text = "stopping"
		default:
			text = fmt.Sprintf("state %d", st.State)
		}
		return nil
	})
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return "not installed", nil
	}
	return text, err
}

func runAsService(config string) error {
	managed, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !managed {
		return errors.New("run-service is reserved for the Windows Service Control Manager; use 'crest start'")
	}
	return svc.Run(serviceName, &windowsRuntime{config: config})
}
func serviceWorkingDirectory(config string) error { return os.Chdir(filepath.Dir(config)) }

type windowsRuntime struct {
	config string
	serve  func(*serviceLifecycle) error
}

func (w *windowsRuntime) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	stop := make(chan struct{})
	var once sync.Once
	ready := make(chan error, 1)
	done := make(chan error, 1)
	serve := w.serve
	if serve == nil {
		serve = func(lc *serviceLifecycle) error { return runServer(w.config, false, nil, lc) }
	}
	go func() { done <- serve(&serviceLifecycle{stop: stop, started: func(err error) { ready <- err }}) }()
	state := svc.Status{State: svc.StartPending, WaitHint: 15000}
	status <- state
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer once.Do(func() { close(stop) })
	for {
		select {
		case err := <-ready:
			if err != nil {
				return true, 1
			}
			ready = nil
			if state.State != svc.StopPending {
				state = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				status <- state
			}
		case err := <-done:
			if err != nil {
				return true, 1
			}
			return false, 0
		case req, ok := <-requests:
			if !ok {
				once.Do(func() { close(stop) })
				requests = nil
				state = svc.Status{State: svc.StopPending, WaitHint: 15000}
				status <- state
				continue
			}
			switch req.Cmd {
			case svc.Interrogate:
				status <- state
			case svc.Stop, svc.Shutdown:
				state = svc.Status{State: svc.StopPending, WaitHint: 15000}
				status <- state
				once.Do(func() { close(stop) })
			}
		case <-ticker.C:
			if state.State == svc.StartPending || state.State == svc.StopPending {
				state.CheckPoint++
				status <- state
			}
		}
	}
}
