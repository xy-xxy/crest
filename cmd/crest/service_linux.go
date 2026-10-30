//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

type linuxServiceManager struct {
	unitPath string
	command  func(...string) ([]byte, error)
}

func newServiceManager() serviceManager {
	return &linuxServiceManager{unitPath: "/etc/systemd/system/crest.service", command: func(args ...string) ([]byte, error) { return exec.Command("systemctl", args...).CombinedOutput() }}
}
func (m *linuxServiceManager) systemctl(args ...string) error {
	b, err := m.command(args...)
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(b)))
	}
	return nil
}
func (m *linuxServiceManager) Install(spec serviceSpec) error {
	if err := m.systemctl("show-environment"); err != nil {
		return fmt.Errorf("a running systemd service manager is required: %w", err)
	}
	state, err := m.command("show", "crest.service", "--property=LoadState")
	if err != nil {
		return fmt.Errorf("check existing service: %w: %s", err, strings.TrimSpace(string(state)))
	}
	if strings.TrimSpace(string(state)) != "LoadState=not-found" {
		return fmt.Errorf("service crest already exists; uninstall it before changing its registration")
	}
	f, err := os.OpenFile(m.unitPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("create %s (existing registrations are not overwritten): %w", m.unitPath, err)
	}
	_, writeErr := f.WriteString(systemdUnit(spec))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(m.unitPath)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if err = m.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("unit saved at %s; retry daemon-reload: %w", m.unitPath, err)
	}
	if err = m.systemctl("enable", "crest.service"); err != nil {
		return fmt.Errorf("unit saved but automatic startup was not enabled: %w", err)
	}
	return nil
}
func (m *linuxServiceManager) Uninstall() error {
	if _, err := os.Stat(m.unitPath); err != nil {
		return err
	}
	if err := m.systemctl("stop", "crest.service"); err != nil {
		return err
	}
	if err := m.systemctl("disable", "crest.service"); err != nil {
		return err
	}
	if err := os.Remove(m.unitPath); err != nil {
		return err
	}
	return m.systemctl("daemon-reload")
}
func (m *linuxServiceManager) Start() error   { return m.systemctl("start", "crest.service") }
func (m *linuxServiceManager) Stop() error    { return m.systemctl("stop", "crest.service") }
func (m *linuxServiceManager) Restart() error { return m.systemctl("restart", "crest.service") }
func (m *linuxServiceManager) Status() (string, error) {
	b, err := m.command("show", "crest.service", "--property=LoadState", "--property=ActiveState", "--property=SubState")
	if err != nil {
		return "", fmt.Errorf("systemctl show: %w: %s", err, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}
func runAsService(config string) error {
	stop := make(chan struct{})
	var notifyErr error
	err := runServer(config, false, nil, &serviceLifecycle{stop: stop, started: func(err error) {
		if err == nil {
			notifyErr = notifySystemdReady(os.Getenv("NOTIFY_SOCKET"))
			if notifyErr != nil {
				close(stop)
			}
		}
	}})
	if notifyErr != nil {
		return notifyErr
	}
	return err
}

func notifySystemdReady(socket string) error {
	if socket == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("systemd readiness notification: %w", err)
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1\nSTATUS=crest running"))
	return err
}
func serviceWorkingDirectory(string) error { return nil }
