//go:build windows

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"vectrify/agent-runner/winsvc"
)

// scmController adapts winsvc.Controller to serviceController.
type scmController struct{ c winsvc.Controller }

func (s scmController) Status() (svcState, uint32, error) {
	st, err := s.c.Status()
	if err != nil {
		return svcStopped, 0, err
	}
	switch st.State {
	case svc.Running:
		return svcRunning, st.ProcessId, nil
	case svc.Stopped:
		return svcStopped, 0, nil
	default:
		return svcPending, st.ProcessId, nil
	}
}
func (s scmController) Start() error { return s.c.Start() }
func (s scmController) Stop() error  { return s.c.Stop() }

// spawnWatchdog starts the detached post-update watchdog. exePath is the
// freshly installed (NEW) binary the service will run; previous is where the
// old one was moved.
//
// The watchdog is run from the PREVIOUS binary, not the new one, on purpose:
// its job includes noticing that the new binary is broken, and a broken
// binary cannot reliably supervise (or roll back) itself. The previous binary
// is the code that is running right now, so it is known to work. Windows
// allows renaming a running executable, so restorePrevious can still move it
// back into place while it runs. Called just before the updater exits so the
// hand-over is supervised regardless of whether the SCM's own restart works.
func spawnWatchdog(exePath, serviceName, version, previous string, log *slog.Logger) error {
	args := []string{}
	// Pass through the service's own arguments (--config <path>) so the
	// watchdog finds the same config and log file.
	args = append(args, os.Args[1:]...)
	args = append(args,
		"-"+FlagWatchdog,
		"-"+FlagWatchdogService+"="+serviceName,
		"-"+FlagWatchdogOldPID+"="+strconv.Itoa(os.Getpid()),
		"-"+FlagWatchdogVersion+"="+version,
		"-"+FlagWatchdogPrevious+"="+previous,
		"-"+FlagWatchdogTarget+"="+exePath,
	)
	cmd := exec.Command(previous, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Detached, no console, own process group: it must outlive this
		// process (which is about to exit) and must not flash a window.
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting post-update watchdog: %w", err)
	}
	log.Info("auto-update: post-update watchdog started", "pid", cmd.Process.Pid, "service", serviceName)
	_ = cmd.Process.Release()
	return nil
}

// RunWatchdogProcess is the entry point of the detached watchdog process
// (main.go calls it when started with -post-update-watchdog). It returns the
// outcome so main can choose an exit code; it never panics the caller.
func RunWatchdogProcess(serviceName, version, previous, exePath string, oldPID uint32, log *slog.Logger) (out WatchdogOutcome) {
	defer func() {
		if p := recover(); p != nil {
			log.Error("watchdog panic recovered", "panic", p)
			out = OutcomeGaveUp
		}
	}()

	if exePath == "" || previous == "" {
		log.Error("watchdog: missing target or previous binary path", "target", exePath, "previous", previous)
		return OutcomeGaveUp
	}
	if serviceName == "" {
		serviceName = winsvc.DefaultName
	}
	log.Info("watchdog: supervising update hand-over",
		"service", serviceName, "version", version, "old_pid", oldPID, "previous", previous)

	ctl := scmController{c: winsvc.Controller{Name: serviceName}}
	swapBack := func() error { return restorePrevious(exePath, previous) }
	markBad := func() error { return markBadVersion(exePath, version) }

	out = runWatchdog(ctl, defaultWatchdogParams(oldPID, version), swapBack, markBad, log)
	log.Info("watchdog: finished", "outcome", out.String())
	if out == OutcomeHealthy {
		// The new version has proven itself; the previous binary is no longer
		// needed as a rollback target. Best-effort: this process may itself be
		// running from it (so Windows refuses the delete) - the service's own
		// delayed cleanup (updater.Start) removes it later.
		removeLeftovers(exePath)
	}
	return out
}
