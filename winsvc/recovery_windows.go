//go:build windows

package winsvc

import (
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// ROOT-CAUSE NOTE (read before touching this file):
// Windows auto-update ends with the runner exiting non-cleanly
// (updater/apply_windows.go: os.Exit(1)) so the SCM's crash-recovery policy
// relaunches the service on the new binary. On a real machine that policy had
// been left at "Take No Action" (delays present, every action type 0), so
// after the 1.0.22 and 1.0.23 updates the service simply stayed down for hours
// until a human restarted it. install.ps1 sets the right policy once, silently,
// and nothing ever checked it again.
//
// EnsureRestartPolicy closes that hole: the runner verifies - and, if it has
// the rights, repairs - its own service's recovery policy at every start and
// right before every update. The updater ALSO has an independent watchdog
// (updater/watchdog.go) that restarts the service without relying on this
// policy at all; this is the first layer, that is the second.

// resetPeriodSeconds is how long the service must stay up before the SCM
// forgets earlier failures and starts again from the first (shortest) delay.
const resetPeriodSeconds = 3600

// DesiredRecoveryActions is the policy the runner wants: restart after 5s,
// then 10s, then 30s; the SCM repeats the last action for every later
// failure, so the service is restarted forever. Must match install.ps1.
func DesiredRecoveryActions() []mgr.RecoveryAction {
	return []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}
}

// AllRestart reports whether actions is non-empty and EVERY action restarts
// the service. (The SCM repeats the last action once the failure count passes
// the list length, and applies action N-1 on the Nth failure, so a single
// "none" anywhere means some failure will not be recovered.)
func AllRestart(actions []mgr.RecoveryAction) bool {
	if len(actions) == 0 {
		return false
	}
	for _, a := range actions {
		if a.Type != mgr.ServiceRestart {
			return false
		}
	}
	return true
}

// EnsureRestartPolicy makes sure the service called name restarts on crash.
// Returns repaired=true if it had to change the policy. If the policy is bad
// and the process lacks the rights to fix it (service account is not an
// administrator) it returns an error - callers should log it as a warning and
// carry on; the updater's watchdog still covers the update path.
func EnsureRestartPolicy(name string, log *slog.Logger) (repaired bool, err error) {
	c := Controller{Name: name}

	s, err := c.open(windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		return false, fmt.Errorf("reading recovery policy: %w", err)
	}
	actions, err := s.RecoveryActions()
	s.Close()
	if err != nil {
		return false, fmt.Errorf("reading recovery policy: %w", err)
	}
	if AllRestart(actions) {
		log.Debug("service recovery policy ok", "service", name)
		return false, nil
	}

	log.Warn("service recovery policy does NOT restart on crash; auto-update would leave the service stopped. Repairing",
		"service", name, "current", describe(actions))

	// SERVICE_START is required in addition to SERVICE_CHANGE_CONFIG: the SCM
	// refuses (ERROR_ACCESS_DENIED) to store a SC_ACTION_RESTART action unless
	// the caller could itself start the service. Found by the real-SCM test.
	s, err = c.open(windows.SERVICE_QUERY_CONFIG | windows.SERVICE_CHANGE_CONFIG | windows.SERVICE_START)
	if err != nil {
		return false, fmt.Errorf("cannot repair recovery policy (service account needs administrator rights; re-run install.ps1 or `sc.exe failure %s reset= %d actions= restart/5000/restart/10000/restart/30000`): %w",
			name, resetPeriodSeconds, err)
	}
	defer s.Close()
	if err := s.SetRecoveryActions(DesiredRecoveryActions(), resetPeriodSeconds); err != nil {
		return false, fmt.Errorf("setting recovery policy: %w", err)
	}
	// Read back: never trust that a write that returned success stuck.
	after, err := s.RecoveryActions()
	if err != nil || !AllRestart(after) {
		return false, fmt.Errorf("recovery policy did not stick after repair (read back %s, err %v)", describe(after), err)
	}
	log.Info("service recovery policy repaired", "service", name, "now", describe(after))
	return true, nil
}

func describe(actions []mgr.RecoveryAction) string {
	if len(actions) == 0 {
		return "none configured"
	}
	names := map[int]string{
		mgr.NoAction:       "none",
		mgr.ServiceRestart: "restart",
		mgr.ComputerReboot: "reboot",
		mgr.RunCommand:     "command",
	}
	out := ""
	for i, a := range actions {
		if i > 0 {
			out += ", "
		}
		n, ok := names[a.Type]
		if !ok {
			n = fmt.Sprintf("type%d", a.Type)
		}
		out += fmt.Sprintf("%s/%s", n, a.Delay)
	}
	return out
}
