// Package executor: process.go implements long-lived, detached background
// processes ("process" command type) — start, stop, list, logs — that
// survive PAST the single call that started them, unlike a "shell" command
// (see executor/shell.go's ROOT-CAUSE NOTE), which always kills its entire
// process tree unconditionally when Run returns, even on a clean exit. That
// is the correct, deliberate behavior for shell.go's own use case (a
// request/response command must always terminate), but it means shell.go
// structurally cannot support "start a dev server, then drive it with
// browser commands issued as separate, later calls" — the server would be
// killed the instant the starting shell call returned.
//
// This file exists specifically for that workflow. A process started here
// is tracked in a small pool keyed by a caller-supplied process_id (the
// same shape as BrowserManager's session_id) and keeps running until
// explicitly stopped, reaped for exceeding max age, or the runner itself
// shuts down.
//
// Gated behind config.AllowShell (see runner/runner.go handleProcess) — the
// same reasoning as browser.go: a runner already trusted with a real shell
// is already trusted with equivalent (or greater) local-machine capability,
// so a separate allow_process setting would add config surface without a
// real security boundary.
//
// Process-tree killing reuses the exact same procTreeIface / newProcTreeFn
// mechanism shell.go uses (see proc_windows.go / proc_other.go) — the
// difference is entirely in WHEN kill() is called: shell.go defers it
// immediately after Run starts, so it always fires the instant the command
// returns; this file only calls it from Stop() (explicit) or the reaper
// (max-age safety net) or Shutdown() (runner exiting) — i.e. never
// automatically just because the *starting* call returned.
package executor

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// managedProcess tracks one detached process this runner started.
type managedProcess struct {
	id         string
	command    string
	workingDir string
	pid        int
	startedAt  time.Time

	cmd     *exec.Cmd
	tree    procTreeIface
	logFile *os.File
	logPath string

	mu       sync.Mutex
	exited   bool
	exitedAt time.Time
	exitCode int
	exitErr  string // human-readable, e.g. "signal: killed" — empty if exitCode == 0
}

// ProcessInfo is the caller-facing snapshot of one managed process, returned
// by Start (the just-created process) and List (every tracked process).
type ProcessInfo struct {
	ID         string
	PID        int
	Command    string
	WorkingDir string
	StartedAt  time.Time
	Running    bool
	ExitCode   int    // valid only when !Running
	ExitError  string // valid only when !Running; empty on a clean (code 0) exit
}

func (p *managedProcess) snapshot() ProcessInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProcessInfo{
		ID:         p.id,
		PID:        p.pid,
		Command:    p.command,
		WorkingDir: p.workingDir,
		StartedAt:  p.startedAt,
		Running:    !p.exited,
		ExitCode:   p.exitCode,
		ExitError:  p.exitErr,
	}
}

// lastActivity returns the time this process last became relevant for
// reaping purposes: its start time while running, or its exit time once it
// has exited. Mirrors browserSession.lastUsedAt's role in BrowserManager's
// reaper, adapted for a resource with no per-call "activity" of its own —
// see reapOnce's doc comment for why this specific definition was chosen.
func (p *managedProcess) lastActivity() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return p.exitedAt
	}
	return p.startedAt
}

// ProcessManager owns a small pool of long-lived, detached processes keyed
// by a caller-supplied process_id. One ProcessManager is created per Runner
// (see runner.New) and lives for the process lifetime.
type ProcessManager struct {
	workspaceRoot string
	log           *slog.Logger
	maxProcesses  int
	maxAge        time.Duration

	mu           sync.Mutex
	processes    map[string]*managedProcess
	reaperOnce   sync.Once
	stopReaper   chan struct{}
	shutdownOnce sync.Once
	shuttingDown bool
}

// NewProcessManager creates a ProcessManager scoped to workspaceRoot (used
// only as the default working directory for a started process, same
// convention as executor.Shell — process start does not enforce path
// containment on working_dir, matching shell.go's own behavior: both are
// gated by allow_shell, which already implies full local-machine trust).
func NewProcessManager(workspaceRoot string, maxProcesses int, maxAge time.Duration, log *slog.Logger) *ProcessManager {
	if maxProcesses <= 0 {
		maxProcesses = 5
	}
	m := &ProcessManager{
		workspaceRoot: workspaceRoot,
		log:           log,
		maxProcesses:  maxProcesses,
		maxAge:        maxAge,
		processes:     make(map[string]*managedProcess),
		stopReaper:    make(chan struct{}),
	}
	m.reaperOnce.Do(func() { go m.reapLoop() })
	return m
}

// processLogDir returns the directory detached-process log files are
// written to. Deliberately NOT under workspaceRoot: these files are
// runner-internal state (retrieved only via the "logs" action, which reads
// and returns their content as text — never exposed as a path the caller
// operates on directly), not user data, so the file_op-style path
// containment rule that applies to screenshots (a path the CALLER chooses)
// does not apply here at all.
func processLogDir() string {
	return filepath.Join(os.TempDir(), "vectrify-runner-process-logs")
}

// Start launches command (interpreted the same way runner_shell interprets
// its command string — bash -c on Unix, powershell -Command on Windows) in
// workingDir (defaults to workspaceRoot), detached from this call's own
// lifetime: the process keeps running after Start returns, until Stop is
// called, the max-age reaper reaps it, or the runner shuts down.
//
// Returns an error if id is already in use (call Stop first, or pick a new
// id) or the pool is full. A command that fails to launch at all (bad
// executable, permission denied) is reported as an error here; a command
// that launches but exits quickly on its own (e.g. a typo'd sub-command) is
// NOT treated as a Start error — the caller discovers that via List/Logs,
// same as they would by checking a real server process's own logs.
func (m *ProcessManager) Start(id, command, workingDir string) (ProcessInfo, error) {
	if id == "" {
		return ProcessInfo{}, fmt.Errorf("process_id is required")
	}
	if command == "" {
		return ProcessInfo{}, fmt.Errorf("command is required")
	}
	if workingDir == "" {
		workingDir = m.workspaceRoot
	}

	m.mu.Lock()
	if m.shuttingDown {
		m.mu.Unlock()
		return ProcessInfo{}, fmt.Errorf("runner is shutting down")
	}
	if _, exists := m.processes[id]; exists {
		m.mu.Unlock()
		return ProcessInfo{}, fmt.Errorf("process_id %q is already in use; stop it first or choose a different process_id", id)
	}
	if len(m.processes) >= m.maxProcesses {
		m.mu.Unlock()
		return ProcessInfo{}, fmt.Errorf(
			"background process limit reached (max_background_processes=%d); stop an existing process_id before starting another",
			m.maxProcesses,
		)
	}
	m.mu.Unlock()

	if err := os.MkdirAll(processLogDir(), 0755); err != nil {
		return ProcessInfo{}, fmt.Errorf("creating process log directory: %w", err)
	}
	logPath := filepath.Join(processLogDir(), fmt.Sprintf("%s-%d.log", sanitizeForFilename(id), time.Now().UnixNano()))
	logFile, err := os.Create(logPath)
	if err != nil {
		return ProcessInfo{}, fmt.Errorf("creating log file: %w", err)
	}

	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		const utf8Preamble = "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; " +
			"$OutputEncoding = [System.Text.Encoding]::UTF8; "
		c = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", utf8Preamble+command)
	} else {
		c = exec.Command("bash", "-c", command)
	}
	c.Dir = workingDir
	c.Stdout = logFile
	c.Stderr = logFile

	// Reuses shell.go's process-tree tracker (Windows Job Object / Unix
	// process group) — but unlike shell.go, kill() is never deferred here.
	// It is only ever called from Stop, the reaper, or Shutdown. See this
	// file's package doc comment for why that difference is the entire
	// point.
	tree, err := newProcTreeFn()
	if err != nil {
		m.log.Warn("process: failed to create process tree tracker; Stop will only kill the direct child, not its descendants", "process_id", id, "err", err)
	}
	if tree != nil {
		tree.configure(c)
	}

	if err := c.Start(); err != nil {
		logFile.Close()
		os.Remove(logPath)
		return ProcessInfo{}, fmt.Errorf("starting process: %w", err)
	}
	if tree != nil {
		if err := tree.attach(c); err != nil {
			m.log.Warn("process: failed to attach to process tree tracker; Stop will only kill the direct child, not its descendants", "process_id", id, "err", err)
		}
	}

	mp := &managedProcess{
		id:         id,
		command:    command,
		workingDir: workingDir,
		pid:        c.Process.Pid,
		startedAt:  time.Now(),
		cmd:        c,
		tree:       tree,
		logFile:    logFile,
		logPath:    logPath,
	}

	m.mu.Lock()
	// Re-check under the lock: another Start(id) could have raced us
	// between the earlier check and here. Extremely unlikely in practice
	// (the LLM issues one tool call at a time in the common case), but
	// cheap to guard properly rather than assume.
	if _, exists := m.processes[id]; exists {
		m.mu.Unlock()
		if tree != nil {
			tree.kill()
			tree.release()
		}
		logFile.Close()
		os.Remove(logPath)
		return ProcessInfo{}, fmt.Errorf("process_id %q is already in use; stop it first or choose a different process_id", id)
	}
	m.processes[id] = mp
	m.mu.Unlock()

	go m.waitForExit(mp)

	// Brief grace period to catch a command that fails immediately (bad
	// executable, permission denied at exec time on Unix, a typo'd
	// sub-command under bash -c/-Command) so Start reports that as an
	// error instead of "started successfully" for a process that was
	// already dead by the time the caller's next tool call checks on it.
	// Anything that survives this long is reported as started — a command
	// that runs for a while and fails later is not a Start-time error, the
	// same way a real dev server crashing five minutes in isn't either.
	time.Sleep(300 * time.Millisecond)
	info := mp.snapshot()
	if !info.Running && info.ExitCode != 0 {
		// Only a non-zero immediate exit is treated as a Start-time error
		// (bad executable, syntax error, permission denied). A command
		// that legitimately finishes fast and successfully (exit code 0)
		// within this window is not a failure — e.g. a quick one-off
		// setup command someone chose to run through this tool rather
		// than runner_shell. Its output is still available via Logs, and
		// List will show it as not Running, exactly like any other
		// process that later exits cleanly on its own.
		detail := info.ExitError
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", info.ExitCode)
		}
		return info, fmt.Errorf("process exited immediately after starting (%s) — check the command; see logs via the logs action for process_id %q before it is cleaned up", detail, id)
	}
	return info, nil
}

// waitForExit blocks until mp's process exits, then records its outcome and
// closes the log file. Runs for the lifetime of every started process,
// including ones that are later Stopped (Stop's kill() causes this Wait to
// return, same as any other exit) — this is the ONLY place cmd.Wait() is
// called, so Stop must never call it itself (double-Wait on the same
// *exec.Cmd is undefined behavior).
func (m *ProcessManager) waitForExit(mp *managedProcess) {
	err := mp.cmd.Wait()

	mp.mu.Lock()
	mp.exited = true
	mp.exitedAt = time.Now()
	if err != nil {
		mp.exitErr = err.Error()
		if exitErr, ok := err.(*exec.ExitError); ok {
			mp.exitCode = exitErr.ExitCode()
		} else {
			mp.exitCode = 1
		}
	}
	mp.mu.Unlock()

	mp.logFile.Close()
}

// Stop kills the process (its whole tree, if the tracker attached
// successfully) and removes it from the pool. Returns false if there was
// no such process_id — not an error; stopping an already-stopped or
// nonexistent process_id is a no-op the caller can treat as success, same
// convention as BrowserManager.Close.
//
// Does not call cmd.Wait() itself — waitForExit (already running in its own
// goroutine since Start) observes the exit this kill() causes and does the
// bookkeeping; Stop only needs to trigger it and remove the pool entry.
func (m *ProcessManager) Stop(id string) bool {
	m.mu.Lock()
	mp, ok := m.processes[id]
	if ok {
		delete(m.processes, id)
	}
	m.mu.Unlock()

	if !ok {
		return false
	}
	if mp.tree != nil {
		mp.tree.kill()
		mp.tree.release()
	} else {
		// No tree tracker (newProcTreeFn failed at Start time) — fall back
		// to killing just the direct child. Descendants, if any, are not
		// tracked and may survive; this mirrors the same documented,
		// narrow limitation shell.go accepts for the analogous failure
		// path (see shell.go's "tree tracker" warning log).
		_ = mp.cmd.Process.Kill()
	}
	return true
}

// List returns a snapshot of every currently tracked process (running or
// recently exited — see reapOnce for when an exited entry is finally
// dropped).
func (m *ProcessManager) List() []ProcessInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ProcessInfo, 0, len(m.processes))
	for _, mp := range m.processes {
		out = append(out, mp.snapshot())
	}
	return out
}

// maxLogBytes bounds how much of a process's combined stdout/stderr Logs
// returns in one call — mirrors FileOps.readFileLines's own clipping
// constant/rationale: a runaway dev server logging in a tight loop must
// never turn one "logs" call into an unbounded response.
const maxLogBytes = 200_000

// Logs returns the tail of the process's combined stdout/stderr log
// (interleaved in the order it was written, since both streams share one
// file — same convention shell.go's callers already expect from combined
// output). tailLines <= 0 means "return everything" (subject to the
// maxLogBytes cap, which is applied from the end so the most recent output
// is what survives truncation).
func (m *ProcessManager) Logs(id string, tailLines int) (string, error) {
	m.mu.Lock()
	mp, ok := m.processes[id]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no such process_id %q (it may have already been stopped and cleaned up)", id)
	}

	data, err := os.ReadFile(mp.logPath)
	if err != nil {
		return "", fmt.Errorf("reading process log: %w", err)
	}
	text := string(data)

	if tailLines > 0 {
		// Strip exactly one trailing newline first: Split on "\n" for a
		// file ending in \n (the overwhelmingly common case for command
		// output) otherwise produces an artificial trailing "" element
		// that would count as one of the tailLines, silently shorting the
		// real content by one line — same fix as FileOps.readFileLines
		// applies for the same reason.
		trimmed := strings.TrimSuffix(text, "\n")
		lines := strings.Split(trimmed, "\n")
		if len(lines) > tailLines {
			lines = lines[len(lines)-tailLines:]
		}
		text = strings.Join(lines, "\n")
	}

	if len(text) > maxLogBytes {
		text = "<earlier output clipped>\n" + text[len(text)-maxLogBytes:]
	}
	return text, nil
}

// reapLoop periodically reaps processes past m.maxAge — a safety net for a
// caller that forgot to Stop a process it started, mirroring
// BrowserManager.reapIdleLoop's role for browser sessions.
func (m *ProcessManager) reapLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopReaper:
			return
		case <-ticker.C:
			m.reapOnce()
		}
	}
}

// reapOnce force-stops any process whose lastActivity (start time while
// running, exit time once exited — see managedProcess.lastActivity) is
// older than m.maxAge.
//
// This single timeout does double duty for two different situations,
// deliberately: (1) a genuinely forgotten long-running process the caller
// never stopped — reaped so it does not run forever and permanently hold a
// pool slot; (2) a process that already exited on its own but whose entry
// nobody ever retrieved logs for or explicitly Stopped — reaped so exited
// entries do not accumulate in the pool forever either. Using the same
// m.maxAge for both keeps this simple (one knob, not two) at the cost of a
// caller having up to m.maxAge after a natural exit to still call Logs
// before the entry disappears — acceptable, since m.maxAge defaults to a
// long duration specifically to leave room for that.
func (m *ProcessManager) reapOnce() {
	m.mu.Lock()
	now := time.Now()
	var toReap []*managedProcess
	for id, mp := range m.processes {
		if now.Sub(mp.lastActivity()) <= m.maxAge {
			continue
		}
		delete(m.processes, id)
		toReap = append(toReap, mp)
	}
	m.mu.Unlock()

	for _, mp := range toReap {
		info := mp.snapshot()
		if info.Running {
			m.log.Info("process: reaping process that exceeded max age", "process_id", mp.id, "pid", mp.pid,
				"age", now.Sub(mp.startedAt).Round(time.Second).String())
			if mp.tree != nil {
				mp.tree.kill()
				mp.tree.release()
			} else {
				_ = mp.cmd.Process.Kill()
			}
		} else {
			m.log.Info("process: cleaning up exited process past max age", "process_id", mp.id, "pid", mp.pid)
		}
		_ = os.Remove(mp.logPath) // best-effort; a leaked temp log file is not worth failing over
	}
}

// Shutdown kills every tracked process. Called once during runner shutdown
// so no detached process is left running as a true orphan after the runner
// process itself exits — see this file's package doc comment: the design
// decision is that a background process's lifetime is bounded by the
// runner's own lifetime, not left to survive a runner restart untracked.
// Idempotent — safe to call more than once (mirrors BrowserManager.Shutdown).
func (m *ProcessManager) Shutdown() {
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		m.shuttingDown = true
		close(m.stopReaper)
		processes := m.processes
		m.processes = make(map[string]*managedProcess)
		m.mu.Unlock()

		for id, mp := range processes {
			if mp.tree != nil {
				mp.tree.kill()
				mp.tree.release()
			} else {
				_ = mp.cmd.Process.Kill()
			}
			m.log.Info("process: killed on runner shutdown", "process_id", id, "pid", mp.pid)
		}
	})
}

// sanitizeForFilename replaces characters that are unsafe in a filename on
// some platform (Windows in particular) with "_", so an arbitrary
// caller-supplied process_id can always be embedded in a log file path
// without failing os.Create.
func sanitizeForFilename(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "process"
	}
	return sb.String()
}
