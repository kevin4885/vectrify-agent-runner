package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeExePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "vectrify-runner")
}

func TestAcquireUpdateLock_FirstCallSucceeds(t *testing.T) {
	exePath := fakeExePath(t)
	lock, err := acquireUpdateLock(exePath)
	if err != nil {
		t.Fatalf("acquireUpdateLock() error = %v", err)
	}
	if lock == nil {
		t.Fatal("acquireUpdateLock() returned nil lock on first call, want a held lock")
	}
	defer lock.release()

	if _, err := os.Stat(lock.path); err != nil {
		t.Errorf("lock file does not exist at %s: %v", lock.path, err)
	}
}

// This is the core regression test for the production bug: two "processes"
// (simulated by two independent acquireUpdateLock calls against the same
// exePath) must never both believe they hold the update lock at the same
// time — that double-hold is exactly what let two update flows race over
// the same binary path and corrupt it.
func TestAcquireUpdateLock_SecondCallBlockedWhileFirstHeld(t *testing.T) {
	exePath := fakeExePath(t)

	first, err := acquireUpdateLock(exePath)
	if err != nil {
		t.Fatalf("first acquireUpdateLock() error = %v", err)
	}
	if first == nil {
		t.Fatal("first acquireUpdateLock() returned nil, want a held lock")
	}
	defer first.release()

	second, err := acquireUpdateLock(exePath)
	if err != nil {
		t.Fatalf("second acquireUpdateLock() error = %v", err)
	}
	if second != nil {
		t.Fatal("second acquireUpdateLock() succeeded while first lock is still held — this is the exact double-update race that corrupted a production binary")
	}
}

func TestAcquireUpdateLock_ReleasedThenReacquirable(t *testing.T) {
	exePath := fakeExePath(t)

	first, err := acquireUpdateLock(exePath)
	if err != nil {
		t.Fatalf("first acquireUpdateLock() error = %v", err)
	}
	first.release()

	second, err := acquireUpdateLock(exePath)
	if err != nil {
		t.Fatalf("second acquireUpdateLock() error = %v", err)
	}
	if second == nil {
		t.Fatal("second acquireUpdateLock() returned nil after first was released, want a held lock")
	}
	defer second.release()
}

// A lock abandoned by a process that died mid-update (e.g. power loss)
// must not permanently wedge auto-update on that machine — a stale lock is
// reclaimed rather than blocking forever.
func TestAcquireUpdateLock_StaleLockIsReclaimed(t *testing.T) {
	exePath := fakeExePath(t)
	lockPath := filepath.Join(filepath.Dir(exePath), lockFileName)

	if err := os.WriteFile(lockPath, []byte("pid=99999\nstarted=2000-01-01T00:00:00Z\n"), 0644); err != nil {
		t.Fatalf("writing fake stale lock: %v", err)
	}
	staleTime := time.Now().Add(-2 * staleLockAge)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatalf("backdating lock mtime: %v", err)
	}

	lock, err := acquireUpdateLock(exePath)
	if err != nil {
		t.Fatalf("acquireUpdateLock() error = %v", err)
	}
	if lock == nil {
		t.Fatal("acquireUpdateLock() returned nil for a stale lock, want it reclaimed")
	}
	defer lock.release()
}

// A fresh (non-stale) lock must NOT be reclaimed — only genuinely
// abandoned locks are. This is the difference between "another update is
// legitimately in progress right now" (must back off) and "a process died
// a while ago and left this behind" (safe to reclaim).
func TestAcquireUpdateLock_FreshLockIsNotReclaimed(t *testing.T) {
	exePath := fakeExePath(t)
	lockPath := filepath.Join(filepath.Dir(exePath), lockFileName)

	if err := os.WriteFile(lockPath, []byte("pid=12345\n"), 0644); err != nil {
		t.Fatalf("writing fake fresh lock: %v", err)
	}
	// mtime defaults to now — well within staleLockAge.

	lock, err := acquireUpdateLock(exePath)
	if err != nil {
		t.Fatalf("acquireUpdateLock() error = %v", err)
	}
	if lock != nil {
		t.Fatal("acquireUpdateLock() reclaimed a fresh lock — should have backed off instead")
	}
}

func TestUpdateLock_ReleaseOnNilIsSafe(t *testing.T) {
	var lock *updateLock
	lock.release() // must not panic
}

func TestLockIsStale(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.lock")
	if err := os.WriteFile(fresh, []byte("x"), 0644); err != nil {
		t.Fatalf("writing fresh lock: %v", err)
	}
	if lockIsStale(fresh) {
		t.Error("lockIsStale(fresh) = true, want false")
	}

	stale := filepath.Join(dir, "stale.lock")
	if err := os.WriteFile(stale, []byte("x"), 0644); err != nil {
		t.Fatalf("writing stale lock: %v", err)
	}
	staleTime := time.Now().Add(-2 * staleLockAge)
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatalf("backdating mtime: %v", err)
	}
	if !lockIsStale(stale) {
		t.Error("lockIsStale(stale) = false, want true")
	}

	missing := filepath.Join(dir, "does-not-exist.lock")
	if !lockIsStale(missing) {
		t.Error("lockIsStale(missing) = false, want true (a vanished lock has nothing left to protect)")
	}
}
