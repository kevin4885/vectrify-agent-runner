//go:build !windows

package updater

// isEnvStartError: the watchdog is Windows-only; present so the shared
// watchdog.go compiles everywhere.
func isEnvStartError(error) bool { return false }
