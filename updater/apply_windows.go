//go:build windows

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

func apply(version string, assets []githubAsset, log *slog.Logger, drain func(time.Duration)) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting exe path: %w", err)
	}

	assetName   := "vectrify-runner-windows-amd64.exe"
	manifestName := "checksums.txt"

	binURL, err := assetURL(assets, assetName)
	if err != nil {
		return err
	}
	manifestURL, err := assetURL(assets, manifestName)
	if err != nil {
		return err
	}

	tmpPath := exePath + ".new"

	log.Info("auto-update: downloading", "version", version, "asset", assetName)
	if err := downloadFile(binURL, tmpPath); err != nil {
		return err
	}

	// ── Verify checksum before touching the running binary ───────────────────
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

	// ── Drain in-flight commands before exiting ───────────────────────────────
	if drain != nil {
		log.Info("auto-update: draining in-flight commands", "timeout", drainTimeout)
		drain(drainTimeout)
	}

	// Write a PowerShell script that stops the service, swaps the binary, and restarts.
	// Sleep 5s first so the current process has fully exited and the SCM has settled.
	scriptPath := exePath + ".update.ps1"
	script := fmt.Sprintf(
		"Start-Sleep -Seconds 5\r\n"+
			"sc.exe stop VectrifyRunner | Out-Null\r\n"+
			"Start-Sleep -Seconds 2\r\n"+
			"Move-Item -Force \"%s\" \"%s\"\r\n"+
			"sc.exe start VectrifyRunner | Out-Null\r\n"+
			"Remove-Item -LiteralPath $MyInvocation.MyCommand.Path -Force\r\n",
		tmpPath, exePath,
	)

	if err := os.WriteFile(scriptPath, []byte(script), 0644); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("writing update script: %w", err)
	}

	cmd := exec.Command("powershell", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	if err := cmd.Start(); err != nil {
		os.Remove(tmpPath)
		os.Remove(scriptPath)
		return fmt.Errorf("launching update script: %w", err)
	}

	log.Info("auto-update: replacement script launched, exiting", "version", version)
	os.Exit(0)
	return nil
}
