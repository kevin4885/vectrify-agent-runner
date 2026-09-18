package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockFileName is created next to the running binary while an update is in
// progress. Its mere existence (not its content) is the lock; content is
// only for a human reading it during an incident.
const lockFileName = ".vectrify-runner-update.lock"

// staleLockAge bounds how long a lock is honored before a later update
// cycle is allowed to reclaim it. A genuine update (download + verify +
// drain + swap) always completes in well under a minute even on a slow
// connection; 15 minutes is a deliberately generous ceiling so this only
// ever fires to recover from a lock abandoned by a process that crashed
// mid-update (e.g. the machine lost power), not to race a real one.
const staleLockAge = 15 * time.Minute

// updateLock represents a held update lock. The zero value is not valid;
// obtain one via acquireUpdateLock.
type updateLock struct {
	path string
}

// ROOT-CAUSE NOTE (read before touching this file):
// Without this lock, two runner processes on the same machine can run
// checkAndApply concurrently and both attempt to download to and swap the
// same binary path. This is not a theoretical race: it happened in
// production. The auto-updater's apply() historically exited the running
// process (os.Exit(0)) to hand off to a detached swap script, without
// first telling the OS service manager this exit was intentional. Every
// mainstream service supervisor (Windows SCM's `sc.exe failure` restart
// actions, systemd's `Restart=always`, launchd's `KeepAlive=true` — see
// install.ps1 / install.sh) treats an unexpected process exit as a crash
// and auto-restarts the OLD binary within seconds. That freshly-restarted
// process immediately re-runs its own startup update check (see
// updater.Start), finds the same new release still available, and starts
// a SECOND, fully independent update flow — downloading to and moving the
// exact same file paths the FIRST flow's detached script is still mid-way
// through swapping. The two flows' os.Create() (truncates) and
// os.Rename()/Move-Item calls interleave with no coordination whatsoever,
// which is exactly how a customer's installed binary ended up truncated /
// corrupted ("... is not a valid Win32 application" on next start).
//
// This lock makes that outcome structurally impossible, regardless of
// whatever OS-level false-restart races still exist (see
// reportStopping in updater.go and the platform-specific apply_*.go files
// for the complementary fix that reduces how often that restart happens in
// the first place — but this lock is the fix that guarantees correctness
// even if that mitigation is imperfect on some platform).
//
// acquireUpdateLock attempts to take the exclusive update lock for the
// binary at exePath. Returns:
//   - (lock, nil) on success — caller now holds the lock and must call
//     lock.release() exactly once, on every code path, including right
//     before any os.Exit call (deferred releases never run through
//     os.Exit).
//   - (nil, nil) if another update is already in progress (lock held and
//     not stale) — caller should skip this update cycle entirely, not
//     treat it as an error.
//   - (nil, err) on an unexpected I/O error acquiring the lock.
func acquireUpdateLock(exePath string) (*updateLock, error) {
	lockPath := filepath.Join(filepath.Dir(exePath), lockFileName)

	ok, err := tryCreateLock(lockPath)
	if err != nil {
		return nil, err
	}
	if ok {
		return &updateLock{path: lockPath}, nil
	}

	// Lock already exists. If it's stale (abandoned by a process that died
	// mid-update, e.g. power loss), reclaim it. If it's fresh, another
	// update is genuinely in progress right now — back off.
	if !lockIsStale(lockPath) {
		return nil, nil
	}
	_ = os.Remove(lockPath) // best-effort; if this fails, the retry below just fails closed too
	ok, err = tryCreateLock(lockPath)
	if err != nil {
		return nil, err
	}
	if ok {
		return &updateLock{path: lockPath}, nil
	}
	// Lost the race to reclaim a stale lock to some other process doing
	// the same thing at the same instant — treat that as "in progress",
	// not an error. Vanishingly rare, but correctness must not depend on
	// which side of this narrow window we land on.
	return nil, nil
}

// tryCreateLock attempts to atomically create lockPath, failing if it
// already exists. O_EXCL make-or-fail is atomic on every platform this
// binary targets (Windows, Linux, macOS), which is what makes this safe as
// a lock primitive without any additional OS-specific coordination.
func tryCreateLock(lockPath string) (bool, error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating update lock %s: %w", lockPath, err)
	}
	defer f.Close()
	// Content is diagnostic only (visible to a human during an incident);
	// staleness is judged purely from the file's mtime, not this content,
	// so no parsing of it is ever required for correctness.
	fmt.Fprintf(f, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	return true, nil
}

// lockIsStale reports whether the lock at lockPath is older than
// staleLockAge. Treats a missing/unreadable lock file as stale — if it
// vanished between our failed create and this check (e.g. the holder
// finished and released it), there's nothing left to protect.
func lockIsStale(lockPath string) bool {
	info, err := os.Stat(lockPath)
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) > staleLockAge
}

// release removes the lock file. Safe to call on a nil receiver (mirrors
// the procTree pattern in executor/proc_*.go) so callers can unconditionally
// call it in a defer without a nil check at every call site.
func (l *updateLock) release() {
	if l == nil {
		return
	}
	_ = os.Remove(l.path)
}
