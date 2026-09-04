//go:build !windows

package executor

import (
	"os/exec"
	"syscall"
)

// procTree groups a shell command's process tree under its own process
// group ID (PGID) so it can be killed as a unit with a single signal to the
// negative PID.
//
// WHY THIS EXISTS: exec.CommandContext / os.Process.Kill only ever signals
// the ONE direct child process (bash). If bash spawns a grandchild (a
// background server, `npm run dev`, a detached daemon via `nohup ... &`),
// that grandchild inherits bash's stdout/stderr pipe file descriptors.
// Killing only bash does NOT close those inherited descriptors — the
// grandchild still holds them open for writing — so our stdout/stderr pipes
// never see EOF and a naive reader loop blocks forever. Setting Setpgid puts
// bash and everything it forks into a single process group; sending SIGKILL
// to the negative of that PGID kills every member of the group in one shot,
// which forces the inherited descriptors closed and unblocks the pipe
// readers.
//
// Do NOT "simplify" this back to a plain c.Process.Kill() — that reintroduces
// the exact leak this file exists to fix.
type procTree struct {
	pgid int
}

// newProcTree is a no-op constructor kept for symmetry with proc_windows.go
// (which needs to create a Job Object handle before Start()). On Unix there
// is no OS resource to allocate up front — the process group is created by
// the kernel implicitly via Setpgid in configure().
func newProcTree() (*procTree, error) {
	return &procTree{}, nil
}

// configure sets SysProcAttr.Setpgid so the child becomes the leader of a
// new process group, before Start() is called.
func (pt *procTree) configure(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
}

// attach records the PGID (== the child's PID, since it's the group leader)
// once the process has started.
func (pt *procTree) attach(c *exec.Cmd) error {
	if pt == nil || c.Process == nil {
		return nil
	}
	pt.pgid = c.Process.Pid
	return nil
}

// kill sends SIGKILL to the entire process group in one syscall. Safe to
// call multiple times / on a nil receiver.
//
// CAVEAT: this kills every process still a member of the group, but a
// descendant that calls setsid(2) (or otherwise detaches into a new
// session/process group — e.g. many daemonization libraries, `nohup`
// implementations that also call setsid, or double-forking daemons) leaves
// this group entirely and will NOT be killed by this call. That escape
// case is exactly what the bounded grace period + forced pipe-close in
// shell.go exist to handle: Run still cannot hang forever even if this
// kill doesn't reach every descendant, but such an escaped descendant can
// legitimately keep running after Run returns.
func (pt *procTree) kill() {
	if pt == nil || pt.pgid == 0 {
		return
	}
	// Negative PID => signal the whole process group, not just the leader.
	_ = syscall.Kill(-pt.pgid, syscall.SIGKILL)
}

// release exists only for API symmetry with proc_windows.go (which must
// close a Job Object handle). There is no OS resource to release on Unix.
func (pt *procTree) release() {}
