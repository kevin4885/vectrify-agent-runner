//go:build !windows

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
)

func apply(version string, log *slog.Logger) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting exe path: %w", err)
	}

	goos   := runtime.GOOS
	goarch := runtime.GOARCH
	asset  := fmt.Sprintf("vectrify-runner-%s-%s", goos, goarch)
	url    := fmt.Sprintf("https://github.com/%s/releases/download/v%s/%s", githubRepo, version, asset)

	tmpPath := exePath + ".new"
	log.Info("auto-update: downloading", "version", version, "asset", asset)
	if err := downloadFile(url, tmpPath); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod new binary: %w", err)
	}

	// Write a shell script that stops the service, swaps the binary, and restarts.
	// Sleep 5s first so the current process has fully exited and the service manager
	// (systemd / launchd) has settled.
	var stopCmd, startCmd string
	if goos == "darwin" {
		stopCmd  = "launchctl stop ai.vectrify.runner"
		startCmd = "launchctl start ai.vectrify.runner"
	} else {
		stopCmd  = "systemctl stop vectrify-runner"
		startCmd = "systemctl start vectrify-runner"
	}

	scriptPath := exePath + ".update.sh"
	script := fmt.Sprintf(
		"#!/bin/bash\n"+
			"sleep 5\n"+
			"%s 2>/dev/null || true\n"+
			"sleep 2\n"+
			"mv -f \"%s\" \"%s\"\n"+
			"%s 2>/dev/null || true\n"+
			"rm -f \"$0\"\n",
		stopCmd, tmpPath, exePath, startCmd,
	)

	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("writing update script: %w", err)
	}

	cmd := exec.Command("bash", scriptPath)
	if err := cmd.Start(); err != nil {
		os.Remove(tmpPath)
		os.Remove(scriptPath)
		return fmt.Errorf("launching update script: %w", err)
	}

	log.Info("auto-update: replacement script launched, exiting", "version", version)
	os.Exit(0)
	return nil
}
