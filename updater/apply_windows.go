//go:build windows

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
)

func apply(exePath, version string, assets []githubAsset, log *slog.Logger, drain func(time.Duration), beforeExit func(), lock *updateLock) error {
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
	isService, _ := svc.IsWindowsService()
	if isService {
		log.Info("auto-update: exiting so the SCM restarts the service on the new binary", "version", version)
		os.Exit(1)
	}
	log.Info("auto-update: updated; restart the runner to run the new version", "version", version)
	os.Exit(0)
	return nil
}
