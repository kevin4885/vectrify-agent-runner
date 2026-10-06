//go:build windows

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"

	"vectrify/agent-runner/winsvc"
)

func apply(exePath, version string, assets []githubAsset, log *slog.Logger, drain func(time.Duration), beforeExit func(), lock *updateLock, stillOK func() bool) error {
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
	// concurrent attempts - however that became possible - simply can't
	// collide on the same file, full stop.
	tmpPath := fmt.Sprintf("%s.new.%d.%d", exePath, os.Getpid(), time.Now().UnixNano())

	log.Info("auto-update: downloading", "version", version, "asset", assetName)
	if err := downloadFile(binURL, tmpPath); err != nil {
		return err
	}

	// -- Verify checksum before touching the running binary -----------------
	// Required, not optional: a release missing checksums.txt aborts the
	// update rather than proceeding unverified. (The release workflow
	// always publishes one - see .github/workflows/release.yml - so a
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

	// -- Prove the candidate can actually run on this machine ---------------
	// A matching checksum does not catch a binary that antivirus mangled or
	// that is not runnable here. Find out BEFORE touching the working install.
	if err := smokeTest(tmpPath, version); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("candidate binary failed its smoke test; keeping the current version: %w", err)
	}
	log.Info("auto-update: candidate binary smoke test passed")

	// -- Make sure the service WILL come back after we exit -----------------
	// Layer 1: the SCM crash-recovery policy. Verify it (and repair it when we
	// have the rights) before relying on it. Layer 2 is the watchdog below.
	isService, _ := svc.IsWindowsService()
	serviceName := ""
	policyOK := false
	if isService {
		if name, nerr := winsvc.OwnName(); nerr != nil {
			log.Warn("auto-update: could not determine own service name", "err", nerr)
			serviceName = winsvc.DefaultName
		} else {
			serviceName = name
		}
		if _, perr := winsvc.EnsureRestartPolicy(serviceName, log); perr != nil {
			log.Warn("auto-update: SCM restart policy is not guaranteed", "err", perr)
		} else {
			policyOK = true
		}
	}

	// -- Last look before the point of no return ----------------------------
	// Download + verify + smoke test take a while; if a command arrived in the
	// meantime, back out cleanly (nothing has been touched yet) and let the
	// next check try again once the runner is idle.
	if stillOK != nil && !stillOK() {
		os.Remove(tmpPath)
		return errDeferred
	}

	// -- Drain in-flight commands before exiting ----------------------------
	if drain != nil {
		log.Info("auto-update: draining in-flight commands", "timeout", drainTimeout)
		drain(drainTimeout)
	}

	// -- Swap the binary in-process (no PowerShell, no helper script) -------
	//
	// Windows lets a running .exe be renamed, just not overwritten, so this
	// process moves itself aside and drops the verified new file at exePath.
	// See swap.go for why the old `.update.ps1` + `-ExecutionPolicy Bypass`
	// approach was removed (it matched a malware-dropper pattern and
	// contributed to Defender flagging the runner as a trojan).
	oldPath, err := swapBinary(exePath, tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return err
	}
	log.Info("auto-update: binary swapped", "version", version, "previous", oldPath)

	// Layer 2: a detached watchdog (the NEW binary) that outlives this process,
	// restarts the service itself if the SCM does not, and rolls back to
	// oldPath if the new version cannot stay up (see updater/watchdog.go).
	if isService {
		if werr := spawnWatchdog(exePath, serviceName, version, oldPath, log); werr != nil {
			if !policyOK {
				// Neither safety net exists. Exiting now would leave the
				// service dead (the exact incident this code prevents), so
				// undo the swap and stay on the working version.
				if rbErr := restorePrevious(exePath, oldPath); rbErr != nil {
					log.Error("auto-update: rollback after watchdog failure also failed", "err", rbErr)
				}
				lock.release()
				return fmt.Errorf("no safe way to restart the service after updating (SCM restart policy unverified and watchdog failed: %v); update abandoned, still on the current version", werr)
			}
			log.Warn("auto-update: watchdog could not start; relying on the SCM restart policy alone", "err", werr)
		}
	}

	// The swap is complete and exePath now holds the new version, so the
	// update lock has done its job. Release it now: os.Exit below skips
	// deferred functions, and nothing else will remove it.
	lock.release()

	// Same cleanup a service stop used to trigger: close any Playwright
	// browser we spawned so it is not orphaned when this process dies.
	if beforeExit != nil {
		beforeExit()
	}

	// ROOT-CAUSE NOTE (read before "simplifying" this exit sequence):
	// The exit below is DELIBERATELY an unacknowledged one - we do not
	// report SERVICE_STOPPED to the SCM. install.ps1 configures
	// `sc.exe failure VectrifyRunner ... actions= restart/5000/...`, so the
	// SCM treats this as a crash and restarts the service from exePath,
	// which is now the NEW binary. That restart is what brings the new
	// version up; there is no helper process to do it.
	//
	// This is the inverse of the previous design, and it is safe for the
	// reason the previous design was not: the earlier production incident
	// (v1.0.17, corrupted binary) came from the SCM restarting the still-OLD
	// binary, which re-detected the update and raced a second swap. Here
	// the file at exePath is already the new version before we exit, so a
	// restarted process has nothing left to update, and lock.go still
	// guards against any other concurrent attempt.
	//
	// Consequence: auto-update on Windows depends on the SCM failure/restart
	// policy that install.ps1 sets. If someone removes it, the service stays
	// stopped after an update until started manually.
	if isService {
		log.Info("auto-update: exiting so the SCM restarts the service on the new binary", "version", version)
		os.Exit(1)
	}
	log.Info("auto-update: updated; restart the runner to run the new version", "version", version)
	os.Exit(0)
	return nil
}
