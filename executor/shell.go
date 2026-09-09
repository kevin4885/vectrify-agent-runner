package executor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// ShellChunk is a piece of output from a running shell command.
type ShellChunk struct {
	Stream string // "stdout" or "stderr"
	Data   string
}

// ShellResult is the final result of a shell command.
type ShellResult struct {
	ExitCode int
	OK       bool
}

// Shell runs shell commands on the local machine.
type Shell struct {
	workspaceRoot string
	log           *slog.Logger
}

// NewShell creates a Shell scoped to workspaceRoot as the default working dir.
func NewShell(workspaceRoot string, log *slog.Logger) *Shell {
	return &Shell{workspaceRoot: workspaceRoot, log: log}
}

// postTimeoutGrace bounds how long Run keeps waiting for the pipe-reader
// goroutines to finish after the process tree has been killed (in response
// to a context deadline, or to the direct child process exiting on its
// own). tree.kill() should force every process holding the pipes' write
// ends open to release them almost immediately, so this is pure insurance
// against a descendant that somehow escaped that mechanism (see the
// documented race window in proc_windows.go) — in that rare case Run asks
// the OS to close its own read ends of the pipes (see closeReadEndAsync)
// rather than waiting forever. This must never be unbounded: an unbounded
// wait here is exactly the root cause of the leak this file fixes.
const postTimeoutGrace = 5 * time.Second

// cmdDoneGrace bounds how long Run waits, after it has already stopped
// draining output, for the concurrently-running c.Wait() goroutine to
// report the direct child's exit status. In virtually every case this is
// near-instant: once tree.kill() or a natural exit has caused our pipe
// reads to unblock, the direct child is either already exited (and Wait
// has already reaped it) or exits within microseconds of being killed. If
// this bound is ever hit, Run reports the run as failed rather than
// guessing at success — see the `!exited` handling near the end of Run.
const cmdDoneGrace = 3 * time.Second

// procTreeIface is the behavior Run() depends on from a process-tree
// tracker (see proc_windows.go / proc_other.go for the real
// platform-specific implementations). Defined as an interface, and obtained
// through the overridable newProcTreeFn below, purely so tests can inject a
// tracker whose kill() is a deliberate no-op — the only way to
// deterministically exercise Run's last-resort escaped-descendant /
// grace-timeout path (see the graceC case below) without depending on
// genuinely defeating the OS-level Job Object / process-group mechanism,
// which would be flaky and platform-fragile to set up from a test.
type procTreeIface interface {
	configure(c *exec.Cmd)
	attach(c *exec.Cmd) error
	kill()
	release()
}

// newProcTreeFn constructs the process-tree tracker Run() uses. It is a
// package-level variable (rather than a direct call to newProcTree) solely
// so tests in this package can temporarily swap it out; production code
// must never reassign it.
var newProcTreeFn = func() (procTreeIface, error) { return newProcTree() }

// Run executes cmd in workingDir (defaults to workspaceRoot), streaming output
// chunks to the chunks channel.  Sends ShellResult to result channel when done.
//
// The chunks and result channels are closed before Run returns.
//
// ROOT-CAUSE NOTE (read before "simplifying" this function):
// exec.CommandContext only ever kills the ONE direct child process it
// started (powershell.exe / bash). If that child spawns a grandchild that
// outlives it — a background dev server, `Start-Process ... -NoNewWindow`,
// `nohup foo &`, a detached test watcher — the grandchild inherits the
// child's stdout/stderr pipe WRITE handles. The pipe therefore never
// reaches EOF, so a plain `io.Read` loop on it never returns, so a plain
// `sync.WaitGroup.Wait()` on that loop never returns, so this function
// would never send a result or close its channels. That, in turn, blocks a
// dispatch goroutine in client/ws_client.go forever, permanently leaking one
// of the runner's limited concurrency slots. This actually happened in
// production. Every mechanism below (procTree, the bounded select loop,
// WaitDelay) exists solely to make that structurally impossible: Run must
// ALWAYS reach the point where it sends to result and closes chunks, no
// matter what the command or its descendants do.
//
// INVARIANT this relies on: the caller must keep draining `chunks` (e.g.
// runner.go's `for chunk := range chunks`) until it closes. `chunks` is
// buffered by the caller (64 in runner.go, 256 in this package's tests),
// but the sends in this function still block once that buffer is full —
// if a future caller stops draining chunks early, those blocking sends
// (not the process-tree logic fixed here) would become the new unbounded
// wait. Don't reintroduce that by making a caller conditionally stop
// reading `chunks`.
func (s *Shell) Run(
	cmd string,
	workingDir string,
	timeoutSecs int,
	chunks chan<- ShellChunk,
	result chan<- ShellResult,
) {
	defer close(chunks)
	defer close(result)

	// Recover must be declared AFTER the channel-close defers so it runs first
	// (LIFO). At recovery time both channels are still open, so we can safely
	// send an error result before the deferred closes fire.
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("shell.Run panic recovered",
				"panic", p,
				"stack", string(debug.Stack()),
			)
			select {
			case result <- ShellResult{ExitCode: 1, OK: false}:
			default: // result already has a value — nothing to do
			}
		}
	}()

	if workingDir == "" {
		workingDir = s.workspaceRoot
	}

	timeout := time.Duration(timeoutSecs) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		// Force UTF-8 I/O so file content with Unicode characters (em-dashes,
		// ellipses, etc.) is not mangled by PowerShell's default system code page.
		const utf8Preamble = "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; " +
			"$OutputEncoding = [System.Text.Encoding]::UTF8; "
		c = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", utf8Preamble+cmd)
	} else {
		c = exec.CommandContext(ctx, "bash", "-c", cmd)
	}
	c.Dir = workingDir

	// Defence-in-depth backstop for the process itself: if ctx is done and
	// the direct child fails to exit promptly after Cancel (default:
	// Process.Kill()), the stdlib force-kills it again after this delay
	// rather than leaving c.Wait() to block indefinitely on Process.Wait.
	// This does NOT touch pipes — we deliberately own our own pipes below
	// instead of using StdoutPipe/StderrPipe, specifically so that c.Wait()
	// can never close them out from under an in-flight Read (see the
	// pipe-ownership comment below for why that matters).
	c.WaitDelay = postTimeoutGrace

	// tree tracks the whole process tree the command creates (Windows Job
	// Object / Unix process group) so it can be killed as a single unit —
	// see proc_windows.go / proc_other.go for the full rationale. Must be
	// configured before Start() and torn down on every exit path, including
	// panics, which is why kill/release are registered as defers immediately
	// after creation rather than called inline at the bottom of the function.
	tree, err := newProcTreeFn()
	if err != nil {
		s.log.Warn("shell: failed to create process tree tracker; tree-kill on timeout will be best-effort only", "err", err)
	}
	// LIFO: tree.kill() runs first (on every return, including panics,
	// success, and non-zero exit), then tree.release().
	//
	// PRODUCT DECISION (not just a leak-fix side effect): the whole process
	// tree is killed unconditionally when Run returns, even after a clean
	// exit. A command that deliberately leaves a background process running
	// (e.g. `Start-Process ... -NoNewWindow` to launch a dev server, `node
	// server.js &`) is NOT expected to have that process survive the
	// command — this is intentional and predictable per product spec, not
	// an accidental consequence of the tree-kill mechanism. See
	// TestShellRun_KillsBackgroundProcess_EvenOnSuccess.
	defer tree.release()
	defer tree.kill()

	tree.configure(c)

	// We deliberately do NOT use c.StdoutPipe()/c.StderrPipe(). Those hand
	// their read end to exec.Cmd's own bookkeeping (c.parentIOPipes), and
	// Cmd.Wait() unconditionally closes everything in that list once the
	// direct child process exits — even if our own streamPipe goroutines
	// are still mid-Read and even if the kernel pipe buffer still has
	// unread bytes in it. That's fine for the common "command exits, no
	// descendants" case, but it means output written by the process in its
	// final moments (a burst right before exit) could be silently dropped
	// the instant Wait() sees the exit, before we've drained it — actual
	// user-visible data loss, not just a hang.
	//
	// Creating and owning the pipes ourselves (os.Pipe()) sidesteps that
	// entirely: c.Stdout/c.Stderr are set to the *os.File write ends
	// directly, which exec.Cmd hands straight to the child process without
	// ever registering the read ends in c.parentIOPipes — so c.Wait() has
	// nothing of ours to close, at any time, for any reason. The read ends
	// (stdoutPipe/stderrPipe below) are closed only by us, only after
	// we've decided to stop waiting for them (see closeReadEndAsync below
	// for how, and why that close is itself never allowed to block Run).
	stdoutPipe, stdoutW, err := os.Pipe()
	if err != nil {
		chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("Error creating stdout pipe: %v\n", err)}
		result <- ShellResult{ExitCode: 1, OK: false}
		return
	}
	stderrPipe, stderrW, err := os.Pipe()
	if err != nil {
		stdoutW.Close()
		stdoutPipe.Close()
		chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("Error creating stderr pipe: %v\n", err)}
		result <- ShellResult{ExitCode: 1, OK: false}
		return
	}
	c.Stdout = stdoutW
	c.Stderr = stderrW

	if err := c.Start(); err != nil {
		stdoutW.Close()
		stderrW.Close()
		stdoutPipe.Close()
		stderrPipe.Close()
		chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("Error starting command: %v\n", err)}
		result <- ShellResult{ExitCode: 1, OK: false}
		return
	}
	// The child (and, on Unix, any descendant that forks before exec)
	// inherited its own copies of the write ends at Start(); our copies
	// must be closed now, or the pipe would never reach EOF even after
	// every process holding a write end exits — we'd be the one still
	// holding it open, recreating the exact bug this file fixes. This is
	// always safe to do synchronously: we are not doing any I/O of our own
	// on these write-end handles, so there is never a Read/Write of ours
	// in flight for Close() to block on.
	stdoutW.Close()
	stderrW.Close()
	// Our read ends must eventually be closed too, but — unlike the write
	// ends above — NEVER synchronously from Run's own goroutine. Reasoning
	// (this is subtle, do not "simplify" away closeReadEndAsync below):
	// on Windows, os.Pipe() returns handles opened for SYNCHRONOUS I/O
	// (verified against runtime internals: os.Pipe → newFile(..., nonBlocking:
	// false) → poll.FD.Init sets isBlocking=true and skips netpoller
	// registration entirely). *os.File.Close on such a handle calls
	// CancelIoEx (only reliable for *asynchronous*/overlapped I/O — its
	// synchronous counterpart is CancelSynchronousIo, which Close does NOT
	// call) and then unconditionally blocks on a semaphore until any
	// in-flight Read releases its reference. If a Read is genuinely stuck
	// (the escaped-descendant edge case tree.kill() exists to make rare,
	// or simply a panic unwinding through this function's defers while a
	// streamPipe goroutine is mid-Read), calling Close() ourselves would
	// block RUN'S OWN goroutine — exactly the unbounded wait this entire
	// file exists to eliminate, just relocated to pipe teardown instead of
	// pipe draining. So every read-end close in this function is fire-and-
	// forget: we ask the OS to close it, but Run never waits to find out
	// whether that succeeded. Worst case in the pathological escaped-
	// descendant scenario, closing this handle blocks forever in a
	// detached goroutine instead of blocking Run — an isolated, bounded
	// leak of one goroutine + one handle, not a leaked dispatch slot.
	defer closeReadEndAsync(stdoutPipe)
	defer closeReadEndAsync(stderrPipe)

	if err := tree.attach(c); err != nil {
		// Not fatal: the direct child still runs and its own exit code is
		// still meaningful. But descendants may no longer be tracked, so
		// log it — this is the kind of thing that should be diagnosable
		// from logs if grandchildren start surviving commands again.
		s.log.Warn("shell: failed to attach process to tree tracker; descendants may not be killed", "err", err)
	}

	// cmdDone receives the *direct* child's exit status as soon as it's
	// available. Started immediately (concurrently with reading the pipes
	// below), because Run must be able to react to the direct child exiting
	// (to trigger tree.kill() below) without waiting for the pipes to drain
	// first — that ordering (drain-then-wait) is exactly what made the old
	// code's wg.Wait() block forever when a descendant held the pipes open.
	// cmdDone is buffered size 1, so this goroutine can always complete its
	// single send and exit even if nothing ever reads from cmdDone (e.g. a
	// panic elsewhere in Run) — it can never leak blocked on that send.
	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- c.Wait()
	}()

	// out is an internal channel owned entirely by Run. streamPipe goroutines
	// ONLY ever send to `out`, never to `chunks` directly, and `out` is only
	// ever closed by the goroutine below, strictly after both streamPipe
	// calls have already returned. That makes send-on-closed-channel on
	// `out` impossible, and — because `chunks` is closed by Run's own defer,
	// after Run's own goroutine stops reading from `out` — send-on-closed
	// `chunks` is likewise impossible: only Run's own goroutine ever writes
	// to `chunks`, and it always stops writing before its defer closes it.
	out := make(chan ShellChunk, 256)
	var pipeWG sync.WaitGroup
	pipeWG.Add(2)
	go s.streamPipe(stdoutPipe, "stdout", out, &pipeWG)
	go s.streamPipe(stderrPipe, "stderr", out, &pipeWG)
	go func() {
		pipeWG.Wait()
		close(out)
	}()
	// Guarantee `out` always gets drained, on every exit path from this
	// point forward — including a panic unwinding out of the select loop
	// below. Without this, a panic mid-loop would leave the two streamPipe
	// goroutines potentially blocked forever on `out <-` once its 256-slot
	// buffer fills (nothing left reading it), leaking those goroutines
	// permanently. Registered as a defer (fires unconditionally) rather
	// than only being started after the loop, specifically to close that
	// gap. Ranging over an already-closed, already-empty `out` (the common
	// case) returns immediately, so this costs nothing on the normal path.
	defer func() {
		go func() {
			for range out {
			}
		}()
	}()

	var timedOut, forceKilled, exited bool
	var cmdErr error
	ctxDone := ctx.Done()
	var graceTimer *time.Timer
	var graceC <-chan time.Time
	defer func() {
		if graceTimer != nil {
			graceTimer.Stop()
		}
	}()
	startGrace := func() {
		if graceC != nil {
			return // already started (e.g. both ctxDone and cmdDone fired)
		}
		graceTimer = time.NewTimer(postTimeoutGrace)
		graceC = graceTimer.C
	}

loop:
	for {
		select {
		case chunk, ok := <-out:
			if !ok {
				// Both pipes hit EOF — either because the command (and
				// everything it spawned) finished producing output on its
				// own, or because tree.kill() below already forced every
				// process holding a write end to release it.
				break loop
			}
			chunks <- chunk

		case err := <-cmdDone:
			// The direct child process has exited (successfully or not).
			// cmdDone is a buffered channel with exactly one send, already
			// consumed here, so set it nil to disable this case for the
			// rest of the loop rather than selecting on a channel that
			// will never receive again.
			cmdDone = nil
			cmdErr = err
			exited = true
			// Product decision: kill the whole tree the moment the
			// command's own process exits, not only on timeout. A
			// deliberately-backgrounded process (e.g. `Start-Process
			// ... -NoNewWindow` launching a dev server) is not expected to
			// outlive the command that started it. This is also what
			// unblocks the pipe readers above in the common case: once
			// every process holding a write end is killed, our Read()
			// calls see EOF.
			tree.kill()
			startGrace()

		case <-ctxDone:
			// Only handle the deadline once: ctxDone is a closed channel
			// from here on and would otherwise be selected on every loop
			// iteration, busy-spinning until graceC fires.
			ctxDone = nil
			timedOut = true
			chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("\n[runner: command timed out after %ds]\n", timeoutSecs)}
			// exec.CommandContext's default Cancel already calls
			// Process.Kill() on the direct child when ctx is done, but
			// that alone does nothing for descendants. Kill the whole tree
			// explicitly so descendants die too, even if the direct
			// child's own Wait() hasn't fired yet (the cmdDone branch
			// above will still fire separately once it does).
			tree.kill()
			startGrace()

		case <-graceC:
			// The tree has been killed but the pipe readers still haven't
			// finished within the grace period — most likely a descendant
			// escaped the job object / process group in the narrow race
			// window documented in proc_windows.go. Ask the OS to close
			// our read ends to try to unblock any Read() still stuck on
			// them, but do NOT wait to find out whether that succeeds
			// (see closeReadEndAsync — on Windows, Close() on a
			// synchronous pipe handle can itself block on an in-flight
			// Read, and Run must never block on anything, ever). Proceed
			// regardless: Run must never block forever.
			//
			// This is the only path where Run leaks anything (the two
			// streamPipe goroutines, and the closeReadEndAsync goroutines
			// below, all block until the escaped descendant eventually
			// releases the handle on its own). Log it — this should be
			// rare in production, and a customer machine seeing this
			// repeatedly is a real signal something is escaping tree-kill.
			s.log.Warn("shell: process tree force-killed; descendant likely escaped tree-kill and still holds output pipes open",
				"timeout_secs", timeoutSecs)
			forceKilled = true
			closeReadEndAsync(stdoutPipe)
			closeReadEndAsync(stderrPipe)
			break loop
		}
	}

	if forceKilled {
		chunks <- ShellChunk{Stream: "stderr", Data: "[runner: process tree force-killed; some child processes may have kept output pipes open]\n"}
	}

	// The deferred `out`-drain goroutine registered right after `out` was
	// created (see above) takes over from here — no separate drain needed
	// on this normal-return path.

	if !exited {
		// We've stopped waiting on `out`, but haven't yet confirmed the
		// direct child's exit status arrived on cmdDone. In virtually
		// every case this is near-instant — tree.kill() or a natural exit
		// already happened, and Wait() finishes microseconds after the
		// process table reflects that. Bound the wait anyway (never
		// unbounded), and if it's somehow still not in by cmdDoneGrace,
		// report the run as FAILED rather than guessing at success: we
		// genuinely don't know whether the command succeeded, and
		// reporting exit code 0 / OK=true for a run whose outcome was
		// never observed would be actively misleading to a caller (or an
		// LLM agent) deciding whether to proceed.
		select {
		case cmdErr = <-cmdDone:
			exited = true
		case <-time.After(cmdDoneGrace):
			s.log.Warn("shell: cmdDone did not arrive within grace period after pipes closed; reporting failure rather than guessing at success")
			chunks <- ShellChunk{Stream: "stderr", Data: "[runner: could not confirm command exit status; reporting failure]\n"}
		}
	}

	exitCode := 0
	switch {
	case timedOut:
		exitCode = -1
	case ctx.Err() == context.DeadlineExceeded:
		// Edge case: `out` closed (or cmdDone fired) in the same instant
		// ctxDone became ready, so the select loop above happened to take
		// a different branch first and never set timedOut. Still report
		// this as a timeout for consistency with the ctxDone branch.
		chunks <- ShellChunk{Stream: "stderr", Data: fmt.Sprintf("\n[runner: command timed out after %ds]\n", timeoutSecs)}
		exitCode = -1
	case !exited:
		// See the cmdDoneGrace branch above: we never confirmed the exit
		// status, so report failure rather than a possibly-false success.
		exitCode = 1
	case cmdErr != nil:
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	result <- ShellResult{ExitCode: exitCode, OK: exitCode == 0}
}

// closeReadEndAsync closes an *os.File in a detached goroutine rather than
// on the caller's goroutine. This exists specifically because Run must
// NEVER block, and on Windows, *os.File.Close on a pipe read end opened by
// os.Pipe() is a SYNCHRONOUS operation that can itself block for as long
// as a Read() on that same handle is in flight elsewhere (see the detailed
// comment where this is called in Run, next to the `defer
// closeReadEndAsync(...)` lines). We still want the close to happen
// eventually for cleanliness, so we don't just leak the *os.File — we just
// refuse to let Run's own goroutine wait for it. In the by-far-most-common
// case (the pipe has already hit EOF or been unblocked by tree.kill()) this
// goroutine returns near-instantly; in the pathological escaped-descendant
// case it might not, but that's an isolated one-goroutine, one-handle
// leak — not the dispatch-slot leak this file exists to eliminate.
func closeReadEndAsync(f *os.File) {
	go f.Close()
}

func (s *Shell) streamPipe(r io.Reader, stream string, out chan<- ShellChunk, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("streamPipe panic recovered",
				"stream", stream,
				"panic", p,
				"stack", string(debug.Stack()),
			)
		}
	}()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out <- ShellChunk{Stream: stream, Data: string(buf[:n])}
		}
		if err != nil {
			break
		}
	}
}

// RunGit executes a structured git operation and returns combined output.
func (s *Shell) RunGit(op, workingDir string, params map[string]interface{}) (string, error) {
	if workingDir == "" {
		workingDir = s.workspaceRoot
	}

	args, err := buildGitArgs(op, params)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = workingDir

	out, err := c.CombinedOutput()
	output := strings.TrimSpace(string(out))

	if err != nil {
		if output != "" {
			return "", fmt.Errorf("%s\n%s", err.Error(), output)
		}
		return "", err
	}
	return output, nil
}

func buildGitArgs(op string, p map[string]interface{}) ([]string, error) {
	str := func(key string) string {
		v, _ := p[key].(string)
		return v
	}
	boolVal := func(key string) bool {
		v, _ := p[key].(bool)
		return v
	}
	intVal := func(key string, def int) int {
		if v, ok := p[key].(float64); ok {
			return int(v)
		}
		return def
	}

	switch op {
	case "status":
		return []string{"status", "--short"}, nil

	case "diff":
		if boolVal("staged") {
			return []string{"diff", "--staged"}, nil
		}
		return []string{"diff"}, nil

	case "log":
		n := intVal("count", 10)
		return []string{"log", "--oneline", fmt.Sprintf("-n%d", n)}, nil

	case "branch":
		branch := str("branch")
		if boolVal("create") {
			if branch == "" {
				return nil, fmt.Errorf("branch name is required when create=true")
			}
			return []string{"checkout", "-b", branch}, nil
		}
		return []string{"branch", "--list"}, nil

	case "checkout":
		branch := str("branch")
		if branch == "" {
			return nil, fmt.Errorf("branch name is required for checkout")
		}
		return []string{"checkout", branch}, nil

	case "add":
		paths := str("paths")
		if paths != "" {
			return append([]string{"add"}, strings.Fields(paths)...), nil
		}
		return []string{"add", "-A"}, nil

	case "commit":
		msg := str("message")
		if msg == "" {
			return nil, fmt.Errorf("commit message is required")
		}
		return []string{"commit", "-m", msg}, nil

	case "push":
		remote := str("remote")
		if remote == "" {
			remote = "origin"
		}
		branch := str("branch")
		args := []string{"push"}
		if boolVal("set_upstream") {
			args = append(args, "--set-upstream")
		}
		args = append(args, remote)
		if branch != "" {
			args = append(args, branch)
		}
		return args, nil

	case "pull":
		remote := str("remote")
		if remote == "" {
			remote = "origin"
		}
		branch := str("branch")
		args := []string{"pull", remote}
		if branch != "" {
			args = append(args, branch)
		}
		return args, nil

	case "clone":
		repoURL := str("repo_url")
		if repoURL == "" {
			return nil, fmt.Errorf("repo_url is required for clone")
		}
		return []string{"clone", repoURL, "."}, nil

	default:
		return nil, fmt.Errorf("unknown git operation: %q", op)
	}
}
