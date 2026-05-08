//go:build !windows

package main

import (
	"log/slog"

	"vectrify/agent-runner/client"
)

// runService on Linux and macOS delegates directly to runInteractive.
// On these platforms the OS service manager (systemd / launchd) handles
// lifecycle by sending SIGTERM, which runInteractive already handles correctly.
func runService(log *slog.Logger, c *client.Client) {
	runInteractive(log, c)
}
