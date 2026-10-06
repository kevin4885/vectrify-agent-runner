package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// swapBinary replaces the executable at exePath with the already-downloaded,
// already-checksum-verified file at newPath, in-process, with no helper
// script. Returns the path the previous binary was moved to.
//
// WHY THIS EXISTS (read before reintroducing a helper script):
// The Windows updater used to write a `.update.ps1` and launch it with
// `powershell -ExecutionPolicy Bypass -File`. "Unsigned exe that downloads
// a binary, drops a PowerShell script next to itself and runs it with
// ExecutionPolicy Bypass" is a textbook dropper pattern, and it was one of
// the behaviours that got the runner flagged by Microsoft Defender's ML
// classifier (Trojan:Win32/Bearfoos.A!ml). Windows does allow a running
// .exe to be RENAMED (just not overwritten or deleted), so the swap needs
// no external process at all:
//
//  1. rename the running exe            -> exe.old   (allowed while running)
//  2. rename the verified new file      -> exe
//  3. caller exits; the service supervisor starts the NEW file at exePath
//
// If step 2 fails, step 1 is undone so the install is never left without a
// binary at exePath.
//
// The function is deliberately platform-neutral (plain os.Rename) so it is
// unit-tested on every OS; only apply_windows.go calls it in production.
func swapBinary(exePath, newPath string) (oldPath string, err error) {
	oldPath = exePath + ".old"

	// A previous update's leftover. It is normally deletable (that process
	// is long gone), but if something still holds it open fall back to a
	// unique name rather than failing the whole update.
	if _, statErr := os.Stat(oldPath); statErr == nil {
		if rmErr := os.Remove(oldPath); rmErr != nil {
			oldPath = fmt.Sprintf("%s.old.%d", exePath, os.Getpid())
			_ = os.Remove(oldPath)
		}
	}

	if err := os.Rename(exePath, oldPath); err != nil {
		return "", fmt.Errorf("moving current binary aside: %w", err)
	}

	if err := os.Rename(newPath, exePath); err != nil {
		// Roll back so exePath is never left empty.
		if rbErr := os.Rename(oldPath, exePath); rbErr != nil {
			return "", fmt.Errorf("installing new binary: %v; ROLLBACK ALSO FAILED (previous binary is at %s): %w", err, oldPath, rbErr)
		}
		return "", fmt.Errorf("installing new binary (rolled back): %w", err)
	}

	return oldPath, nil
}

// removeLeftovers best-effort deletes files a previous update left next to
// exePath: the moved-aside old binaries (exe.old, exe.old.<pid>) and the
// legacy exe.update.ps1 written by pre-1.x-hardening updaters. Never
// touches exe.new.* — that may belong to an update in progress in another
// process (see lock.go).
func removeLeftovers(exePath string) {
	_ = os.Remove(exePath + ".old")
	_ = os.Remove(exePath + ".update.ps1")
	if matches, err := filepath.Glob(exePath + ".old.*"); err == nil {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
	if matches, err := filepath.Glob(exePath + ".bad.*"); err == nil {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}
}

// restorePrevious undoes swapBinary: it moves the (bad) binary now at exePath
// aside to a unique "<exe>.bad.<unixnano>" name and puts the previous binary
// back at exePath. Used by the post-update watchdog when the new version
// cannot stay up. Like swapBinary it never leaves exePath without a binary:
// if installing the previous one fails, the bad one is moved back.
func restorePrevious(exePath, oldPath string) error {
	if _, err := os.Stat(oldPath); err != nil {
		return fmt.Errorf("previous binary %s is not available: %w", oldPath, err)
	}
	badPath := fmt.Sprintf("%s.bad.%d", exePath, time.Now().UnixNano())
	if err := os.Rename(exePath, badPath); err != nil {
		return fmt.Errorf("moving bad binary aside: %w", err)
	}
	if err := os.Rename(oldPath, exePath); err != nil {
		if rbErr := os.Rename(badPath, exePath); rbErr != nil {
			return fmt.Errorf("restoring previous binary: %v; ROLLBACK ALSO FAILED (bad binary is at %s): %w", err, badPath, rbErr)
		}
		return fmt.Errorf("restoring previous binary (undone): %w", err)
	}
	return nil
}
