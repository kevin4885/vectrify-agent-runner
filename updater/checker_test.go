package updater

import (
	"errors"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSource struct {
	rel      *githubRelease
	err      error
	fails    int
	oks      int
	fetchCnt int
}

func (f *fakeSource) fetch() (*githubRelease, error)  { f.fetchCnt++; return f.rel, f.err }
func (f *fakeSource) noteFailure(*slog.Logger, error) { f.fails++ }
func (f *fakeSource) noteSuccess(*slog.Logger)        { f.oks++ }

type applyCall struct {
	version string
	stillOK bool
}

// newTestChecker wires a checker to fakes. applied records every apply() call.
func newTestChecker(t *testing.T, cur string, rel *githubRelease, act *Activity, applied *[]applyCall) (*checker, *fakeClock, *fakeSource) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(3_000_000, 0)}
	src := &fakeSource{rel: rel}
	exe := filepath.Join(t.TempDir(), "vectrify-runner.exe")
	c := &checker{
		version: cur,
		log:     quietLog(),
		src:     src,
		gate:    newGate(Options{IdleWindow: 5 * time.Minute, Activity: func() Activity { return *act }}, clk.Now),
		exePath: func() (string, error) { return exe, nil },
		applyFn: func(_ string, version string, _ []githubAsset, _ *slog.Logger, _ func(time.Duration), _ func(), _ *updateLock, stillOK func() bool) error {
			*applied = append(*applied, applyCall{version, stillOK()})
			return nil
		},
	}
	return c, clk, src
}

func TestChecker_UpToDate_DoesNothing(t *testing.T) {
	var applied []applyCall
	c, _, _ := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.5"}, &Activity{}, &applied)
	c.checkAndApply()
	if len(applied) != 0 {
		t.Fatal("same version must not apply")
	}
}

func TestChecker_NewerAndIdle_Applies(t *testing.T) {
	var applied []applyCall
	c, _, src := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.6"}, &Activity{}, &applied)
	c.checkAndApply()
	if len(applied) != 1 || applied[0].version != "1.0.6" || !applied[0].stillOK {
		t.Fatalf("applied = %+v, want one apply of 1.0.6 with stillOK", applied)
	}
	if src.oks != 1 {
		t.Errorf("noteSuccess calls = %d, want 1", src.oks)
	}
}

// Decision: nothing is downloaded or locked while the runner is busy.
func TestChecker_NewerButBusy_DoesNotApplyAndDoesNotTakeLock(t *testing.T) {
	var applied []applyCall
	c, _, _ := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.6"}, &Activity{Busy: true}, &applied)
	exe, _ := c.exePath()
	c.checkAndApply()
	if len(applied) != 0 {
		t.Fatal("busy runner must not start an update")
	}
	if exists(filepath.Join(filepath.Dir(exe), lockFileName)) {
		t.Error("a deferred update must not leave/take the update lock")
	}
}

func TestChecker_BecomesIdleLater_Applies(t *testing.T) {
	var applied []applyCall
	act := &Activity{Busy: true}
	c, _, _ := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.6"}, act, &applied)
	c.checkAndApply()
	c.checkAndApply()
	if len(applied) != 0 {
		t.Fatal("still busy")
	}
	act.Busy = false
	c.checkAndApply()
	if len(applied) != 1 {
		t.Fatalf("applied %d times after going idle, want 1", len(applied))
	}
}

func TestChecker_ForcedAfterCap_AppliesEvenWhenBusy_StillOKTrue(t *testing.T) {
	var applied []applyCall
	c, clk, _ := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.6"}, &Activity{Busy: true}, &applied)
	c.checkAndApply()
	clk.Sleep(maxDeferral + time.Minute)
	c.checkAndApply()
	if len(applied) != 1 {
		t.Fatalf("applied %d times, want 1 (forced)", len(applied))
	}
	if !applied[0].stillOK {
		t.Error("a forced update must pass the last-look check, or the cap would never take effect")
	}
}

// The runner got busy while the download was running: apply() backs out with
// errDeferred. That is not an error, and must release the lock.
func TestChecker_ApplyReturnsDeferred_ReleasesLockAndIsNotAFailure(t *testing.T) {
	var applied []applyCall
	c, _, _ := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.6"}, &Activity{}, &applied)
	c.applyFn = func(string, string, []githubAsset, *slog.Logger, func(time.Duration), func(), *updateLock, func() bool) error {
		return errDeferred
	}
	exe, _ := c.exePath()
	c.checkAndApply()
	if exists(filepath.Join(filepath.Dir(exe), lockFileName)) {
		t.Fatal("lock must be released after a deferred apply")
	}
	// ...and a retry is possible straight away.
	c.applyFn = func(_ string, v string, _ []githubAsset, _ *slog.Logger, _ func(time.Duration), _ func(), _ *updateLock, _ func() bool) error {
		applied = append(applied, applyCall{version: v})
		return nil
	}
	c.checkAndApply()
	if len(applied) != 1 {
		t.Fatal("retry after a deferred apply should reach apply() again")
	}
}

func TestChecker_ApplyFails_ReleasesLock(t *testing.T) {
	var applied []applyCall
	c, _, _ := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.6"}, &Activity{}, &applied)
	c.applyFn = func(string, string, []githubAsset, *slog.Logger, func(time.Duration), func(), *updateLock, func() bool) error {
		return errors.New("checksum mismatch")
	}
	exe, _ := c.exePath()
	c.checkAndApply()
	if exists(filepath.Join(filepath.Dir(exe), lockFileName)) {
		t.Fatal("lock must be released after a failed apply")
	}
}

func TestChecker_FetchError_IsCountedAndNotFatal(t *testing.T) {
	var applied []applyCall
	c, _, src := newTestChecker(t, "1.0.5", nil, &Activity{}, &applied)
	src.err = errors.New("network down")
	c.checkAndApply()
	if src.fails != 1 || len(applied) != 0 {
		t.Fatalf("fails=%d applied=%d, want 1,0", src.fails, len(applied))
	}
}

func TestChecker_BadVersionIsSkipped(t *testing.T) {
	var applied []applyCall
	c, _, _ := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.6"}, &Activity{}, &applied)
	exe, _ := c.exePath()
	if err := markBadVersion(exe, "1.0.6"); err != nil {
		t.Fatal(err)
	}
	c.checkAndApply()
	if len(applied) != 0 {
		t.Fatal("a release that was rolled back must not be re-installed")
	}
	// a newer one is still fine
	c.src.(*fakeSource).rel = &githubRelease{TagName: "v1.0.7"}
	c.checkAndApply()
	if len(applied) != 1 || applied[0].version != "1.0.7" {
		t.Fatalf("applied = %+v, want 1.0.7", applied)
	}
}

func TestChecker_PanicInOneCycleDoesNotEscape(t *testing.T) {
	var applied []applyCall
	c, _, _ := newTestChecker(t, "1.0.5", &githubRelease{TagName: "v1.0.6"}, &Activity{}, &applied)
	var calls atomic.Int32
	c.applyFn = func(string, string, []githubAsset, *slog.Logger, func(time.Duration), func(), *updateLock, func() bool) error {
		calls.Add(1)
		panic("boom")
	}
	c.safeCheckAndApply() // must not panic
	if calls.Load() != 1 {
		t.Fatal("apply should have been reached")
	}
}
