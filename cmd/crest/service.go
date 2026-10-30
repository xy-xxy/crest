package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const serviceName = "crest"

type serviceSpec struct {
	Name             string   `json:"name"`
	Executable       string   `json:"executable"`
	Arguments        []string `json:"arguments"`
	WorkingDirectory string   `json:"working_directory"`
	AutoStart        bool     `json:"auto_start"`
}
type serviceManager interface {
	Install(serviceSpec) error
	Uninstall() error
	Start() error
	Stop() error
	Restart() error
	Status() (string, error)
}

func dispatchServiceCommand(args []string, out io.Writer) (bool, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return false, nil
	}
	return true, executeServiceCommand(args, out, newServiceManager(), prepareService, runAsService)
}
func executeServiceCommand(args []string, out io.Writer, manager serviceManager, prepare func(string) (serviceSpec, error), run func(string) error) error {
	action := args[0]
	switch action {
	case "install", "uninstall", "start", "stop", "restart", "status", "run-service":
	default:
		return fmt.Errorf("unknown command %q (use install, uninstall, start, stop, restart or status)", action)
	}
	fs := flag.NewFlagSet("crest "+action, flag.ContinueOnError)
	fs.SetOutput(out)
	var config string
	var dryRun, start bool
	if action == "install" || action == "run-service" {
		fs.StringVar(&config, "c", "", "configuration file (defaults to crest.yml/crest.yaml/crest.conf)")
	}
	if action == "install" {
		fs.BoolVar(&dryRun, "dry-run", false, "print the registration plan without installing")
		fs.BoolVar(&start, "start", false, "start the service immediately after installation")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if action == "run-service" {
		if config == "" || !filepath.IsAbs(config) {
			return errors.New("run-service requires an absolute -c configuration path")
		}
		return run(config)
	}
	var err error
	switch action {
	case "install":
		spec, e := prepare(config)
		if e != nil {
			return e
		}
		if dryRun {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(spec)
		}
		if err = manager.Install(spec); err != nil {
			break
		}
		fmt.Fprintf(out, "Service %s installed (automatic startup).\nExecutable: %s\nConfig: %s\n", serviceName, spec.Executable, spec.Arguments[len(spec.Arguments)-1])
		if start {
			if err = manager.Start(); err != nil {
				return fmt.Errorf("service installed but could not start; inspect log/console.log: %w", err)
			}
			fmt.Fprintln(out, "Service crest started.")
		} else {
			fmt.Fprintln(out, "Run 'crest start' to start it now.")
		}
		return nil
	case "uninstall":
		err = manager.Uninstall()
	case "start":
		err = manager.Start()
	case "stop":
		err = manager.Stop()
	case "restart":
		err = manager.Restart()
	case "status":
		var status string
		status, err = manager.Status()
		if err == nil {
			fmt.Fprintln(out, "crest:", status)
		}
		return err
	}
	if err != nil {
		return fmt.Errorf("service %s: %w (installation/control may require Administrator or root privileges)", action, err)
	}
	fmt.Fprintf(out, "Service crest: %s completed.\n", action)
	return nil
}
func prepareService(config string) (serviceSpec, error) {
	resolved, _, err := resolveConfigPath(config)
	if err != nil {
		return serviceSpec{}, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return serviceSpec{}, err
	}
	if _, err = loadConfig(resolved); err != nil {
		return serviceSpec{}, fmt.Errorf("invalid service configuration: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return serviceSpec{}, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return serviceSpec{}, err
	}
	return makeServiceSpec(exe, resolved)
}
func makeServiceSpec(exe, config string) (serviceSpec, error) {
	if !filepath.IsAbs(exe) || !filepath.IsAbs(config) {
		return serviceSpec{}, errors.New("service executable and configuration paths must be absolute")
	}
	for _, p := range []string{exe, config} {
		for _, r := range p {
			if r < 32 || r == 127 {
				return serviceSpec{}, errors.New("service paths must not contain control characters")
			}
		}
	}
	return serviceSpec{Name: serviceName, Executable: exe, Arguments: []string{"run-service", "-c", config}, WorkingDirectory: filepath.Dir(config), AutoStart: true}, nil
}

// systemd interprets percent specifiers in paths, and dollar variables in
// Exec* arguments. No shell is involved; quote each argument independently.
func systemdQuote(s string, execArg bool) string {
	s = strings.ReplaceAll(s, "%", "%%")
	if execArg {
		s = strings.ReplaceAll(s, "$", "$$")
	}
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + s + `"`
}
func systemdUnit(s serviceSpec) string {
	args := []string{systemdQuote(s.Executable, false)}
	for _, a := range s.Arguments {
		args = append(args, systemdQuote(a, true))
	}
	return fmt.Sprintf(`[Unit]
Description=crest web server and reverse proxy
Wants=network-online.target
After=network-online.target

[Service]
Type=notify
NotifyAccess=main
TimeoutStartSec=30s
WorkingDirectory=%s
ExecStart=%s
ExecReload=%s "-s" "reload" "-c" %s
Restart=on-failure
RestartSec=3s
TimeoutStopSec=15s

[Install]
WantedBy=multi-user.target
`, strings.ReplaceAll(s.WorkingDirectory, "%", "%%"), strings.Join(args, " "), systemdQuote(s.Executable, false), systemdQuote(s.Arguments[len(s.Arguments)-1], true))
}
