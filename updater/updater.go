// Package updater checks for new releases on GitHub and self-updates the binary.
//
// On startup and every CheckInterval (default 5 min) the runner looks for a new
// release (via the github.com redirect - see release.go - not the rate-limited
// API). If a newer version is found, and the runner has been idle for
// IdleWindow (default 5 min; see options.go), it:
//   1. Downloads the binary for the current platform.
//   2. Downloads the SHA256 manifest and verifies the download before touching
//      the running binary.  The update is aborted if the checksum does not match.
//   3. Calls drain() to let in-flight commands finish (up to drainTimeout).
//   4. Windows: renames the running exe aside and moves the new one into
//      place in-process (swap.go), then exits so the SCM failure/restart
//      policy relaunches the service on the new binary. No helper script.
//      Linux/macOS: writes a tiny shell script, spawns it detached, and exits;
//      the script stops the service, swaps the binary, and restarts.
//
// Disabled automatically for dev builds (version == "dev").
package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"
)

const (
	githubRepo = "kevin4885/vectrify-agent-runner"

	// drainTimeout is the maximum time we wait for in-flight commands to finish
	// before applying an update and exiting.  Keeps updates snappy while still
	// giving short-running commands a chance to complete.
	drainTimeout = 30 * time.Second

	// leftoverGrace is how long after startup the previous binary
	// (exe.old) is kept before the best-effort cleanup removes it. It must
	// exceed the post-update watchdog's Deadline (4 min) so a rollback
	// target is never deleted while the watchdog may still need it.
	leftoverGrace = 6 * time.Minute
)

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Start launches the background auto-update loop.
// drain is called with drainTimeout before the process exits to let in-flight
// commands finish.  Pass nil to skip draining (e.g. in tests).
// beforeExit is called (if non-nil) right before the process exits to finish
// an update, for cleanup the OS would otherwise get from a service stop
// (e.g. closing the Playwright browser).
// opts sets the check interval, the idle window and the activity source (see
// options.go); zero values select the defaults.
// Returns immediately; the check runs in a goroutine.
// Does nothing for dev builds (version == "dev").
func Start(currentVersion string, log *slog.Logger, drain func(time.Duration), beforeExit func(), opts Options) {
	if currentVersion == "dev" {
		log.Debug("auto-update: disabled in dev build")
		return
	}
	opts = opts.withDefaults()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("updater panic recovered",
					"panic", p,
					"stack", string(debug.Stack()),
				)
			}
		}()
		// Best-effort: remove the previous binary / legacy script an earlier
		// update left behind (see swap.go). DELAYED, not immediate: right
		// after an update, exe.old is the rollback target of the post-update
		// watchdog (watchdog.go). Deleting it at startup would leave the
		// watchdog nothing to roll back to. leftoverGrace outlasts the
		// watchdog's whole budget; the watchdog also deletes it itself the
		// moment the new version proves healthy.
		if exePath, err := os.Executable(); err == nil {
			time.AfterFunc(leftoverGrace, func() { removeLeftovers(exePath) })
		}

		c := newChecker(currentVersion, log, drain, beforeExit, opts)
		c.checkAndApply()
		ticker := time.NewTicker(opts.CheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			// A panic in one cycle must not end the loop for good: the
			// runner would then silently never update again.
			c.safeCheckAndApply()
		}
	}()
}

// releaseSource is the part of releaseFetcher the checker needs (a seam for tests).
type releaseSource interface {
	fetch() (*githubRelease, error)
	noteFailure(log *slog.Logger, err error)
	noteSuccess(log *slog.Logger)
}

// checker holds the state that must survive between checks: the deferral clock
// and the failure counter.
type checker struct {
	version    string
	log        *slog.Logger
	drain      func(time.Duration)
	beforeExit func()

	src  releaseSource
	gate *gate

	// seams, overridden in tests
	exePath func() (string, error)
	applyFn func(exePath, version string, assets []githubAsset, log *slog.Logger,
		drain func(time.Duration), beforeExit func(), lock *updateLock, stillOK func() bool) error
}

func newChecker(version string, log *slog.Logger, drain func(time.Duration), beforeExit func(), opts Options) *checker {
	return &checker{
		version:    version,
		log:        log,
		drain:      drain,
		beforeExit: beforeExit,
		src:        newReleaseFetcher(githubRepo, version),
		gate:       newGate(opts, time.Now),
		exePath:    os.Executable,
		applyFn:    apply,
	}
}

func (c *checker) safeCheckAndApply() {
	defer func() {
		if p := recover(); p != nil {
			c.log.Error("auto-update: check panicked; will retry next interval",
				"panic", p, "stack", string(debug.Stack()))
		}
	}()
	c.checkAndApply()
}

func (c *checker) checkAndApply() {
	log := c.log
	rel, err := c.src.fetch()
	if err != nil {
		c.src.noteFailure(log, err)
		return
	}
	c.src.noteSuccess(log)

	latest := rel.TagName
	if len(latest) > 0 && latest[0] == 'v' {
		latest = latest[1:]
	}
	if !isNewer(c.version, latest) {
		log.Debug("auto-update: up to date", "version", c.version)
		c.gate.reset()
		return
	}

	exePath, err := c.exePath()
	if err != nil {
		log.Warn("auto-update: could not resolve own executable path", "err", err)
		return
	}

	// A release that was installed, could not stay up, and was rolled back by
	// the watchdog must not be re-installed - otherwise every check would
	// re-download the same broken release and crash-loop the runner again. A
	// newer release (different version string) is still eligible. Checks now
	// run every few minutes, so only say it once, not on every check.
	if isBadVersion(exePath, latest) {
		log.Debug("auto-update: skipping release that previously failed and was rolled back",
			"current", c.version, "latest", latest)
		c.gate.reset()
		return
	}

	// Idle gate: do not restart the runner under someone's feet. Decided BEFORE
	// taking the lock or downloading anything, so a busy runner does no work.
	proceed, forced := c.gate.evaluate(log, c.version, latest)
	if !proceed {
		return
	}

	// See lock.go's ROOT-CAUSE NOTE: this lock is the fix that guarantees
	// two runner processes on the same machine (e.g. a freshly
	// SCM/systemd/launchd-restarted old binary racing a still-mid-update
	// old binary) can never both attempt to swap the same file at once.
	lock, err := acquireUpdateLock(exePath)
	if err != nil {
		log.Warn("auto-update: failed to acquire update lock; skipping this cycle", "err", err)
		return
	}
	if lock == nil {
		log.Info("auto-update: another update is already in progress on this machine (lock held); skipping this cycle",
			"current", c.version, "latest", latest)
		return
	}
	// NOT deferred: apply() calls os.Exit on its own success path, and
	// deferred functions never run through os.Exit. Correctness therefore
	// relies on explicit release on every path instead:
	//   - apply() returns an error       -> released right below by US.
	//   - apply() succeeds and os.Exit's -> released by apply() itself, right
	//     after the binary swap completes (see apply_windows.go) or by the
	//     detached swap script (apply_other.go) - not by any Go code here,
	//     since the process is gone by then.
	log.Info("auto-update: new version available", "current", c.version, "latest", latest)

	// stillOK is consulted by apply() once the download is verified, just
	// before the point of no return: the download takes a while, and a command
	// may have arrived meanwhile. A forced update (deferral cap hit) skips it.
	stillOK := func() bool { return forced || c.gate.busyReason() == "" }

	if err := c.applyFn(exePath, latest, rel.Assets, log, c.drain, c.beforeExit, lock, stillOK); err != nil {
		lock.release()
		if errors.Is(err, errDeferred) {
			// Not a failure: the runner got busy while we downloaded. The
			// temp file is already gone; the next check starts over.
			log.Info("auto-update: runner became active during download; deferring until idle", "latest", latest)
			return
		}
		log.Error("auto-update: failed", "err", err)
	}
}
// isNewer returns true if latest is a higher semver than current.
func isNewer(current, latest string) bool {
	if current == latest {
		return false
	}
	var cMaj, cMin, cPat int
	var lMaj, lMin, lPat int
	fmt.Sscanf(current, "%d.%d.%d", &cMaj, &cMin, &cPat)
	fmt.Sscanf(latest, "%d.%d.%d", &lMaj, &lMin, &lPat)
	if lMaj != cMaj {
		return lMaj > cMaj
	}
	if lMin != cMin {
		return lMin > cMin
	}
	return lPat > cPat
}

// assetURL returns the browser download URL for a named asset from the release,
// or an error if the asset is not present.
func assetURL(assets []githubAsset, name string) (string, error) {
	for _, a := range assets {
		if a.Name == name {
			return a.BrowserDownloadURL, nil
		}
	}
	return "", fmt.Errorf("asset %q not found in release", name)
}

// downloadFile downloads url and writes it to dest, replacing any existing file.
func downloadFile(url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dest, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("writing %s: %w", dest, err)
	}
	return nil
}

// downloadSHA256Manifest fetches the checksum file and returns a map of
// filename -> expected hex SHA256.  The manifest is expected to be in the
// standard `sha256sum` format:
//
//	<hex>  <filename>
func downloadSHA256Manifest(url string) (map[string]string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("manifest download returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	sums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		// sha256sum uses "  filename" (two spaces) or " *filename" (space+asterisk)
		name := strings.TrimPrefix(parts[1], "*")
		sums[name] = parts[0]
	}
	return sums, nil
}

// verifySHA256 computes the SHA256 of the file at path and compares it to
// expected (hex string).  Returns an error if they do not match.
func verifySHA256(path, expected string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening file for checksum: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing file: %w", err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("SHA256 mismatch: got %s, expected %s", got, expected)
	}
	return nil
}
