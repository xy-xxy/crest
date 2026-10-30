//go:build !linux && !windows

package main

import "fmt"

type unsupportedServiceManager struct{}

func newServiceManager() serviceManager { return unsupportedServiceManager{} }
func serviceUnsupported() error {
	return fmt.Errorf("service registration supports Windows SCM and Linux systemd; use foreground mode on this platform")
}
func (unsupportedServiceManager) Install(serviceSpec) error { return serviceUnsupported() }
func (unsupportedServiceManager) Uninstall() error          { return serviceUnsupported() }
func (unsupportedServiceManager) Start() error              { return serviceUnsupported() }
func (unsupportedServiceManager) Stop() error               { return serviceUnsupported() }
func (unsupportedServiceManager) Restart() error            { return serviceUnsupported() }
func (unsupportedServiceManager) Status() (string, error)   { return "", serviceUnsupported() }
func runAsService(string) error                             { return serviceUnsupported() }
func serviceWorkingDirectory(string) error                  { return nil }
