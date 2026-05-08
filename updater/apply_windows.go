//go:build windows

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
)

func apply(version string, log *slog.Logger) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting exe path: %w", err)
	}

	url     := fmt.Sprintf("https://github.com/%s/releases/download/v%s/vectrify-runner-windows-amd64.exe", githubRepo, version)
	tmpPath := exePath + ".new"

	log.Info("auto-update: downloading", "version", version)
	if err := downloadFile(url, tmpPath); err != nil {
		return err
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
