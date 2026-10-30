package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeServiceManager struct {
	calls []string
	spec  serviceSpec
	err   error
}

func (m *fakeServiceManager) Install(s serviceSpec) error {
	m.calls = append(m.calls, "install")
	m.spec = s
	return m.err
}
func (m *fakeServiceManager) Uninstall() error { m.calls = append(m.calls, "uninstall"); return m.err }
func (m *fakeServiceManager) Start() error     { m.calls = append(m.calls, "start"); return m.err }
func (m *fakeServiceManager) Stop() error      { m.calls = append(m.calls, "stop"); return m.err }
func (m *fakeServiceManager) Restart() error   { m.calls = append(m.calls, "restart"); return m.err }
func (m *fakeServiceManager) Status() (string, error) {
	m.calls = append(m.calls, "status")
	return "running", m.err
}

func TestServiceCommandInstall(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "Crest App", "crest")
	cfg := filepath.Join(t.TempDir(), "配置 空格", "crest.yml")
	prepare := func(string) (serviceSpec, error) { return makeServiceSpec(exe, cfg) }
	for _, tc := range []struct{ args, want []string }{{[]string{"install"}, []string{"install"}}, {[]string{"install", "--start"}, []string{"install", "start"}}, {[]string{"install", "--dry-run"}, nil}} {
		m := &fakeServiceManager{}
		var out bytes.Buffer
		if err := executeServiceCommand(tc.args, &out, m, prepare, nil); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(m.calls, tc.want) {
			t.Fatalf("calls=%v want=%v", m.calls, tc.want)
		}
		if len(tc.want) > 0 {
			if m.spec.Executable != exe || !reflect.DeepEqual(m.spec.Arguments, []string{"run-service", "-c", cfg}) || m.spec.WorkingDirectory != filepath.Dir(cfg) || !m.spec.AutoStart {
				t.Fatalf("bad registration: %+v", m.spec)
			}
		} else {
			var plan serviceSpec
			if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
				t.Fatal(err)
			}
			if plan.Executable != exe {
				t.Fatal("dry-run lost path")
			}
		}
	}
}
func TestServiceCommandValidationBeforeMutation(t *testing.T) {
	prepare := func(string) (serviceSpec, error) { return serviceSpec{}, errors.New("bad config") }
	for _, args := range [][]string{{"install", "--start"}, {"install", "unexpected"}, {"install", "-t"}, {"bogus"}} {
		m := &fakeServiceManager{}
		err := executeServiceCommand(args, &bytes.Buffer{}, m, prepare, nil)
		if err == nil || len(m.calls) != 0 {
			t.Fatalf("invalid input mutated service: %v %v", args, m.calls)
		}
	}
	m := &fakeServiceManager{err: errors.New("already exists")}
	err := executeServiceCommand([]string{"install", "--start"}, &bytes.Buffer{}, m, func(string) (serviceSpec, error) { return serviceSpec{}, nil }, nil)
	if err == nil || !reflect.DeepEqual(m.calls, []string{"install"}) {
		t.Fatal("failed install continued to start")
	}
}
func TestServiceControlDoesNotRequireConfig(t *testing.T) {
	for _, action := range []string{"start", "stop", "restart", "status", "uninstall"} {
		m := &fakeServiceManager{}
		err := executeServiceCommand([]string{action}, &bytes.Buffer{}, m, func(string) (serviceSpec, error) {
			t.Fatal("control should not load a config")
			return serviceSpec{}, nil
		}, nil)
		if err != nil || !reflect.DeepEqual(m.calls, []string{action}) {
			t.Fatalf("%s: %v %v", action, m.calls, err)
		}
	}
}
func TestSystemdEscaping(t *testing.T) {
	s := serviceSpec{Name: serviceName, Executable: `/opt/Crest $App/crest`, Arguments: []string{"run-service", "-c", `/etc/crest configs/quote"-$name-%n.conf`}, WorkingDirectory: `/etc/crest configs`}
	unit := systemdUnit(s)
	for _, want := range []string{`Type=notify`, `NotifyAccess=main`, `ExecStart="/opt/Crest $App/crest" "run-service" "-c" "/etc/crest configs/quote\"-$$name-%%n.conf"`, `WorkingDirectory=/etc/crest configs`, `TimeoutStopSec=15s`} {
		if !strings.Contains(unit, want) {
			t.Fatalf("missing %q in:\n%s", want, unit)
		}
	}
	if _, err := makeServiceSpec("relative", filepath.Join(t.TempDir(), "crest.yml")); err == nil {
		t.Fatal("accepted relative executable")
	}
	if _, err := makeServiceSpec(filepath.Join(t.TempDir(), "bad\nExecStart=bad"), filepath.Join(t.TempDir(), "crest.yml")); err == nil {
		t.Fatal("accepted control-character path")
	}
}
func TestPrepareServiceInvalidConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "crest.yml")
	if err := os.WriteFile(p, []byte("http: [invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareService(p); err == nil {
		t.Fatal("invalid config accepted")
	}
}
func TestManagedRuntimeStartsAndStops(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	d := t.TempDir()
	conf := filepath.Join(d, "crest.conf")
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	if err = os.WriteFile(conf, []byte(`http { access_log off; error_log stderr crit; server { listen `+addr+`; location / { return 200 "managed"; } } }`), 0600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	ready := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- runServer(conf, false, nil, &serviceLifecycle{stop: stop, started: func(err error) { ready <- err }})
	}()
	defer func() {
		close(stop)
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("service failed to stop")
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup not reported")
	}
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
}
func TestManagedRuntimeReportsStartupFailure(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	ready := make(chan error, 1)
	err = runServer(filepath.Join(t.TempDir(), "missing.conf"), false, nil, &serviceLifecycle{started: func(err error) { ready <- err }})
	if err == nil {
		t.Fatal("missing config started service")
	}
	select {
	case got := <-ready:
		if got == nil {
			t.Fatal("startup reported success")
		}
	default:
		t.Fatal("startup failure not reported")
	}
}

func TestManagedRuntimeRejectsOccupiedListener(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	path := filepath.Join(t.TempDir(), "crest.conf")
	if err = os.WriteFile(path, []byte(`http { access_log off; error_log stderr crit; server { listen `+ln.Addr().String()+`; location / { return 200 "ok"; } } }`), 0600); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	err = runServer(path, false, nil, &serviceLifecycle{started: func(err error) { ready <- err }})
	if err == nil {
		t.Fatal("occupied port started successfully")
	}
	if got := <-ready; got == nil {
		t.Fatal("SCM would receive a false Running state")
	}
	if _, err = os.Stat(path + ".ctl"); !os.IsNotExist(err) {
		t.Fatalf("failed startup left control file: %v", err)
	}
}
