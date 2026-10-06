//go:build !windows

package updater

import "log/slog"

// RunWatchdogProcess exists only so main.go compiles everywhere. The
// post-update watchdog is a Windows-only mechanism: on Linux/macOS the update
// script restarts the service itself (apply_other.go).
func RunWatchdogProcess(_, _, _, _ string, _ uint32, log *slog.Logger) WatchdogOutcome {
	log.Warn("post-update watchdog is only supported on Windows; nothing to do")
	return OutcomeGaveUp
}
