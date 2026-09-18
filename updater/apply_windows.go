//go:build windows

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/windows/svc"
)

// scmStopFallback bounds how long apply() waits for the SCM-triggered stop
// (see below) to actually terminate this process before falling back to
// exiting directly. Should never be hit on the normal path — see the
// comment at the call site.
const scmStopFallback = 10 * time.Second

func apply(exePath, version string, assets []githubAsset, log *slog.Logger, drain func(time.Duration), lock *updateLock) error {
	assetName := "vectrify-runner-windows-amd64.exe"
	manifestName := "checksums.txt"

	binURL, err := assetURL(assets, assetName)
	if err != nil {
		return err
	}

	// Unique per attempt (pid + timestamp), not a fixed ".new" suffix: even
	// with the update lock in lock.go making concurrent updates impossible
	// in practice, a fixed shared path is one more thing that would have
	// to go right for that guarantee to hold. A unique path means two
	// concurrent attempts — however that became possible — simply can't
	// collide on the same file, full stop.
	tmpPath := fmt.Sprintf("%s.new.%d.%d", exePath, os.Getpid(), time.Now().UnixNano())

	log.Info("auto-update: downloading", "version", version, "asset", assetName)
	if err := downloadFile(binURL, tmpPath); err != nil {
		return err
	}

	// ── Verify checksum before touching the running binary ───────────────────
	// Required, not optional: a release missing checksums.txt aborts the
	// update rather than proceeding unverified. (The release workflow
	// always publishes one — see .github/workflows/release.yml — so a
	// release without one indicates something wrong with that release,
	// which is exactly when this should refuse to install it.)
	manifestURL, err := assetURL(assets, manifestName)
	if err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("no checksum manifest in release %s; refusing to install unverified binary: %w", version, err)
	}
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

	// Write a PowerShell script that waits for the service to actually
	// reach Stopped (bounded poll, not a blind fixed sleep — see below for
	// why this process's own exit is what actually stops it), swaps the
	// binary, releases the update lock, and restarts.
	scriptPath := exePath + ".update.ps1"
	script := fmt.Sprintf(
		"$deadline = (Get-Date).AddSeconds(30)\r\n"+
			"while ((Get-Date) -lt $deadline) {\r\n"+
			"    $svc = Get-Service -Name VectrifyRunner -ErrorAction SilentlyContinue\r\n"+
			"    if (-not $svc -or $svc.Status -eq 'Stopped') { break }\r\n"+
			"    Start-Sleep -Milliseconds 500\r\n"+
			"}\r\n"+
			// Idempotent safety net: if the service is somehow still not
			// stopped after the poll above (should not happen on the
			// normal path — see the sc.exe stop call further down), force
			// it now rather than risk Move-Item failing on a locked file.
			"sc.exe stop VectrifyRunner | Out-Null\r\n"+
			"Start-Sleep -Seconds 1\r\n"+
			"Move-Item -Force \"%s\" \"%s\"\r\n"+
			"Remove-Item -Force \"%s\" -ErrorAction SilentlyContinue\r\n"+
			"sc.exe start VectrifyRunner | Out-Null\r\n"+
			"Remove-Item -LiteralPath $MyInvocation.MyCommand.Path -Force\r\n",
		tmpPath, exePath, lock.path,
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

	// ROOT-CAUSE NOTE (read before "simplifying" this exit sequence):
	// This process must never call os.Exit on the success path without
	// first making sure the Windows Service Control Manager knows this
	// exit was requested, not a crash. install.ps1 configures
	// `sc.exe failure VectrifyRunner ... actions=restart/...` — SCM policy
	// that auto-restarts the OLD binary within seconds of ANY exit it did
	// not itself request. If this process just called os.Exit(0) directly
	// (as it used to), the SCM would treat that as a crash, restart the
	// still-old binary, and that freshly-restarted process's own startup
	// update check would detect the same new release and start a SECOND,
	// fully independent update flow — racing the first flow's detached
	// script over the exact same exePath. That happened in production and
	// corrupted the installed binary (interleaved os.Create/Move-Item from
	// two update flows at once). The update lock in lock.go makes that
	// outcome structurally impossible even if this exit sequence somehow
	// still races, but fixing the race at its source (here) is what stops
	// the false-crash-restart, and thus the wasted duplicate download/
	// verify work, from happening at all.
	//
	// The fix: when running as an installed Windows service, ask the SCM
	// to stop the service via `sc.exe stop`. That delivers a genuine Stop
	// control request back to THIS SAME PROCESS's own service dispatch
	// loop (service_windows.go's Execute), which already correctly
	// reports StopPending to the SCM before exiting — so the ensuing exit
	// is now an SCM-acknowledged stop, not a surprise termination, and the
	// failure/restart policy does not fire. This process does not need to
	// call os.Exit itself in that case: Execute's own stop-handling
	// goroutine will terminate the whole process once the SCM delivers the
	// control request (typically near-instant).
	//
	// When NOT running as a service (e.g. a developer running the binary
	// directly in a terminal via --config during local testing), there is
	// no SCM watching this process and no failure policy to misfire, so
	// the original direct os.Exit(0) is correct and unchanged for that
	// case.
	isService, _ := svc.IsWindowsService()
	if isService {
		log.Info("auto-update: requesting SCM stop (so the exit is recognized as requested, not a crash)", "version", version)
		if err := exec.Command("sc.exe", "stop", "VectrifyRunner").Run(); err != nil {
			log.Warn("auto-update: sc.exe stop failed; falling back to direct exit", "err", err)
		}
		// Safety net only — on the normal path the SCM's Stop control
		// reaches Execute's dispatch loop and THAT goroutine calls
		// os.Exit, killing this whole process (including this goroutine)
		// well before this sleep elapses. If that somehow didn't happen
		// (sc.exe stop itself failed, or some other unexpected mishap),
		// fall back to exiting directly rather than leaving the process
		// running with the update lock held indefinitely.
		time.Sleep(scmStopFallback)
		log.Warn("auto-update: SCM did not stop this process within the fallback window; exiting directly", "timeout", scmStopFallback)
	}

	log.Info("auto-update: replacement script launched, exiting", "version", version)
	// Deferred functions never run through os.Exit, so the lock is
	// deliberately NOT released here — the detached script above removes
	// it after the swap completes. Releasing it now would reopen exactly
	// the race this lock exists to close.
	os.Exit(0)
	return nil
}
