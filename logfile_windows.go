//go:build windows

package main

import "golang.org/x/sys/windows/svc"

// defaultLogFile returns the default log file path when running as a Windows
// Service (stdout is discarded by the SCM). Returns empty string when running
// interactively so logs still go to the terminal.
func defaultLogFile() string {
	isService, err := svc.IsWindowsService()
	if err == nil && isService {
		return `C:\ProgramData\VectrifyRunner\vectrify-runner.log`
	}
	return ""
}
