//go:build windows

// Package winsvc holds the small amount of Windows Service Control Manager
// (SCM) plumbing the runner needs beyond what golang.org/x/sys/windows/svc
// gives it for free:
//
//   - discovering the name of the service this process is running as
//     (installs can be suffixed, e.g. "VectrifyRunner-TS");
//   - a least-privilege controller (query / start / stop) for that service,
//     used by the post-update watchdog (updater/watchdog*.go);
//   - checking and self-healing the service's crash-recovery policy
//     (recovery_windows.go), which auto-update relies on.
//
// Every SCM handle is opened with only the access rights the operation needs,
// so a runner whose service account is NOT an administrator degrades to a
// logged warning instead of failing outright.
package winsvc

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// DefaultName is the service name used by a default (un-suffixed) install,
// and the fallback when the real name cannot be discovered.
const DefaultName = "VectrifyRunner"

// OwnName returns the SCM service name of the service whose process is the
// current process, by enumerating active Win32 services and matching on PID.
// Needs only SC_MANAGER_CONNECT | SC_MANAGER_ENUMERATE_SERVICE, which every
// authenticated account has.
func OwnName() (string, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return "", fmt.Errorf("opening SCM: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	var buf []byte
	var bytesNeeded, returned, resume uint32
	for {
		var p *byte
		if len(buf) > 0 {
			p = &buf[0]
		}
		resume = 0
		err = windows.EnumServicesStatusEx(scm, windows.SC_ENUM_PROCESS_INFO,
			windows.SERVICE_WIN32, windows.SERVICE_ACTIVE,
			p, uint32(len(buf)), &bytesNeeded, &returned, &resume, nil)
		if err == nil {
			break
		}
		if err != syscall.ERROR_MORE_DATA || bytesNeeded <= uint32(len(buf)) {
			return "", fmt.Errorf("enumerating services: %w", err)
		}
		buf = make([]byte, bytesNeeded)
	}
	if returned == 0 || len(buf) == 0 {
		return "", fmt.Errorf("no active services returned")
	}

	pid := uint32(os.Getpid())
	entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), int(returned))
	for _, e := range entries {
		if e.ServiceStatusProcess.ProcessId == pid {
			return windows.UTF16PtrToString(e.ServiceName), nil
		}
	}
	return "", fmt.Errorf("pid %d is not an active service process", pid)
}

// Controller queries, starts and stops one named service.
type Controller struct {
	Name string
}

// open opens the service with exactly the requested access mask.
func (c Controller) open(access uint32) (*mgr.Service, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, fmt.Errorf("opening SCM: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	name, err := syscall.UTF16PtrFromString(c.Name)
	if err != nil {
		return nil, err
	}
	h, err := windows.OpenService(scm, name, access)
	if err != nil {
		return nil, fmt.Errorf("opening service %q (access 0x%x): %w", c.Name, access, err)
	}
	return &mgr.Service{Name: c.Name, Handle: h}, nil
}

// Status returns the service's current SCM status (state + process id).
func (c Controller) Status() (svc.Status, error) {
	s, err := c.open(windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return svc.Status{}, err
	}
	defer s.Close()
	return s.Query()
}

// Start asks the SCM to start the service. "Already running" is success.
func (c Controller) Start() error {
	s, err := c.open(windows.SERVICE_START | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Start(); err != nil && err != windows.ERROR_SERVICE_ALREADY_RUNNING {
		return fmt.Errorf("starting service %q: %w", c.Name, err)
	}
	return nil
}

// Stop asks the SCM to stop the service. "Not active" is success.
func (c Controller) Stop() error {
	s, err := c.open(windows.SERVICE_STOP | windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer s.Close()
	if _, err := s.Control(svc.Stop); err != nil && err != windows.ERROR_SERVICE_NOT_ACTIVE {
		return fmt.Errorf("stopping service %q: %w", c.Name, err)
	}
	return nil
}
