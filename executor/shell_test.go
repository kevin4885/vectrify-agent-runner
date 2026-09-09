package executor

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newTestShell creates a Shell rooted at a fresh temp directory with a
// discard logger, so tests don't spam stdout with slog output.
func newTestShell(t *testing.T) (*Shell, string) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	return NewShell(dir, log), dir
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// runBounded runs s.Run(cmd, ...) and blocks the calling test goroutine until
// EITHER a ShellResult is received, OR bound elapses — whichever comes
// first. It never blocks forever: that unbounded-wait failure mode is
// precisely the bug this whole test file exists to catch, so the test
// harness itself must not be capable of hanging the same way.
//
// Returns the collected chunks, the result (zero value if bound elapsed),
// whether chunks was observed to close, and whether the result actually
// arrived within bound.
func runBounded(t *testing.T, s *Shell, cmd, workingDir string, timeoutSecs int, bound time.Duration) (chunks []ShellChunk, res ShellResult, chunksClosed bool, gotResult bool) {
	t.Helper()

	chunksCh := make(chan ShellChunk, 256)
	resultCh := make(chan ShellResult, 1)

	go s.Run(cmd, workingDir, timeoutSecs, chunksCh, resultCh)

	var collected []ShellChunk
	chunksDone := make(chan struct{})
	go func() {
		defer close(chunksDone)
		for c := range chunksCh {
			collected = append(collected, c)
		}
	}()

	select {
	case r := <-resultCh:
		res = r
		gotResult = true
	case <-time.After(bound):
		t.Errorf("runBounded: Shell.Run did not send a result within %s — this indicates Run is blocked forever (the exact leak this test guards against)", bound)
		// Do NOT return `collected` here: the goroutine above may still be
		// appending to it concurrently (Run is, by definition, still
		// running on this failure path), so reading it now would be a
		// data race. Returning nil is safe and the test has already
		// failed via t.Errorf above.
		return nil, ShellResult{}, false, false
	}

	// chunksCh is closed by Run right after result is sent (see shell.go's
	// defer ordering), so this should complete almost immediately once we
	// already have the result. Still bound it defensively.
	select {
	case <-chunksDone:
		chunksClosed = true
	case <-time.After(5 * time.Second):
		t.Errorf("runBounded: chunks channel did not close within 5s of receiving result")
		// Same race concern as above: the collector goroutine may still be
		// appending to `collected` concurrently. Don't return it.
		return nil, res, false, gotResult
	}

	return collected, res, chunksClosed, gotResult
}

// combinedOutput concatenates all chunk data for substring assertions.
func combinedOutput(chunks []ShellChunk) string {
	var sb strings.Builder
	for _, c := range chunks {
		sb.WriteString(c.Data)
	}
	return sb.String()
}

// skipIfNotWindows skips tests whose command strings are PowerShell-specific.
// This runner's tests run on Windows per the project's CI/dev environment;
// on other platforms these would need bash-equivalent commands, which is out
// of scope for this guard (kept honest rather than faking pass/fail).
func skipIfNotWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("test uses PowerShell-specific commands; skipping on " + runtime.GOOS)
	}
}

// waitForPidGone polls (bounded) until the process with the given PID is no
// longer running, or the bound elapses. Returns false if the process is
// still running when the bound is reached.
func waitForPidGone(t *testing.T, pid int, bound time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if !pidRunningWindows(pid) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !pidRunningWindows(pid)
}

// pidRunningWindows checks whether a PID is currently an active process,
// via `Get-Process`. Used only by Windows-only tests.
func pidRunningWindows(pid int) bool {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf("Get-Process -Id %d -ErrorAction SilentlyContinue | ForEach-Object { $_.Id }", pid),
	).Output()
	if err != nil {
		// Get-Process erroring is treated as "not found" for our purposes.
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// waitForFile polls (bounded) until path exists and returns its trimmed
// contents, or fails the test if it never appears. Used to avoid a race
// between PowerShell's own startup time (which can itself eat into a short
// timeout under load) and reading the pid file the test command writes.
func waitForFile(t *testing.T, path string, bound time.Duration) (string, bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b)), true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", false
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Regression test for the goroutine/dispatch-slot leak (most important)
// ─────────────────────────────────────────────────────────────────────────────

// TestShellRun_GrandchildOutlivesParent_DoesNotHang is the regression test for the
// production incident: a command starts a detached grandchild (via
// Start-Process -NoNewWindow) that inherits the stdout pipe write handle and
// outlives the immediate child. Before the fix, Run's wg.Wait() blocked
// forever on that pipe, wedging the caller's dispatch goroutine permanently.
//
// This test MUST fail (time out) against the pre-fix code and MUST pass
// after the fix (Run returns promptly because the whole process tree,
// including the grandchild, is killed on timeout).
func TestShellRun_GrandchildOutlivesParent_DoesNotHang(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()

	s, dir := newTestShell(t)
	pidFile := filepath.Join(dir, "grandchild.pid")

	// Spawn a grandchild that sleeps for 25s — far longer than the timeout
	// given to Run below. -NoNewWindow makes it inherit console handles
	// (including the stdout/stderr pipes powershell.exe itself inherited
	// from us), which is exactly the inherited-handle mechanism that causes
	// the leak if the whole tree isn't killed.
	//
	// The main script ALSO sleeps past the timeout (its own Start-Sleep),
	// not just the detached grandchild. This matters: it means the ctx
	// deadline genuinely fires while the *direct* child (powershell.exe
	// running this whole script) is still alive, so this test exercises
	// the real production scenario — a shell command that is still running
	// when its timeout expires, having spawned a background process —
	// rather than a fast-exiting parent (that scenario is covered by
	// TestShellRun_KillsBackgroundProcess_EvenOnSuccess instead).
	cmd := fmt.Sprintf(
		`$p = Start-Process powershell -ArgumentList @('-NoProfile','-Command','Start-Sleep -Seconds 25') -NoNewWindow -PassThru; `+
			`$p.Id | Out-File -FilePath '%s' -Encoding ascii; `+
			`Start-Sleep -Seconds 25`,
		pidFile,
	)

	// timeoutSecs is deliberately generous (not the bare minimum 2s) so
	// that PowerShell interpreter startup — which competes with several
	// other PowerShell-spawning tests running in parallel under
	// t.Parallel() — cannot itself consume the whole budget and prevent
	// Start-Process from ever executing. If that happened this test would
	// pass on a trivial timeout without ever exercising the grandchild /
	// inherited-handle mechanism it exists to test, i.e. it would also pass
	// against the pre-fix buggy code — exactly the false-negative this
	// comment is guarding against.
	const timeoutSecs = 6
	const bound = 25 * time.Second // generous; fix should return in ~timeout+grace, well under this

	chunks, res, chunksClosed, gotResult := runBounded(t, s, cmd, dir, timeoutSecs, bound)

	if !gotResult {
		t.Fatalf("Run never sent a result within %s — the leak is present", bound)
	}
	if !chunksClosed {
		t.Fatalf("chunks channel was never closed — the leak is present")
	}

	// Confirm the grandchild actually started — otherwise this test would
	// pass vacuously (on a plain timeout with no inherited-handle involved)
	// both before and after the fix, proving nothing.
	if _, ok := waitForFile(t, pidFile, 3*time.Second); !ok {
		t.Fatalf("grandchild pid file was never written — the grandchild never started, so this test did not exercise the leak scenario")
	}

	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (timeout)", res.ExitCode)
	}
	if res.OK {
		t.Errorf("OK = true, want false for a timed-out command")
	}
	out := combinedOutput(chunks)
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected timeout message in output, got: %q", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Normal successful command
// ─────────────────────────────────────────────────────────────────────────────

func TestShellRun_SuccessfulCommand(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()

	s, dir := newTestShell(t)
	chunks, res, chunksClosed, gotResult := runBounded(t, s, "Write-Output 'hello-vectrify'", dir, 15, 10*time.Second)

	if !gotResult {
		t.Fatalf("did not receive result")
	}
	if !chunksClosed {
		t.Fatalf("chunks channel was never closed")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !res.OK {
		t.Errorf("OK = false, want true")
	}
	out := combinedOutput(chunks)
	if !strings.Contains(out, "hello-vectrify") {
		t.Errorf("expected stdout to contain %q, got: %q", "hello-vectrify", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Non-zero exit code
// ─────────────────────────────────────────────────────────────────────────────

func TestShellRun_NonZeroExit(t *testing.T) {
	skipIfNotWindows(t)
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
		t.Errorf("OK = true, want false for non-zero exit")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Timeout of a simple long sleep (no grandchild involved)
// ─────────────────────────────────────────────────────────────────────────────

func TestShellRun_SimpleTimeout(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()

	s, dir := newTestShell(t)
	start := time.Now()
	chunks, res, _, gotResult := runBounded(t, s, "Start-Sleep -Seconds 30", dir, 2, 15*time.Second)
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

// ─────────────────────────────────────────────────────────────────────────────
// 5. Process-tree kill: grandchild is actually gone afterwards
// ─────────────────────────────────────────────────────────────────────────────

func TestShellRun_KillsWholeProcessTree(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()

	s, dir := newTestShell(t)
	pidFile := filepath.Join(dir, "grandchild.pid")

	cmd := fmt.Sprintf(
		`$p = Start-Process powershell -ArgumentList @('-NoProfile','-Command','Start-Sleep -Seconds 25') -NoNewWindow -PassThru; `+
			`$p.Id | Out-File -FilePath '%s' -Encoding ascii; `+
			`Start-Sleep -Seconds 25`,
		pidFile,
	)

	// See the comment in TestShellRun_GrandchildOutlivesParent_DoesNotHang
	// for why timeoutSecs is generous rather than the bare minimum: this
	// test's only real assertion is that the grandchild is gone afterwards,
	// which is meaningless if the grandchild never started in the first
	// place.
	const timeoutSecs = 6
	_, res, _, gotResult := runBounded(t, s, cmd, dir, timeoutSecs, 25*time.Second)
	if !gotResult {
		t.Fatalf("did not receive result")
	}
	if res.ExitCode != -1 {
		t.Fatalf("ExitCode = %d, want -1 (timeout)", res.ExitCode)
	}

	pidStr, ok := waitForFile(t, pidFile, 3*time.Second)
	if !ok {
		t.Fatalf("grandchild pid file was never written — the grandchild never started, so this test did not exercise the tree-kill mechanism")
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("could not parse grandchild pid %q: %v", pidStr, err)
	}

	if !waitForPidGone(t, pid, 10*time.Second) {
		t.Errorf("grandchild process (pid %d) is still running 10s after Run returned — process tree was not fully killed", pid)
	}
}

// TestShellRun_KillsBackgroundProcess_EvenOnSuccess documents and verifies
// the product decision (see the comment on `defer tree.kill()` in
// shell.go): a command that exits cleanly but deliberately leaves a
// background process running (e.g. `Start-Process` to launch a dev server)
// does NOT get to keep that process alive after Run returns. This is
// intentional, not an accidental side effect of the leak fix — background
// servers a command starts are not expected to survive the command.
//
// This test specifically targets the in-loop `tree.kill()` on the cmdDone
// branch (the actual mechanism), not just the unconditional `defer
// tree.kill()` that also runs on every return path: it asserts BOTH that
// Run returns quickly (well under postTimeoutGrace, so the grace-timer
// force-break path was never exercised) AND that no "force-killed" chunk
// appears in the output. If the in-loop kill-on-cmdDone were deleted, the
// deferred kill would still eventually reap the grandchild, but this test
// would still catch the regression via the "no force-killed message"
// assertion (the grace path fires its own message the deferred kill alone
// does not need).
func TestShellRun_KillsBackgroundProcess_EvenOnSuccess(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()

	s, dir := newTestShell(t)
	pidFile := filepath.Join(dir, "grandchild.pid")

	// This command exits immediately (no timeout involved) after launching
	// a detached long-running grandchild.
	cmd := fmt.Sprintf(
		`$p = Start-Process powershell -ArgumentList @('-NoProfile','-Command','Start-Sleep -Seconds 25') -NoNewWindow -PassThru; `+
			`$p.Id | Out-File -FilePath '%s' -Encoding ascii; `+
			`exit 0`,
		pidFile,
	)

	start := time.Now()
	chunks, res, _, gotResult := runBounded(t, s, cmd, dir, 15, 10*time.Second)
	elapsed := time.Since(start)

	if !gotResult {
		t.Fatalf("did not receive result")
	}
	if res.ExitCode != 0 || !res.OK {
		t.Fatalf("ExitCode = %d, OK = %v, want a clean success (0, true)", res.ExitCode, res.OK)
	}
	if elapsed > postTimeoutGrace {
		t.Errorf("Run took %s to return after a clean exit — want well under postTimeoutGrace (%s); this suggests the in-loop kill-on-cmdDone didn't fire and Run fell through to the grace-timer force-break path instead", elapsed, postTimeoutGrace)
	}
	out := combinedOutput(chunks)
	if strings.Contains(out, "force-killed") {
		t.Errorf("output contains the force-killed message, meaning Run took the grace-timeout path instead of killing the tree promptly on cmdDone: %q", out)
	}

	pidStr, ok := waitForFile(t, pidFile, 3*time.Second)
	if !ok {
		t.Fatalf("grandchild pid file was never written — the grandchild never started")
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		t.Fatalf("could not parse grandchild pid %q: %v", pidStr, err)
	}

	if !waitForPidGone(t, pid, 10*time.Second) {
		t.Errorf("grandchild process (pid %d) is still running 10s after a *successful* Run returned — background processes must be killed even on success per product spec", pid)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 8. Escaped-descendant / grace-timeout last-resort path
// ─────────────────────────────────────────────────────────────────────────────

// noKillProcTree wraps a real procTreeIface but makes kill() a deliberate
// no-op, simulating a descendant that has escaped the Job Object / process
// group (the documented race window in proc_windows.go, or a setsid'd
// daemon on Unix per proc_other.go). This is the only realistic way to
// deterministically exercise Run's last-resort graceC / forceKilled path
// without depending on actually racing or defeating the OS-level tree-kill
// mechanism, which would be inherently flaky.
type noKillProcTree struct {
	inner procTreeIface
}

func (n *noKillProcTree) configure(c *exec.Cmd)    { n.inner.configure(c) }
func (n *noKillProcTree) attach(c *exec.Cmd) error { return n.inner.attach(c) }
func (n *noKillProcTree) kill()                    {} // deliberately does nothing
func (n *noKillProcTree) release()                 { n.inner.release() }

// TestShellRun_EscapedDescendant_ForcedAfterGrace verifies Run's true
// last-resort safety net: even if tree.kill() completely fails to unblock
// the pipe readers (simulated here via noKillProcTree), Run still returns
// within postTimeoutGrace of the timeout firing, reports failure, and warns
// about the forced kill in the output — it does not hang forever.
func TestShellRun_EscapedDescendant_ForcedAfterGrace(t *testing.T) {
	skipIfNotWindows(t)

	// Not run with t.Parallel(): mutates the package-level newProcTreeFn,
	// which every other test in this file/package also reads.
	orig := newProcTreeFn
	newProcTreeFn = func() (procTreeIface, error) {
		real, err := newProcTree()
		if err != nil {
			return nil, err
		}
		return &noKillProcTree{inner: real}, nil
	}
	defer func() { newProcTreeFn = orig }()

	s, dir := newTestShell(t)
	pidFile := filepath.Join(dir, "grandchild.pid")

	// Spawn a grandchild that inherits the pipe write handle, exactly like
	// the other grandchild tests — but this time tree.kill() (which would
	// normally kill it) is a no-op. Note that ctx's default Cancel still
	// kills the DIRECT child (powershell.exe) independently of tree.kill(),
	// so the direct child dies right on schedule; what must NOT happen is
	// the pipe reaching EOF because of that alone — it can't, since the
	// grandchild (with kill() neutered) still holds the write handle open.
	// The only way Run can still return is via the graceC branch.
	cmd := fmt.Sprintf(
		`$p = Start-Process powershell -ArgumentList @('-NoProfile','-Command','Start-Sleep -Seconds 30') -NoNewWindow -PassThru; `+
			`$p.Id | Out-File -FilePath '%s' -Encoding ascii; `+
			`Start-Sleep -Seconds 30`,
		pidFile,
	)

	// timeoutSecs uses the same generous value as the other grandchild
	// tests (see the comment in TestShellRun_GrandchildOutlivesParent_DoesNotHang)
	// so PowerShell cold-start under parallel test load can't consume the
	// whole budget before Start-Process ever runs.
	const timeoutSecs = 6
	start := time.Now()
	chunks, res, chunksClosed, gotResult := runBounded(t, s, cmd, dir, timeoutSecs, 25*time.Second)
	elapsed := time.Since(start)

	if !gotResult {
		t.Fatalf("Run never sent a result — the last-resort forced-path safety net failed to bound the wait")
	}
	if !chunksClosed {
		t.Fatalf("chunks channel was never closed")
	}
	pidStr, ok := waitForFile(t, pidFile, 3*time.Second)
	if !ok {
		t.Fatalf("grandchild pid file was never written — the grandchild never started, so this test did not exercise the escaped-descendant scenario")
	}
	// Since tree.kill() is neutered for this test, the grandchild's normal
	// cleanup path (tree.release() closing the Job Object, which would
	// still kill it via JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE) is delegated
	// through noKillProcTree.release() to the real release() — so in
	// practice the grandchild IS still cleaned up when Run returns. This
	// t.Cleanup is defensive belt-and-braces in case that ever changes,
	// not a workaround for an actual orphan.
	if pid, err := strconv.Atoi(pidStr); err == nil {
		t.Cleanup(func() {
			_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command",
				fmt.Sprintf("Stop-Process -Id %d -Force -ErrorAction SilentlyContinue", pid)).Run()
		})
	}
	// Should return at roughly timeoutSecs + postTimeoutGrace, not before
	// (the tree.kill() no-op means the fast EOF path can't fire) and not
	// much after (the grace timer must fire promptly).
	wantMin := time.Duration(timeoutSecs) * time.Second
	wantMax := wantMin + postTimeoutGrace + 5*time.Second // slack for scheduling
	if elapsed < wantMin || elapsed > wantMax {
		t.Errorf("elapsed = %s, want roughly between %s and %s (timeoutSecs + postTimeoutGrace, with slack)", elapsed, wantMin, wantMax)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 (timeout)", res.ExitCode)
	}
	if res.OK {
		t.Errorf("OK = true, want false")
	}
	out := combinedOutput(chunks)
	if !strings.Contains(out, "timed out") {
		t.Errorf("expected timeout message in output, got: %q", out)
	}
	if !strings.Contains(out, "force-killed") {
		t.Errorf("expected force-killed diagnostic message in output (kill() was a no-op, so the grace path must have been taken), got: %q", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Invalid working directory / start failure still returns a result
// ─────────────────────────────────────────────────────────────────────────────

func TestShellRun_InvalidWorkingDir_ReturnsResult(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()

	s, dir := newTestShell(t)
	badDir := filepath.Join(dir, "does-not-exist", "nested", "nope")

	_, res, chunksClosed, gotResult := runBounded(t, s, "Write-Output 'unreachable'", badDir, 15, 10*time.Second)

	if !gotResult {
		t.Fatalf("Run did not return a result for an invalid working directory — it must never hang on a start failure")
	}
	if !chunksClosed {
		t.Fatalf("chunks channel was never closed")
	}
	if res.OK {
		t.Errorf("OK = true, want false for a start failure")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 7. Large output on a fast-exiting command is not silently truncated
// ─────────────────────────────────────────────────────────────────────────────

// TestShellRun_LargeOutput_NotTruncated guards against the specific data-loss
// risk that motivated owning our own stdout/stderr pipes instead of using
// c.StdoutPipe()/c.StderrPipe(): if Cmd.Wait() were allowed to close our
// pipe read-ends the instant the direct child exits (which is what
// StdoutPipe/StderrPipe's bookkeeping does), a command that writes a large
// burst of output and exits immediately could have its trailing output
// silently dropped before streamPipe finishes draining the kernel pipe
// buffer. This test writes enough output to exceed a typical OS pipe buffer
// (64KB on Windows/Linux) in one burst, then asserts every byte arrived.
func TestShellRun_LargeOutput_NotTruncated(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()

	const lineLen = 100
	const lineCount = 2000 // ~200KB, comfortably larger than any OS pipe buffer
	line := strings.Repeat("x", lineLen)

	// Emit lineCount lines of `line`, then exit immediately — no artificial
	// delay, so Wait() sees the exit as soon as PowerShell's write loop
	// finishes, maximizing the chance of losing unread buffered output if
	// pipe ownership were wrong.
	cmd := fmt.Sprintf(`1..%d | ForEach-Object { '%s' }`, lineCount, line)

	s, dir := newTestShell(t)
	chunks, res, chunksClosed, gotResult := runBounded(t, s, cmd, dir, 15, 15*time.Second)

	if !gotResult {
		t.Fatalf("did not receive result")
	}
	if !chunksClosed {
		t.Fatalf("chunks channel was never closed")
	}
	if res.ExitCode != 0 || !res.OK {
		t.Fatalf("ExitCode = %d, OK = %v, want a clean success (0, true)", res.ExitCode, res.OK)
	}

	out := combinedOutput(chunks)
	gotLines := strings.Count(out, line)
	if gotLines != lineCount {
		t.Errorf("got %d occurrences of the repeated line, want %d — output was truncated (total captured bytes: %d)", gotLines, lineCount, len(out))
	}
}
