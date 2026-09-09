//go:build !windows

package executor

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// syscallKillZero checks whether pid exists by sending signal 0 (no-op
// signal, standard existence-check idiom on POSIX systems).
func syscallKillZero(pid int) error {
	return syscall.Kill(pid, syscall.Signal(0))
}

// TestShellRun_Unix_KillsWholeProcessTree exercises proc_other.go's
// Setpgid/process-group-kill path directly (as opposed to proc_windows.go's
// Job Object path, which the Windows-only tests in shell_test.go exercise).
// This file is built only on !windows because proc_other.go itself carries
// that build tag — on Windows there is nothing here to exercise.
func TestShellRun_Unix_KillsWholeProcessTree(t *testing.T) {
	t.Parallel()

	s, dir := newTestShell(t)
	pidFile := filepath.Join(dir, "grandchild.pid")

	// `disown` detaches the grandchild from bash's job control but it still
	// inherits bash's stdout/stderr file descriptors, which is the same
	// inherited-handle mechanism as the Windows Start-Process scenario. The
	// foreground `sleep 25` after it ensures bash itself is still running
	// when the timeout fires below, matching the real production scenario
	// (a still-running shell command that spawned a background process).
	cmd := fmt.Sprintf(`sleep 25 & echo $! > %q; disown; sleep 25`, pidFile)

	const timeoutSecs = 6
	_, res, _, gotResult := runBounded(t, s, cmd, dir, timeoutSecs, 25*time.Second)
	if !gotResult {
		t.Fatalf("did not receive result — the leak is present on the Unix path")
	}
	if res.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 (timeout)", res.ExitCode)
	}

	pidStr, ok := waitForFile(t, pidFile, 3*time.Second)
	if !ok {
		t.Fatalf("grandchild pid file was never written — the grandchild never started")
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("could not parse grandchild pid %q: %v", pidStr, err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscallKillZero(pid); err != nil {
			return // gone
		}
		if time.Now().After(deadline) {
			t.Errorf("grandchild process (pid %d) is still running 10s after Run returned — process group was not fully killed", pid)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestShellRun_Unix_SuccessfulCommand mirrors TestShellRun_SuccessfulCommand
// (Windows/PowerShell version in shell_test.go) using bash, so the shipped
// Unix binary's basic success path has direct coverage, not just the
// tree-kill path above.
func TestShellRun_Unix_SuccessfulCommand(t *testing.T) {
	t.Parallel()

	s, dir := newTestShell(t)
	chunks, res, chunksClosed, gotResult := runBounded(t, s, "echo hello-vectrify", dir, 15, 10*time.Second)

	if !gotResult {
		t.Fatalf("did not receive result")
	}
	if !chunksClosed {
		t.Fatalf("chunks channel was never closed")
	}
	if res.ExitCode != 0 || !res.OK {
		t.Errorf("ExitCode = %d, OK = %v, want (0, true)", res.ExitCode, res.OK)
	}
	out := combinedOutput(chunks)
	if !strings.Contains(out, "hello-vectrify") {
		t.Errorf("expected stdout to contain %q, got: %q", "hello-vectrify", out)
	}
}

// TestShellRun_Unix_NonZeroExit mirrors TestShellRun_NonZeroExit using bash.
func TestShellRun_Unix_NonZeroExit(t *testing.T) {
	t.Parallel()

	s, dir := newTestShell(t)
	_, res, _, gotResult := runBounded(t, s, "exit 3", dir, 15, 10*time.Second)

	if !gotResult {
		t.Fatalf("did not receive result")
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if res.OK {
		t.Errorf("OK = true, want false for a non-zero exit")
	}
}

// TestShellRun_Unix_SimpleTimeout mirrors TestShellRun_SimpleTimeout using
// bash, with no grandchild/process-group complexity involved — just a
// direct child that fails to exit before the context deadline.
func TestShellRun_Unix_SimpleTimeout(t *testing.T) {
	t.Parallel()

	s, dir := newTestShell(t)
	start := time.Now()
	chunks, res, _, gotResult := runBounded(t, s, "sleep 30", dir, 2, 15*time.Second)
	elapsed := time.Since(start)

	if !gotResult {
		t.Fatalf("did not receive result")
	}
	if elapsed > 10*time.Second {
		t.Errorf("Run took %s to return after a 2s timeout — expected prompt return", elapsed)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
	out := combinedOutput(chunks)
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected timeout message in output, got: %q", out)
	}
}
