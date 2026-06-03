//go:build !windows

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func apply(version string, assets []githubAsset, log *slog.Logger, drain func(time.Duration)) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting exe path: %w", err)
	}

	goos        := runtime.GOOS
	goarch      := runtime.GOARCH
	assetName   := fmt.Sprintf("vectrify-runner-%s-%s", goos, goarch)
	manifestName := "checksums.txt"

	binURL, err := assetURL(assets, assetName)
	if err != nil {
		return err
	}

	tmpPath := exePath + ".new"
	log.Info("auto-update: downloading", "version", version, "asset", assetName)
	if err := downloadFile(binURL, tmpPath); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod new binary: %w", err)
	}

	// ── Verify checksum before touching the running binary ───────────────────
	// Checksum verification is optional: if the release does not include a
	// checksums.txt manifest we log a warning and proceed.  This keeps updates
	// working for releases that pre-date the manifest, while still verifying
	// integrity whenever the manifest is present.
	manifestURL, err := assetURL(assets, manifestName)
	if err != nil {
		log.Warn("auto-update: no checksum manifest in release, skipping verification", "version", version)
	} else {
		log.Info("auto-update: verifying checksum")
		sums, err := downloadSHA256Manifest(manifestURL)
		if err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("fetching checksum manifest: %w", err)
		}
		expected, ok := sums[assetName]
		if !ok {
			os.Remove(tmpPath)
			return fmt.Errorf("checksum for %q not found in manifest", assetName)
		}
		if err := verifySHA256(tmpPath, expected); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("checksum verification failed: %w", err)
		}
		log.Info("auto-update: checksum verified")
	}

	// ── Drain in-flight commands before exiting ───────────────────────────────
	if drain != nil {
		log.Info("auto-update: draining in-flight commands", "timeout", drainTimeout)
		drain(drainTimeout)
	}

	// Write a shell script that stops the service, swaps the binary, and restarts.
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
