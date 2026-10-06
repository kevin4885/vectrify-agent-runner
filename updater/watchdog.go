package updater

import (
	"fmt"
	"log/slog"
	"time"
)

// Post-update watchdog - platform-neutral core.
//
// WHY THIS EXISTS: on Windows the updater swaps the binary and then exits
// non-cleanly so the SCM's crash-recovery policy relaunches the service. That
// single mechanism is a single point of failure: when the policy was
// misconfigured the service stayed down for ~11 hours after an update. The
// watchdog is an independent second mechanism. A short-lived detached helper
// process (the NEW binary, started with -post-update-watchdog just before the
// updater exits) supervises the hand-over:
//
//  1. waits for the old process to be gone;
//  2. gives the SCM a grace period to restart the service itself;
//  3. if it has not, starts the service directly;
//  4. waits for the new process to stay up for StableFor;
//  5. if the new version never becomes stable (crash loop), stops the service,
//     restores exe.old, marks the version bad, and starts the old version.
//
// This file holds only the decision logic, written against a small interface
// with an injectable clock so it is fully unit-tested on every OS. The Windows
// SCM adapter and the process spawn live in watchdog_windows.go.

// WatchdogFlags are the command-line flags the detached post-update watchdog
// process is started with. main.go parses them; keep the names in sync.
const (
	FlagWatchdog         = "post-update-watchdog"
	FlagWatchdogService  = "watchdog-service"
	FlagWatchdogOldPID   = "watchdog-old-pid"
	FlagWatchdogVersion  = "watchdog-version"
	FlagWatchdogPrevious = "watchdog-previous"
	FlagWatchdogTarget   = "watchdog-target"
)

type svcState int

const (
	svcStopped svcState = iota
	svcPending          // start/stop/pause/continue pending
	svcRunning
)

// serviceController is the slice of the SCM the watchdog needs.
type serviceController interface {
	// Status returns the service state and the PID of its process (0 if none).
	Status() (state svcState, pid uint32, err error)
	Start() error
	Stop() error
}

// WatchdogOutcome is the end state of a watchdog run.
type WatchdogOutcome int

const (
	// OutcomeHealthy: the new version is running and stayed up for StableFor.
	OutcomeHealthy WatchdogOutcome = iota
	// OutcomeRolledBack: the new version was unhealthy; the previous binary
	// has been restored and started.
	OutcomeRolledBack
	// OutcomeGaveUp: could not get a healthy service and did not (or could
	// not) roll back; a human is needed. Everything was logged.
	OutcomeGaveUp
)

func (o WatchdogOutcome) String() string {
	switch o {
	case OutcomeHealthy:
		return "healthy"
	case OutcomeRolledBack:
		return "rolled-back"
	default:
		return "gave-up"
	}
}

type watchdogParams struct {
	OldPID  uint32
	Version string

	Poll         time.Duration // status poll interval
	RestartGrace time.Duration // how long the SCM gets to restart the service itself
	StartRetry   time.Duration // minimum gap between our own Start calls
	StableFor    time.Duration // new process must stay up this long to count as healthy
	Deadline     time.Duration // overall budget before declaring the update unhealthy
	StopWait     time.Duration // how long to wait for a Stop to take effect
	MaxCrashes   int           // distinct new processes that died before stabilising
	MaxBinErrors int           // Start attempts that failed in a way that implicates the binary

	// IsEnvError reports whether a failed Start is an ENVIRONMENT problem
	// (bad service-account password, disabled service, access denied) rather
	// than evidence the new binary is broken. nil means "none are".
	IsEnvError func(error) bool

	Now   func() time.Time
	Sleep func(time.Duration)
}

func defaultWatchdogParams(oldPID uint32, version string) watchdogParams {
	return watchdogParams{
		OldPID:       oldPID,
		Version:      version,
		Poll:         time.Second,
		RestartGrace: 20 * time.Second, // > the 5s first SCM restart delay
		StartRetry:   15 * time.Second,
		StableFor:    45 * time.Second,
		Deadline:     4 * time.Minute,
		StopWait:     30 * time.Second,
		MaxCrashes:   3,
		MaxBinErrors: 3,
		IsEnvError:   isEnvStartError,
		Now:          time.Now,
		Sleep:        time.Sleep,
	}
}

// runWatchdog supervises the service until it is healthy, rolled back, or the
// budget is spent. swapBack restores the previous binary; markBad records the
// version so the updater will not reinstall it.
func runWatchdog(ctl serviceController, p watchdogParams, swapBack, markBad func() error, log *slog.Logger) WatchdogOutcome {
	begin := p.Now()

	var (
		curPID    uint32 // the new-process PID we are currently observing
		curSince  time.Time
		crashes   int
		downSince = begin
		lastStart time.Time
		envErrs   int // Start refused for environment reasons
		binErrs   int // Start failed in a way that implicates the binary (bad exe, never reported running)
		sawNew    bool
	)

	for p.Now().Sub(begin) < p.Deadline {
		state, pid, err := ctl.Status()
		now := p.Now()
		if err != nil {
			log.Warn("watchdog: querying service failed", "err", err)
			p.Sleep(p.Poll)
			continue
		}

		switch {
		case pid != 0 && pid != p.OldPID:
			// A NEW process owns the service (starting or running).
			sawNew = true
			if pid != curPID {
				if curPID != 0 {
					crashes++
					log.Warn("watchdog: new version's process died before stabilising",
						"pid", curPID, "crashes", crashes)
				}
				curPID, curSince = pid, now
				log.Info("watchdog: new process observed", "pid", pid)
			}
			if state == svcRunning && now.Sub(curSince) >= p.StableFor {
				log.Info("watchdog: new version is healthy", "version", p.Version, "pid", pid, "stable_for", now.Sub(curSince).Round(time.Second))
				return OutcomeHealthy
			}

		case state == svcStopped:
			if curPID != 0 {
				crashes++
				log.Warn("watchdog: service stopped before the new version stabilised",
					"pid", curPID, "crashes", crashes)
				curPID = 0
				downSince = now
			}
			if now.Sub(downSince) >= p.RestartGrace && (lastStart.IsZero() || now.Sub(lastStart) >= p.StartRetry) {
				lastStart = now
				if err := ctl.Start(); err != nil {
					if p.IsEnvError != nil && p.IsEnvError(err) {
						envErrs++
					} else {
						binErrs++
					}
					log.Error("watchdog: starting the service failed", "err", err, "env_errors", envErrs, "binary_errors", binErrs)
				} else {
					log.Warn("watchdog: SCM did not restart the service in time; started it directly",
						"waited", now.Sub(downSince).Round(time.Second))
				}
			}
		}
		// else: a transition (pending, or the old PID not yet gone) - just wait.

		if binErrs >= p.MaxBinErrors {
			return rollbackUpdate(ctl, p, swapBack, markBad, log,
				fmt.Sprintf("the service failed to start %d times in a way that implicates the new binary", binErrs))
		}
		if crashes >= p.MaxCrashes {
			return rollbackUpdate(ctl, p, swapBack, markBad, log,
				fmt.Sprintf("new version crashed %d times without stabilising", crashes))
		}
		p.Sleep(p.Poll)
	}

	if !sawNew && envErrs > 0 && binErrs == 0 {
		// The service never ran, and every Start was refused for an
		// ENVIRONMENT reason (account password changed, logon right revoked,
		// service disabled, ...). That says nothing against the new binary;
		// rolling back would only mark a good release bad. Whereas a Start
		// that fails with "not a valid Win32 application", or times out
		// because the process died before reporting, DOES implicate the
		// binary and falls through to the rollback below.
		log.Error("watchdog: service could not be started at all; NOT rolling back because this looks like an environment problem, not a bad binary. Check the service account and Event Viewer",
			"env_errors", envErrs)
		return OutcomeGaveUp
	}
	return rollbackUpdate(ctl, p, swapBack, markBad, log,
		fmt.Sprintf("new version not stable within %s", p.Deadline))
}

// rollbackUpdate restores the previous binary and starts it.
func rollbackUpdate(ctl serviceController, p watchdogParams, swapBack, markBad func() error, log *slog.Logger, reason string) WatchdogOutcome {
	log.Error("watchdog: new version unhealthy; rolling back to the previous binary", "version", p.Version, "reason", reason)

	// Mark first: even if the rest of the rollback fails half way, the old
	// binary must not be told to install this release again.
	if err := markBad(); err != nil {
		log.Warn("watchdog: could not record bad version", "err", err)
	}

	stopAndWait(ctl, p, log)
	if err := swapBack(); err != nil {
		log.Error("watchdog: ROLLBACK FAILED; trying to start the service anyway", "err", err)
		_ = ctl.Start()
		return OutcomeGaveUp
	}
	// The SCM may have restarted the crashing service while we were swapping;
	// make sure the process that is running is the restored binary.
	stopAndWait(ctl, p, log)
	if err := ctl.Start(); err != nil {
		log.Error("watchdog: rollback done but starting the service failed", "err", err)
		return OutcomeGaveUp
	}
	log.Warn("watchdog: rolled back to the previous version and started the service", "bad_version", p.Version)
	return OutcomeRolledBack
}

func stopAndWait(ctl serviceController, p watchdogParams, log *slog.Logger) {
	if err := ctl.Stop(); err != nil {
		log.Warn("watchdog: stop request failed", "err", err)
	}
	start := p.Now()
	for p.Now().Sub(start) < p.StopWait {
		state, _, err := ctl.Status()
		if err == nil && state == svcStopped {
			return
		}
		p.Sleep(p.Poll)
	}
	log.Warn("watchdog: service did not report stopped in time", "waited", p.StopWait)
}
