//go:build windows

package executor

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procTree wraps a Windows Job Object so that a shell command's ENTIRE
// process tree — including grandchildren spawned by the shell itself
// (e.g. `Start-Process ... -NoNewWindow`, `npm run dev` spawning a node
// server, etc.) — can be killed as a single unit.
//
// WHY THIS EXISTS: exec.CommandContext / os.Process.Kill only ever signals
// the ONE direct child process (powershell.exe). If that child spawns a
// grandchild, the grandchild inherits powershell.exe's stdout/stderr pipe
// write handles. Killing only powershell.exe does NOT close those inherited
// handles — the grandchild still holds them open — so our stdout/stderr
// pipes never see EOF and a naive reader loop blocks forever. A Job Object
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE guarantees that closing the job
// handle (or calling TerminateJobObject) terminates every process that was
// ever assigned to the job, no matter how deep the tree, which forces those
// inherited handles closed and unblocks the pipe readers.
//
// Do NOT "simplify" this back to a plain c.Process.Kill() — that reintroduces
// the exact leak this file exists to fix.
type procTree struct {
	job windows.Handle
}

// newProcTree creates a Job Object configured to kill all member processes
// when the job handle is closed or TerminateJobObject is called.
func newProcTree() (*procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	return &procTree{job: job}, nil
}

// configure is a no-op on Windows (kept for API symmetry with proc_other.go,
// which sets Setpgid). Job Object assignment happens after Start() in attach,
// since the process handle is only available once the process exists.
func (pt *procTree) configure(c *exec.Cmd) {}

// attach assigns the already-started process to the job object so that
// killing the job later kills the whole tree.
//
// NOTE on race windows: there is an inherent short gap between c.Start()
// returning and this call running.
//   - A pathologically fast process could in theory spawn and exit
//     descendants within that gap before assignment completes. In practice
//     this window is microseconds and powershell.exe does not do
//     meaningful work before we regain control here.
//   - More subtly: attach() looks up the process purely by PID
//     (OpenProcess(pid)). If the child has already exited and its PID has
//     been recycled by an unrelated process within that same short gap —
//     rare, but not impossible under heavy process churn — this would
//     attach (and later kill) that unrelated process instead. We accept
//     this narrow, documented risk rather than adding PID-reuse detection
//     (e.g. comparing process creation timestamps via GetProcessTimes),
//     which would meaningfully complicate this function for a benefit that
//     doesn't matter for this use case.
//
// A fully race-free solution to the first issue would require starting the
// process CREATE_SUSPENDED, assigning it to the job, then resuming its main
// thread — meaningfully more complex for a benefit that doesn't matter for
// this use case (LLM-driven shell commands, not adversarial sandboxing). We
// accept both documented, narrow races rather than over-engineer this.
func (pt *procTree) attach(c *exec.Cmd) error {
	if pt == nil || c.Process == nil {
		return nil
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(c.Process.Pid))
	if err != nil {
		return fmt.Errorf("OpenProcess: %w", err)
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(pt.job, h); err != nil {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

// kill terminates every process currently assigned to the job — the whole
// tree, however deep — in one call. Safe to call multiple times / on a nil
// receiver.
func (pt *procTree) kill() {
	if pt == nil || pt.job == 0 {
		return
	}
	// Exit code value is arbitrary; TerminateJobObject just needs *a* value.
	_ = windows.TerminateJobObject(pt.job, 1)
}

// release closes the job handle. Safe to call multiple times / on a nil
// receiver.
func (pt *procTree) release() {
	if pt == nil || pt.job == 0 {
		return
	}
	windows.CloseHandle(pt.job)
	pt.job = 0
}
