//go:build linux

package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLinuxServiceInstallAndUninstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crest.service")
	var calls [][]string
	m := &linuxServiceManager{unitPath: path, command: func(args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "show" {
			return []byte("LoadState=not-found\n"), nil
		}
		return nil, nil
	}}
	spec, err := makeServiceSpec("/opt/crest app/crest", "/etc/crest app/crest.yml")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Install(spec); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"show-environment"}, {"show", "crest.service", "--property=LoadState"}, {"daemon-reload"}, {"enable", "crest.service"}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("registration commands=%v", calls)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != systemdUnit(spec) {
		t.Fatalf("unit file=%q err=%v", b, err)
	}
	if err = m.Install(spec); err == nil {
		t.Fatal("existing service overwritten")
	}
	if err = m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unit not removed: %v", err)
	}
	if !reflect.DeepEqual(calls[len(calls)-3:], [][]string{{"stop", "crest.service"}, {"disable", "crest.service"}, {"daemon-reload"}}) {
		t.Fatalf("uninstall commands=%v", calls)
	}
}
func TestLinuxServiceRegistrationFailureIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crest.service")
	m := &linuxServiceManager{unitPath: path, command: func(args ...string) ([]byte, error) {
		if args[0] == "show" {
			return []byte("LoadState=not-found\n"), nil
		}
		if args[0] == "enable" {
			return []byte("permission denied"), errors.New("failed")
		}
		return nil, nil
	}}
	spec, _ := makeServiceSpec("/opt/crest", "/etc/crest.yml")
	err := m.Install(spec)
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("partial failure hidden: %v", err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("saved registration unavailable for repair")
	}
}
func TestSystemdReadinessNotification(t *testing.T) {
	// Unix socket path length is limited; avoid long test directory prefixes.
	d, err := os.MkdirTemp("/tmp", "crest-notify-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(d)
	socket := filepath.Join(d, "notify")
	ln, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err = notifySystemdReady(socket); err != nil {
		t.Fatal(err)
	}
	ln.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 128)
	n, _, err := ln.ReadFromUnix(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "READY=1") {
		t.Fatalf("notification=%q %v", buf[:n], err)
	}
}

func TestLinuxServiceDoesNotShadowVendorUnit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crest.service")
	m := &linuxServiceManager{unitPath: path, command: func(args ...string) ([]byte, error) {
		if args[0] == "show" {
			return []byte("LoadState=loaded\n"), nil
		}
		return nil, nil
	}}
	spec, _ := makeServiceSpec("/opt/crest", "/etc/crest.yml")
	if err := m.Install(spec); err == nil {
		t.Fatal("overrode a service installed outside /etc/systemd/system")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("created overriding unit")
	}
}
