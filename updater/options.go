package updater

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	// defaultCheckInterval is how often the runner asks GitHub whether a newer
	// release exists. Cheap because the request is conditional (ETag, see
	// release.go): an unchanged release is a 304 with no body.
	defaultCheckInterval = 5 * time.Minute

	// defaultIdleWindow is how long the runner must have been quiet (no command
	// received or finished, none in flight) before it will update itself.
	defaultIdleWindow = 5 * time.Minute

	// maxDeferral bounds how long an available update may be postponed by
	// activity. After this much continuous deferral the update is applied
	// anyway (still after the normal drain of in-flight commands), so a runner
	// that is never quiet cannot stay on an old - possibly insecure or broken -
	// version forever.
	maxDeferral = 6 * time.Hour

	// deferReminderEvery is how often a still-deferred update is re-announced
	// at INFO level (every other deferral is DEBUG, so a busy runner does not
	// log every check).
	deferReminderEvery = time.Hour
)

// Activity is a point-in-time view of how busy the runner is. The client
// supplies it; the updater never reaches into the client.
type Activity struct {
	// Busy is true while any command is in flight or waiting for a slot.
	Busy bool
	// LastActive is the last time a command was received OR finished. The
	// zero value means "no activity yet".
	LastActive time.Time
}

// Options tunes the auto-update loop. The zero value of each field selects the
// default, so a caller may set only what it cares about.
type Options struct {
	// CheckInterval is how often to look for a new release.
	CheckInterval time.Duration
	// IdleWindow is the required quiet period before an update may be applied.
	IdleWindow time.Duration
	// Activity reports current activity. nil means "always idle" (tests only).
	Activity func() Activity
}

func (o Options) withDefaults() Options {
	if o.CheckInterval <= 0 {
		o.CheckInterval = defaultCheckInterval
	}
	if o.IdleWindow <= 0 {
		o.IdleWindow = defaultIdleWindow
	}
	return o
}

// errDeferred is returned by apply() when the runner became active between the
// first idle check and the moment just before the binary swap. It is not a
// failure: the temp download is discarded and the next check retries.
var errDeferred = errors.New("update deferred: runner became active")

// gate decides whether it is a good moment to update. It is only ever used from
// the single updater goroutine, so it needs no locking.
type gate struct {
	opts Options
	now  func() time.Time

	deferredSince time.Time // zero = not currently deferring
	lastReminder  time.Time
}

func newGate(opts Options, now func() time.Time) *gate {
	return &gate{opts: opts.withDefaults(), now: now}
}

// busyReason returns "" when the runner has been idle for the whole idle
// window, otherwise a human-readable reason it should not update yet.
func (g *gate) busyReason() string {
	if g.opts.Activity == nil {
		return ""
	}
	a := g.opts.Activity()
	if a.Busy {
		return "a command is in flight"
	}
	if a.LastActive.IsZero() {
		return ""
	}
	idle := g.now().Sub(a.LastActive)
	if idle < g.opts.IdleWindow {
		return fmt.Sprintf("last command activity was %s ago (need %s idle)",
			idle.Round(time.Second), g.opts.IdleWindow)
	}
	return ""
}

// evaluate is called when a newer release exists. proceed=false means "not now,
// try again next check". forced=true means the deferral cap was hit, so the
// caller must update even though the runner is active.
func (g *gate) evaluate(log *slog.Logger, current, latest string) (proceed, forced bool) {
	reason := g.busyReason()
	if reason == "" {
		g.reset()
		return true, false
	}

	now := g.now()
	if g.deferredSince.IsZero() {
		g.deferredSince, g.lastReminder = now, now
		log.Info("auto-update: new version available but the runner is active; deferring until idle",
			"current", current, "latest", latest, "reason", reason, "idle_window", g.opts.IdleWindow)
		return false, false
	}

	waited := now.Sub(g.deferredSince)
	if waited >= maxDeferral {
		log.Warn("auto-update: update has been deferred for the maximum time; applying now despite activity",
			"current", current, "latest", latest, "deferred_for", waited.Round(time.Minute), "max", maxDeferral)
		return true, true
	}
	if now.Sub(g.lastReminder) >= deferReminderEvery {
		g.lastReminder = now
		log.Info("auto-update: update still deferred (runner keeps being active)",
			"latest", latest, "deferred_for", waited.Round(time.Minute), "reason", reason)
	} else {
		log.Debug("auto-update: update deferred", "latest", latest, "reason", reason)
	}
	return false, false
}

// reset clears the deferral clock. Called when nothing is pending (up to date)
// or when the gate lets an update through.
func (g *gate) reset() {
	g.deferredSince = time.Time{}
	g.lastReminder = time.Time{}
}
