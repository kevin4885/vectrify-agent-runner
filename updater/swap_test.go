package updater

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSwapBinary_ReplacesAndKeepsOld(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	newer := exe + ".new.1.2"
	writeFile(t, exe, "OLD")
	writeFile(t, newer, "NEW")

	oldPath, err := swapBinary(exe, newer)
	if err != nil {
		t.Fatalf("swapBinary() error = %v", err)
	}
	if got := readFile(t, exe); got != "NEW" {
		t.Errorf("exe content = %q, want NEW", got)
	}
	if got := readFile(t, oldPath); got != "OLD" {
		t.Errorf("old content = %q, want OLD", got)
	}
	if exists(newer) {
		t.Error("staged new file still exists after swap")
	}
}

func TestSwapBinary_ReplacesStaleOldFile(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	newer := exe + ".new.1.2"
	writeFile(t, exe, "OLD")
	writeFile(t, newer, "NEW")
	writeFile(t, exe+".old", "ANCIENT") // leftover from a previous update

	oldPath, err := swapBinary(exe, newer)
	if err != nil {
		t.Fatalf("swapBinary() error = %v", err)
	}
	if got := readFile(t, oldPath); got != "OLD" {
		t.Errorf("old content = %q, want OLD (stale leftover must be replaced)", got)
	}
	if got := readFile(t, exe); got != "NEW" {
		t.Errorf("exe content = %q, want NEW", got)
	}
}

func TestSwapBinary_RollsBackWhenNewFileMissing(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	writeFile(t, exe, "OLD")

	// Staged file does not exist, so step 2 (install new) fails and step 1
	// (move current aside) must be undone.
	if _, err := swapBinary(exe, exe+".new.does-not-exist"); err == nil {
		t.Fatal("swapBinary() error = nil, want error for missing staged file")
	}
	if got := readFile(t, exe); got != "OLD" {
		t.Errorf("exe content after failed swap = %q, want OLD (rollback)", got)
	}
	if exists(exe + ".old") {
		t.Error(".old should not remain after a successful rollback")
	}
}

func TestSwapBinary_MissingCurrentBinaryErrors(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	newer := exe + ".new.1.2"
	writeFile(t, newer, "NEW")

	if _, err := swapBinary(exe, newer); err == nil {
		t.Fatal("swapBinary() error = nil, want error when current binary is missing")
	}
	if got := readFile(t, newer); got != "NEW" {
		t.Errorf("staged file must be untouched on early failure, got %q", got)
	}
}

func TestRemoveLeftovers(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "vectrify-runner.exe")
	writeFile(t, exe, "CURRENT")
	writeFile(t, exe+".old", "x")
	writeFile(t, exe+".old.4242", "x")
	writeFile(t, exe+".update.ps1", "x")
	inProgress := exe + ".new.9.9" // may belong to an update running in another process
	writeFile(t, inProgress, "x")

	removeLeftovers(exe)

	for _, gone := range []string{exe + ".old", exe + ".old.4242", exe + ".update.ps1"} {
		if exists(gone) {
			t.Errorf("%s should have been removed", filepath.Base(gone))
		}
	}
	if !exists(exe) {
		t.Error("current binary must never be removed")
	}
	if !exists(inProgress) {
		t.Error(".new.* must be left alone (may be another process's in-progress update)")
	}
}