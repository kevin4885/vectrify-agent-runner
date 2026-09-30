package executor

import (
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

func newTestProcessManager(t *testing.T, maxProcesses int, maxAge time.Duration) (*ProcessManager, string) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	m := NewProcessManager(dir, maxProcesses, maxAge, log)
	t.Cleanup(m.Shutdown)
	return m, dir
}

// longRunningCommand returns a shell command (platform-appropriate) that
// sleeps far longer than any single test needs, so a process started with
// it is still safely "running" for every assertion a test makes before its
// own cleanup (Stop or t.Cleanup(m.Shutdown)) ends it.
func longRunningCommand() string {
	if runtime.GOOS == "windows" {
		return "Start-Sleep -Seconds 60"
	}
	return "sleep 60"
}

// quickExitCommand returns a shell command that prints marker to
// stdout/stderr (both, so it's visible in the combined log regardless of
// which stream a test checks) and exits immediately with exitCode.
func quickExitCommand(marker string, exitCode int) string {
	if runtime.GOOS == "windows" {
		return "Write-Output '" + marker + "'; exit " + itoa(exitCode)
	}
	return "echo '" + marker + "'; exit " + itoa(exitCode)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// ─────────────────────────────────────────────────────────────────────────────
// Start / List / Stop
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_StartListStop(t *testing.T) {
	m, dir := newTestProcessManager(t, 5, time.Hour)

	info, err := m.Start("p1", longRunningCommand(), "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !info.Running {
		t.Errorf("Start() returned Running=false for a long-running command")
	}
	if info.PID == 0 {
		t.Errorf("Start() returned PID=0")
	}
	if info.WorkingDir != dir {
		t.Errorf("Start() WorkingDir = %q, want default workspaceRoot %q", info.WorkingDir, dir)
	}

	list := m.List()
	if len(list) != 1 || list[0].ID != "p1" {
		t.Fatalf("List() = %+v, want exactly one entry with ID=p1", list)
	}

	if !m.Stop("p1") {
		t.Errorf("Stop(p1) = false, want true (process was running)")
	}
	if m.Stop("p1") {
		t.Errorf("second Stop(p1) = true, want false (already stopped/removed)")
	}

	list = m.List()
	if len(list) != 0 {
		t.Errorf("List() after Stop = %+v, want empty", list)
	}
}

func TestProcessManager_DuplicateProcessIDRejected(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)

	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Fatalf("first Start() error = %v", err)
	}
	if _, err := m.Start("p1", longRunningCommand(), ""); err == nil {
		t.Errorf("second Start() with the same process_id should have failed")
	}
	m.Stop("p1")

	// After stopping, the id is free again.
	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Errorf("Start() after stopping the same process_id should succeed, got error = %v", err)
	}
}

func TestProcessManager_PoolLimitEnforced(t *testing.T) {
	m, _ := newTestProcessManager(t, 1, time.Hour)

	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Fatalf("Start(p1) error = %v", err)
	}
	if _, err := m.Start("p2", longRunningCommand(), ""); err == nil {
		t.Fatalf("Start(p2) should have failed: pool limit (1) already reached by p1")
	}

	m.Stop("p1")
	if _, err := m.Start("p2", longRunningCommand(), ""); err != nil {
		t.Fatalf("Start(p2) after stopping p1 should succeed, got error = %v", err)
	}
}

func TestProcessManager_RequiredFields(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)

	if _, err := m.Start("", longRunningCommand(), ""); err == nil {
		t.Errorf("Start() with empty process_id should have failed")
	}
	if _, err := m.Start("p1", "", ""); err == nil {
		t.Errorf("Start() with empty command should have failed")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The core behavior this tool exists for: survives past a single call.
// ─────────────────────────────────────────────────────────────────────────────

// TestProcessManager_SurvivesAcrossSeparateCalls is the direct regression
// test for the gap this tool was built to close: a process started by one
// Start() call must still be observably running when checked by a LATER,
// separate call (List/Logs), unlike executor.Shell.Run, which kills its
// entire process tree unconditionally the moment the starting call itself
// returns (see shell.go's ROOT-CAUSE NOTE and
// TestShellRun_KillsBackgroundProcess_EvenOnSuccess in shell_test.go for
// the behavior this test is deliberately the mirror image of).
func TestProcessManager_SurvivesAcrossSeparateCalls(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)

	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Simulate time passing between separate tool calls.
	time.Sleep(200 * time.Millisecond)

	list := m.List()
	if len(list) != 1 || !list[0].Running {
		t.Fatalf("process should still be running after Start() returned and time passed; List() = %+v", list)
	}
	m.Stop("p1")
}

// ─────────────────────────────────────────────────────────────────────────────
// Immediate-exit detection
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_StartReportsImmediateExitAsError(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)

	_, err := m.Start("p1", quickExitCommand("boom", 3), "")
	if err == nil {
		t.Fatalf("Start() with a command that exits immediately should return an error")
	}
	if !strings.Contains(err.Error(), "3") {
		t.Errorf("Start() error = %v, want it to mention the exit code (3)", err)
	}
}

func TestProcessManager_StartOfInvalidExecutableFails(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)

	cmd := "definitely-not-a-real-executable-xyz123 --flag"
	if _, err := m.Start("p1", cmd, ""); err == nil {
		t.Errorf("Start() with an invalid command should have failed (either at exec time or via immediate-exit detection)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Logs
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_LogsCapturesOutput(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)

	_, err := m.Start("p1", quickExitCommand("hello-from-process", 0), "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// Give waitForExit a moment to close the log file after the quick exit.
	time.Sleep(200 * time.Millisecond)

	logs, err := m.Logs("p1", 0)
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if !strings.Contains(logs, "hello-from-process") {
		t.Errorf("Logs() = %q, want it to contain %q", logs, "hello-from-process")
	}
}

func TestProcessManager_LogsTailLinesLimitsOutput(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)

	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "1..10 | ForEach-Object { Write-Output \"line-$_\" }"
	} else {
		cmd = "for i in 1 2 3 4 5 6 7 8 9 10; do echo line-$i; done"
	}
	if _, err := m.Start("p1", cmd, ""); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	logs, err := m.Logs("p1", 3)
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	if len(lines) != 3 {
		t.Errorf("Logs(tailLines=3) returned %d lines, want 3 (got: %q)", len(lines), logs)
	}
	if !strings.Contains(logs, "line-10") {
		t.Errorf("Logs(tailLines=3) = %q, want it to contain the LAST line (line-10)", logs)
	}
}

func TestProcessManager_LogsUnknownProcessIDErrors(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)
	if _, err := m.Logs("nonexistent", 0); err == nil {
		t.Errorf("Logs() for an unknown process_id should have failed")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Exit tracking for a process that exits on its own (not via Stop)
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_ListReflectsNaturalExit(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)

	// This command survives Start's own immediate-exit detection window
	// (300ms) but exits shortly after, so List (a separate, later call)
	// observes it having already exited on its own.
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "Start-Sleep -Milliseconds 500; exit 7"
	} else {
		cmd = "sleep 0.5; exit 7"
	}
	info, err := m.Start("p1", cmd, "")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if !info.Running {
		t.Fatalf("Start() reported the process as already exited within its own detection window; test needs a longer delay")
	}

	time.Sleep(1200 * time.Millisecond)

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("List() = %+v, want exactly one entry", list)
	}
	if list[0].Running {
		t.Errorf("List() reports Running=true for a process that should have exited on its own by now")
	}
	if list[0].ExitCode != 7 {
		t.Errorf("List() ExitCode = %d, want 7", list[0].ExitCode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Idle/max-age reaper
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_ReaperKillsProcessPastMaxAge(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, 200*time.Millisecond)

	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	// reapOnce is the same logic the background ticker calls; invoking it
	// directly keeps this test fast and deterministic instead of sleeping
	// past the real 30s ticker interval.
	m.reapOnce()

	list := m.List()
	if len(list) != 0 {
		t.Errorf("process should have been reaped after exceeding max age; List() = %+v", list)
	}
}

func TestProcessManager_ReaperCleansUpExitedEntries(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, 200*time.Millisecond)

	if _, err := m.Start("p1", quickExitCommand("done", 0), ""); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	time.Sleep(350 * time.Millisecond) // let it exit AND exceed maxAge
	m.reapOnce()

	list := m.List()
	if len(list) != 0 {
		t.Errorf("exited entry should have been cleaned up by the reaper past max age; List() = %+v", list)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Shutdown
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_ShutdownKillsTrackedProcesses(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)
	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	m.Shutdown()

	list := m.List()
	if len(list) != 0 {
		t.Errorf("List() after Shutdown() = %+v, want empty", list)
	}
	// Must not panic on a repeat call.
	m.Shutdown()
	m.Shutdown()
}

func TestProcessManager_StartAfterShutdownFails(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)
	m.Shutdown()

	if _, err := m.Start("p1", longRunningCommand(), ""); err == nil {
		t.Errorf("Start() after Shutdown() should have failed")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// working_dir
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_CustomWorkingDir(t *testing.T) {
	m, _ := newTestProcessManager(t, 5, time.Hour)
	customDir := t.TempDir()

	info, err := m.Start("p1", longRunningCommand(), customDir)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if info.WorkingDir != customDir {
		t.Errorf("Start() WorkingDir = %q, want %q", info.WorkingDir, customDir)
	}
	m.Stop("p1")
}
