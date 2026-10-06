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

// supervisorStopFallback bounds how long apply() waits for the supervisor
// (systemd / launchd)-triggered stop below to actually terminate this
// process before falling back to exiting directly. Should never be hit on
// the normal path — see the comment at the call site.
const supervisorStopFallback = 10 * time.Second

func apply(exePath, version string, assets []githubAsset, log *slog.Logger, drain func(time.Duration), _ func(), lock *updateLock, stillOK func() bool) error {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	assetName := fmt.Sprintf("vectrify-runner-%s-%s", goos, goarch)
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
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod new binary: %w", err)
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

	// ── Last look before the point of no return ───────────────────────────────
	// Download + verify take a while; if a command arrived in the meantime,
	// back out cleanly (nothing has been touched yet) and let the next check
	// try again once the runner is idle.
	if stillOK != nil && !stillOK() {
		os.Remove(tmpPath)
		return errDeferred
	}

	// ── Drain in-flight commands before exiting ───────────────────────────────
	if drain != nil {
		log.Info("auto-update: draining in-flight commands", "timeout", drainTimeout)
		drain(drainTimeout)
	}

	// Write a shell script that waits for the service to actually stop
	// (bounded poll, not a blind fixed sleep), swaps the binary, releases
	// the update lock, and restarts.
	var isStoppedCmd, forceStopCmd, startCmd string
	if goos == "darwin" {
		// `launchctl list` exits non-zero once the label is gone — which
		// only happens after `launchctl unload` below has fully removed
		// it (see the ROOT-CAUSE NOTE further down for why unload, not
		// stop, is required here).
		isStoppedCmd = "! launchctl list ai.vectrify.runner >/dev/null 2>&1"
		forceStopCmd = "launchctl unload /Library/LaunchDaemons/ai.vectrify.runner.plist 2>/dev/null || true"
		startCmd = "launchctl load /Library/LaunchDaemons/ai.vectrify.runner.plist 2>/dev/null || true"
	} else {
		isStoppedCmd = "! systemctl is-active --quiet vectrify-runner"
		forceStopCmd = "systemctl stop vectrify-runner 2>/dev/null || true"
		startCmd = "systemctl start vectrify-runner 2>/dev/null || true"
	}

	scriptPath := exePath + ".update.sh"
	script := fmt.Sprintf(
		"#!/bin/bash\n"+
			"deadline=$(( $(date +%%s) + 30 ))\n"+
			"while [ \"$(date +%%s)\" -lt \"$deadline\" ]; do\n"+
			"    if %s; then break; fi\n"+
			"    sleep 0.5\n"+
			"done\n"+
			// Idempotent safety net: if still not stopped after the poll
			// above (should not happen on the normal path — see the
			// supervisor-stop call further down), force it now rather
			// than risk `mv` failing on, or corrupting, a running binary.
			"%s\n"+
			"sleep 1\n"+
			"mv -f \"%s\" \"%s\"\n"+
			"rm -f \"%s\"\n"+
			"%s\n"+
			"rm -f \"$0\"\n",
		isStoppedCmd, forceStopCmd, tmpPath, exePath, lock.path, startCmd,
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

	// ROOT-CAUSE NOTE (read before "simplifying" this exit sequence):
	// This process must never call os.Exit on the success path without
	// first making sure the OS service supervisor (systemd on Linux,
	// launchd on macOS) knows this exit was requested, not a crash.
	// install.sh configures `Restart=always` (systemd) / `KeepAlive=true`
	// (launchd) — both mean "restart unconditionally on ANY exit" unless
	// the supervisor itself is the one that caused the exit. If this
	// process just called os.Exit(0) directly (as it used to), the
	// supervisor would restart the still-old binary within seconds, and
	// that freshly-restarted process's own startup update check would
	// detect the same new release and start a SECOND, fully independent
	// update flow — racing the first flow's detached script over the
	// exact same exePath. That happened in production (on Windows, whose
	// equivalent mechanism is `sc.exe failure ... actions=restart/...` —
	// see apply_windows.go) and corrupted the installed binary
	// (interleaved os.Create/mv from two update flows at once). The
	// update lock in lock.go makes that outcome structurally impossible
	// even if this exit sequence somehow still races, but fixing the
	// race at its source (here) is what stops the false-crash-restart,
	// and thus the wasted duplicate download/verify work, from happening
	// at all.
	//
	// systemd (Linux): per systemd.service(5) — "When the death of the
	// process is a result of systemd operation (e.g. service stop or
	// restart), the service will not be restarted" — regardless of
	// Restart=always. So asking systemd itself to stop the unit (rather
	// than just letting this process exit on its own) is sufficient:
	// `systemctl stop` delivers SIGTERM to this same process, this
	// process's EXISTING signal handler (see runInteractive in main.go,
	// used unchanged via service_other.go on this platform) already
	// correctly calls Runner.Shutdown() then os.Exit(0) — and because
	// systemd itself initiated that termination, its restart policy does
	// not fire.
	//
	// launchd (macOS): unlike systemd, a `KeepAlive=true` job (boolean,
	// not the dict form with e.g. SuccessfulExit) restarts UNCONDITIONALLY
	// on any exit, including one caused by `launchctl stop` — there is no
	// "this was an intentional stop" exception for the boolean form. The
	// only way to prevent the restart is to remove the job from launchd's
	// supervision entirely via `launchctl unload`, which is why this
	// path uses unload/load (in the script above) rather than stop/start.
	//
	// This process still exits via its own os.Exit call below in both
	// cases; the difference from before is that the supervisor already
	// knows about (systemd) or has forgotten (launchd) this exit by the
	// time it happens, so neither one restarts the still-old binary.
	if goos == "darwin" {
		log.Info("auto-update: requesting launchctl unload (so KeepAlive does not restart the old binary)", "version", version)
		if err := exec.Command("launchctl", "unload", "/Library/LaunchDaemons/ai.vectrify.runner.plist").Run(); err != nil {
			log.Warn("auto-update: launchctl unload failed; proceeding to exit directly", "err", err)
		}
	} else {
		log.Info("auto-update: requesting systemctl stop (so the exit is recognized as requested, not a crash)", "version", version)
		// Started, not waited on: `systemctl stop` blocks until the unit
		// actually stops, which happens via THIS SAME PROCESS receiving
		// SIGTERM and exiting — waiting on it here would deadlock this
		// goroutine against its own termination. Fire it and let the
		// existing SIGTERM handler (see main.go's runInteractive) do the
		// rest; the sleep below is a bounded safety net, not the primary
		// mechanism.
		if err := exec.Command("systemctl", "stop", "vectrify-runner").Start(); err != nil {
			log.Warn("auto-update: systemctl stop failed to launch; falling back to direct exit", "err", err)
		}
	}
	// Safety net only — on the normal path the supervisor-triggered
	// signal (systemd's SIGTERM via the existing handler, or launchd
	// simply no longer supervising this process) terminates this whole
	// process well before this sleep elapses. If that somehow didn't
	// happen, fall back to exiting directly rather than leaving the
	// process running with the update lock held indefinitely.
	time.Sleep(supervisorStopFallback)
	log.Warn("auto-update: supervisor did not stop this process within the fallback window; exiting directly", "timeout", supervisorStopFallback)

	log.Info("auto-update: replacement script launched, exiting", "version", version)
	// Deferred functions never run through os.Exit, so the lock is
	// deliberately NOT released here — the detached script above removes
	// it after the swap completes. Releasing it now would reopen exactly
	// the race this lock exists to close.
	os.Exit(0)
	return nil
}
