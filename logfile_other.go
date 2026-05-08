//go:build !windows

package main

// defaultLogFile on Linux and macOS always returns empty — systemd captures
// stdout to the journal and launchd writes to the configured log file.
func defaultLogFile() string {
	return ""
}
