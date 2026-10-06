//go:build !windows

package main

import (
	"log/slog"

	"vectrify/agent-runner/client"
	"vectrify/agent-runner/runner"
	"vectrify/agent-runner/updater"
)

// runService on Linux and macOS delegates directly to runInteractive.
// On these platforms the OS service manager (systemd / launchd) handles
// lifecycle by sending SIGTERM, which runInteractive already handles correctly.
func runService(log *slog.Logger, c *client.Client, r *runner.Runner, uo updater.Options) {
	runInteractive(log, c, r, uo)
}
