// Package executor: browser.go implements Playwright-driven browser
// automation ("browser" command type) — navigate, click, fill, screenshot,
// extract text/HTML, evaluate JS, wait-for-selector — against a small pool
// of stateful sessions that persist across multiple commands until closed
// or reaped for inactivity.
//
// Gated behind config.AllowShell (see runner/runner.go handleBrowser) —
// browser automation shares the shell permission rather than having its
// own allow_browser setting. Requires the Playwright driver + Chromium
// binaries to already be installed on this machine; see
// `vectrify-runner -install-browsers` in main.go.
//
// Stealth: every launched context disables the Blink automation-controlled
// flag, uses a realistic UA/viewport/locale/timezone tuple, and injects the
// evasion script vendored at executor/stealth.min.js (sourced from
// github.com/jonfriesen/playwright-go-stealth v0.0.3 — see
// stealth.min.js.LICENSE) into every new page. This raises the bar against
// basic/medium bot detection but is NOT a guarantee against advanced
// systems (Cloudflare Turnstile with behavioral scoring, Akamai,
// PerimeterX/DataDome) — see CLAUDE.md.
package executor

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mxschmitt/playwright-go"
)

//go:embed stealth.min.js
var stealthJS string

// stealthUserAgent is a realistic, current desktop Chrome UA string used for
// every launched context instead of Playwright's default headless UA
// (which itself is a well-known bot-detection signal). Paired with the
// matching Sec-Ch-Ua headers below and the stealth init script.
const stealthUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// browserSession holds one open Playwright browser context + page, kept
// alive across multiple "browser" commands until explicitly closed or
// reaped for inactivity.
//
// inUse/busy guard against three concurrency hazards that a bare map entry
// would allow: (1) the idle reaper closing a context out from under an
// action that is still running because lastUsedAt is only bumped when an
// action *starts*, not for its whole duration; (2) an explicit `close`
// racing an in-flight action on the same session_id; (3) two concurrent
// commands on the same session_id interleaving actions on one Page with no
// ordering, which would defeat the entire point of a stateful session.
// acquire/release below serialize access — the reaper and closeSession both
// skip (defer) any session currently busy rather than closing underneath it.
type browserSession struct {
	context    playwright.BrowserContext
	page       playwright.Page
	lastUsedAt time.Time

	busyMu sync.Mutex // held for the duration of exactly one action
}

// acquire blocks until no other action is running against this session,
// then marks it busy and refreshes lastUsedAt (so a long-running action
// cannot be reaped mid-flight — see reapOnce). release must be called
// exactly once, however the action concludes, via `defer s.release()`.
func (s *browserSession) acquire() {
	s.busyMu.Lock()
	s.lastUsedAt = time.Now()
}

func (s *browserSession) release() {
	s.lastUsedAt = time.Now()
	s.busyMu.Unlock()
}

// BrowserManager owns a single shared Playwright + Browser process and a
// bounded pool of sessions keyed by an API-supplied session_id. One
// BrowserManager is created per Runner (see runner.New) and lives for the
// process lifetime.
type BrowserManager struct {
	workspaceRoot string
	log           *slog.Logger
	maxSessions   int
	idleTimeout   time.Duration
	headless      bool

	mu           sync.Mutex
	pw           *playwright.Playwright
	browser      playwright.Browser
	sessions     map[string]*browserSession
	reaperOnce   sync.Once
	stopReaper   chan struct{}
	shutdownOnce sync.Once
	shuttingDown bool

	// Single-flight + failure-backoff state for the driver/Chromium
	// install. Guarded by mu. installing is non-nil while one goroutine is
	// actively running playwright.Install() — every other concurrent
	// caller waits on installing.done instead of starting a duplicate
	// ~300MB download. lastInstallErr/lastInstallErrAt cache the most
	// recent failure so a machine with no egress (or a full disk, or a
	// blocking proxy) doesn't re-attempt the full download on every single
	// browser command — see installFailureBackoff.
	installing       *installAttempt
	lastInstallErr   error
	lastInstallErrAt time.Time
}

// installAttempt tracks one in-flight EnsureInstalled call so concurrent
// callers can wait for it instead of starting a duplicate install.
type installAttempt struct {
	done chan struct{}
	err  error
}

// installFailureBackoff is how long EnsureInstalled short-circuits with the
// cached error after an install failure, instead of immediately retrying
// the full ~300MB download on the very next browser command. Each attempt
// otherwise holds a heavy dispatch slot for the duration of a failing
// download (e.g. against a dead proxy) — this bounds how often that can
// happen without requiring the customer to notice and intervene.
const installFailureBackoff = 30 * time.Second

// NewBrowserManager creates a BrowserManager. The underlying Playwright
// driver and Chromium browser process are started lazily on the first
// browser command, not here — most runners will never use this feature, so
// paying the driver-startup cost at process boot for every customer would
// be wasteful.
func NewBrowserManager(workspaceRoot string, maxSessions int, idleTimeout time.Duration, headless bool, log *slog.Logger) *BrowserManager {
	if maxSessions <= 0 {
		maxSessions = 3
	}
	m := &BrowserManager{
		workspaceRoot: workspaceRoot,
		log:           log,
		maxSessions:   maxSessions,
		idleTimeout:   idleTimeout,
		headless:      headless,
		sessions:      make(map[string]*browserSession),
		stopReaper:    make(chan struct{}),
	}
	return m
}

// ensureStarted lazily starts the Playwright driver + Chromium browser the
// first time it is needed, and starts the idle-reaper goroutine exactly
// once. Thin wrapper around EnsureInstalled with no progress channel —
// used by the normal action path (getOrCreateSession) where the driver is
// virtually always already installed (EnsureInstalled having been called
// explicitly by runner.handleBrowser first); kept as a self-healing
// fallback for any call path that skips that explicit call.
func (m *BrowserManager) ensureStarted() error {
	return m.EnsureInstalled(nil)
}

// EnsureInstalled makes sure the Playwright driver + Chromium browser are
// installed and the shared browser process is running — installing them
// automatically (streaming progress text on progressCh, if non-nil) the
// first time this is ever called on a given machine, instead of requiring
// the customer to have run `vectrify-runner -install-browsers` by hand.
//
// Call this explicitly (with a progress channel) at the top of
// runner.handleBrowser, before dispatching to any action, so the ~300MB
// one-time download streams back to the caller instead of the command
// silently hanging for up to a couple of minutes on a customer's very
// first browser command. Safe to call on every command — it is a
// near-instant no-op once the browser is already running.
//
// Two properties that a naive "lock, check, install, unlock" implementation
// would not have, both needed because this can be called concurrently by
// multiple in-flight browser commands:
//   - Single-flight: if a download is already in progress when a second
//     caller arrives, the second caller waits for the same attempt instead
//     of starting a duplicate ~300MB download.
//   - Failure backoff: a failed install (no egress, full disk, blocking
//     proxy) is cached for installFailureBackoff instead of being retried
//     on every subsequent command — otherwise a misconfigured machine pays
//     the full failing-download cost (and a heavy dispatch slot) on every
//     single browser command an agent attempts.
//
// Deliberately does NOT hold mu for the duration of the download itself
// (unlike an earlier version of this code) — only to check/set the
// single-flight state, and again afterward to install the resulting
// browser/pw. Holding mu across a multi-minute download would block every
// other session operation and, critically, Shutdown() — which would hang
// a Windows service stop / reboot for the whole download if one happened
// to be in flight.
func (m *BrowserManager) EnsureInstalled(progressCh chan<- InstallProgress) error {
	m.mu.Lock()
	if m.browser != nil {
		m.mu.Unlock()
		return nil // already started — fast path, no install needed
	}
	if m.shuttingDown {
		m.mu.Unlock()
		return fmt.Errorf("runner is shutting down")
	}
	if attempt := m.installing; attempt != nil {
		// Another caller is already installing — wait for it instead of
		// starting a second concurrent download.
		m.mu.Unlock()
		<-attempt.done
		return attempt.err
	}
	if m.lastInstallErr != nil && time.Since(m.lastInstallErrAt) < installFailureBackoff {
		err := m.lastInstallErr
		m.mu.Unlock()
		return fmt.Errorf("browser driver install failed recently, retrying in %s: %w",
			(installFailureBackoff - time.Since(m.lastInstallErrAt)).Round(time.Second), err)
	}
	attempt := &installAttempt{done: make(chan struct{})}
	m.installing = attempt
	m.mu.Unlock()

	err := m.doInstallAndStart(progressCh)

	m.mu.Lock()
	m.installing = nil
	if err != nil {
		m.lastInstallErr = err
		m.lastInstallErrAt = time.Now()
	} else {
		m.lastInstallErr = nil
	}
	m.mu.Unlock()

	attempt.err = err
	close(attempt.done)
	return err
}

// doInstallAndStart runs the actual driver-start / auto-install / Chromium
// launch sequence with no lock held, then takes mu only briefly to publish
// the result. Called with mu NOT held (see EnsureInstalled).
func (m *BrowserManager) doInstallAndStart(progressCh chan<- InstallProgress) error {
	runLogger := slog.New(&progressLogHandler{ch: progressCh})

	pw, err := playwright.Run(&playwright.RunOptions{Logger: runLogger, Verbose: true})
	if err == nil {
		return m.finishStart(pw)
	}
	if !strings.Contains(err.Error(), "install the driver") {
		// A different failure (e.g. a corrupt partial install, or a
		// permissions problem) — auto-installing over it is unlikely to
		// help and could mask the real cause, so surface it as-is rather
		// than silently attempting an install.
		return fmt.Errorf("starting playwright driver: %w", err)
	}

	// First-ever browser command on this machine: the driver isn't
	// installed. Auto-install it now instead of making the customer run
	// `vectrify-runner -install-browsers` by hand.
	if progressCh != nil {
		progressCh <- InstallProgress{
			Data: "Playwright driver not found on this machine — installing now " +
				"(one-time download, ~300MB, may take a couple of minutes)...\n",
		}
	}
	w := &progressWriter{ch: progressCh}
	if err := playwright.Install(&playwright.RunOptions{
		Stdout:   w,
		Stderr:   w,
		Logger:   runLogger,
		Verbose:  true,
		Browsers: []string{"chromium"}, // only Chromium is used by BrowserManager; skip Firefox/WebKit to keep this a ~300MB download, not ~600MB+
	}); err != nil {
		return fmt.Errorf("auto-installing playwright driver: %w", err)
	}
	if progressCh != nil {
		progressCh <- InstallProgress{Data: "\nInstall complete.\n"}
	}

	pw, err = playwright.Run(&playwright.RunOptions{Logger: runLogger, Verbose: true})
	if err != nil {
		return fmt.Errorf("starting playwright driver after auto-install: %w", err)
	}
	return m.finishStart(pw)
}

// finishStart launches Chromium given an already-running Playwright driver
// connection and starts the idle reaper. Takes mu itself (called with no
// lock held, from doInstallAndStart).
func (m *BrowserManager) finishStart(pw *playwright.Playwright) error {
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(m.headless),
		// Reduces one of the more visible automation fingerprints
		// (navigator.webdriver aside, this flag also disables several
		// related Blink-internal automation hooks). See
		// github.com/jonfriesen/playwright-go-stealth's README, which
		// recommends this alongside the injected stealth script.
		Args: []string{"--disable-blink-features=AutomationControlled"},
	})
	if err != nil {
		_ = pw.Stop()
		return fmt.Errorf("launching chromium: %w", err)
	}
	m.mu.Lock()
	m.pw = pw
	m.browser = browser
	m.mu.Unlock()
	m.reaperOnce.Do(func() { go m.reapIdleLoop() })
	return nil
}

// InstallProgress carries one chunk of stdout/stderr text produced while
// EnsureInstalled auto-downloads the Playwright driver + Chromium browser.
// Mirrors executor.ShellChunk's role for shell commands — runner.go
// forwards each chunk to the API as a protocol.StreamMsg.
type InstallProgress struct {
	Data string
}

// progressWriter adapts an io.Writer to forward each Write call's bytes as
// one InstallProgress chunk on ch, so playwright.Install()'s own
// stdout/stderr streams back to the caller in near-real-time instead of
// the multi-minute download silently blocking with no signal. A nil ch
// (EnsureInstalled called without a progress channel, e.g. via
// ensureStarted's fallback path) discards writes — Install() itself still
// runs and blocks normally either way.
type progressWriter struct {
	ch chan<- InstallProgress
}

func (w *progressWriter) Write(p []byte) (int, error) {
	if w.ch != nil {
		w.ch <- InstallProgress{Data: string(p)}
	}
	return len(p), nil
}

// progressLogHandler adapts a channel to slog.Handler so playwright-go's own
// internal driver-phase logging (d.log(...) in run.go, gated by
// RunOptions.Verbose) also streams back to the caller — without this,
// RunOptions.Stdout/Stderr only capture the install *subprocess*'s output;
// the driver-startup logging that happens via slog would otherwise go to
// slog.Default() and vanish into the void on a Windows service with no
// console. Deliberately minimal: playwright-go's own log lines are plain
// messages with no structured attrs worth preserving field-by-field here.
type progressLogHandler struct {
	ch chan<- InstallProgress
}

func (h *progressLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *progressLogHandler) Handle(_ context.Context, r slog.Record) error {
	if h.ch != nil {
		h.ch <- InstallProgress{Data: r.Message + "\n"}
	}
	return nil
}

func (h *progressLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *progressLogHandler) WithGroup(string) slog.Handler      { return h }

// reapIdleLoop periodically closes sessions that have been idle longer than
// m.idleTimeout, so a forgotten `launch` (no matching `close`) does not hold
// a real Chromium context + page open forever.
func (m *BrowserManager) reapIdleLoop() {
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

// reapOnce closes sessions idle longer than m.idleTimeout. A session
// currently mid-action (busyMu held by acquire()) is skipped for this
// cycle rather than closed out from under it — its lastUsedAt was already
// refreshed when the action started, so it will simply be reconsidered on
// a later tick once the action finishes and the session goes idle for
// real.
func (m *BrowserManager) reapOnce() {
	m.mu.Lock()
	now := time.Now()
	var toClose []struct {
		id string
		s  *browserSession
	}
	for id, s := range m.sessions {
		if now.Sub(s.lastUsedAt) <= m.idleTimeout {
			continue
		}
		if !s.busyMu.TryLock() {
			// Mid-action — leave it in the map, try again next tick.
			continue
		}
		// Claimed busyMu on behalf of the close below; remove from the map
		// now (under the same mu acquisition that decided to reap it) so
		// no new action can start against it while its Close() call (a
		// potentially slow IPC round-trip to Chromium) runs outside mu.
		delete(m.sessions, id)
		toClose = append(toClose, struct {
			id string
			s  *browserSession
		}{id, s})
	}
	m.mu.Unlock()

	for _, e := range toClose {
		m.log.Info("browser: closing idle session", "session_id", e.id,
			"idle_for", now.Sub(e.s.lastUsedAt).Round(time.Second).String())
		_ = e.s.context.Close()
		e.s.busyMu.Unlock()
	}
}

// Shutdown closes every open session and the shared browser + driver. Called
// once during runner shutdown so no Chromium process is left running after
// the runner process exits. Idempotent — safe to call more than once (a
// second call is a no-op) via shutdownOnce, since close(m.stopReaper) would
// otherwise panic on a repeat call.
func (m *BrowserManager) Shutdown() {
	m.shutdownOnce.Do(func() {
		m.mu.Lock()
		m.shuttingDown = true
		close(m.stopReaper)
		sessions := m.sessions
		m.sessions = make(map[string]*browserSession)
		browser := m.browser
		pw := m.pw
		m.browser = nil
		m.pw = nil
		m.mu.Unlock()

		for id, s := range sessions {
			// Wait for any in-flight action to finish before closing —
			// same rationale as closeSession.
			s.busyMu.Lock()
			if err := s.context.Close(); err != nil {
				m.log.Warn("browser: error closing session during shutdown", "session_id", id, "err", err)
			}
			s.busyMu.Unlock()
		}
		if browser != nil {
			_ = browser.Close()
		}
		if pw != nil {
			_ = pw.Stop()
		}
	})
}

// getOrCreateSession returns the existing session for sessionID, or creates
// a new one if it does not exist. Enforces maxSessions on creation.
//
// Deliberately calls ensureStarted() (which itself locks/unlocks mu, and
// may block for a multi-minute install on a cold machine) without mu held
// — see EnsureInstalled's doc comment for why holding mu across an install
// is unsafe. This means two concurrent callers can both pass the "session
// doesn't exist yet" check below and both proceed to call ensureStarted();
// EnsureInstalled's own single-flight logic (m.installing) collapses that
// into one real install, so this is safe, just re-verified after
// ensureStarted returns rather than assumed.
func (m *BrowserManager) getOrCreateSession(sessionID string) (*browserSession, error) {
	m.mu.Lock()
	if s, ok := m.sessions[sessionID]; ok {
		s.lastUsedAt = time.Now()
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	if err := m.ensureStarted(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Re-check: another goroutine may have created this session_id (or hit
	// the cap) while we were outside the lock in ensureStarted().
	if s, ok := m.sessions[sessionID]; ok {
		s.lastUsedAt = time.Now()
		return s, nil
	}

	if len(m.sessions) >= m.maxSessions {
		return nil, fmt.Errorf(
			"browser session limit reached (max_browser_sessions=%d); close an existing session_id before opening another",
			m.maxSessions,
		)
	}

	ctx, err := m.browser.NewContext(playwright.BrowserNewContextOptions{
		UserAgent: playwright.String(stealthUserAgent),
		Viewport: &playwright.Size{
			Width:  1366,
			Height: 768,
		},
		Locale:     playwright.String("en-US"),
		TimezoneId: playwright.String("America/New_York"),
		ExtraHttpHeaders: map[string]string{
			"Sec-Ch-Ua":          `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`,
			"Sec-Ch-Ua-Platform": `"Windows"`,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating browser context: %w", err)
	}

	page, err := ctx.NewPage()
	if err != nil {
		_ = ctx.Close()
		return nil, fmt.Errorf("creating page: %w", err)
	}

	// Injected on the context, not just this first page, so any popup or
	// target=_blank page opened later in this context is stealthed too —
	// AddInitScript on a BrowserContext applies to every page it creates,
	// present and future.
	if err := ctx.AddInitScript(playwright.Script{Content: playwright.String(stealthJS)}); err != nil {
		// Not fatal — the session is still usable, just less stealthy.
		// Logged so a customer machine missing something the stealth
		// injection depends on is diagnosable, rather than silently
		// degrading protection with no trace.
		m.log.Warn("browser: stealth injection failed; continuing without it", "err", err)
	}

	s := &browserSession{context: ctx, page: page, lastUsedAt: time.Now()}
	m.sessions[sessionID] = s
	return s, nil
}

// closeSession closes and removes the session for sessionID, if it exists.
// Returns false if there was no such session (not an error — closing an
// already-closed/nonexistent session is a no-op the caller can treat as
// success). Waits for any in-flight action against this session to finish
// first (via busyMu) rather than closing the context out from under it —
// unlike the idle reaper, an explicit close request should always
// eventually succeed, not be skipped for a cycle.
func (m *BrowserManager) closeSession(sessionID string) bool {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if ok {
		delete(m.sessions, sessionID)
	}
	m.mu.Unlock()

	if !ok {
		return false
	}
	s.busyMu.Lock()
	if err := s.context.Close(); err != nil {
		m.log.Warn("browser: error closing session", "session_id", sessionID, "err", err)
	}
	s.busyMu.Unlock()
	return true
}

// guardScreenshotPath validates a screenshot destination path is inside
// workspaceRoot, mirroring FileOps.guardPath's containment rule — a browser
// command must never be able to write outside the sandboxed workspace any
// more than a file_op command can.
func (m *BrowserManager) guardScreenshotPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", path, err)
	}
	clean := filepath.Clean(abs)
	root := filepath.Clean(m.workspaceRoot)
	if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace root %q", path, m.workspaceRoot)
	}
	return clean, nil
}

// msTimeout converts a whole-seconds timeout (as sent over the wire) to the
// *float64 milliseconds Playwright's generated options expect. Returns nil
// (use Playwright's own default) when secs <= 0.
func msTimeout(secs int) *float64 {
	if secs <= 0 {
		return nil
	}
	ms := float64(secs) * 1000
	return &ms
}

// ── Actions ──────────────────────────────────────────────────────────────
//
// Every action below follows the same shape: getOrCreateSession, then
// s.acquire()/defer s.release() around the actual Playwright call. acquire
// serializes access to one session's Page (see browserSession's doc
// comment) and protects against the idle reaper or an explicit close()
// tearing the context down mid-action.

// allowedGotoSchemes are the only URL schemes Goto will navigate to.
// Browser automation on a runner is intentionally gated behind the same
// allow_shell trust as everything else here (an allow_shell runner can
// already `curl`/`cat` anything a browser could reach), so this is not a
// sandbox boundary — it exists to keep Goto's behavior matching what an
// operator reading "browser automation" would expect (fetch a web page),
// rather than silently also doubling as a local-file/internal-metadata
// reader via `goto file:///...` or `goto http://169.254.169.254/...` — the
// latter is still reachable via http(s) to a loopback/link-local address,
// which is unavoidable without breaking legitimate localhost testing use
// cases, but file:/data:/chrome: access is not the browser tool's purpose
// and is blocked outright.
var allowedGotoSchemes = map[string]bool{"http": true, "https": true}

func validateGotoURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid url %q: %w", rawURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if !allowedGotoSchemes[scheme] {
		return fmt.Errorf("url scheme %q is not allowed for goto; only http/https are permitted", scheme)
	}
	return nil
}

// Goto navigates the session's page to url. Only http/https URLs are
// accepted — see allowedGotoSchemes.
func (m *BrowserManager) Goto(sessionID, rawURL string, timeoutSecs int) error {
	if err := validateGotoURL(rawURL); err != nil {
		return err
	}
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	s.acquire()
	defer s.release()
	_, err = s.page.Goto(rawURL, playwright.PageGotoOptions{Timeout: msTimeout(timeoutSecs)})
	return err
}

// Click clicks the first element matching selector.
func (m *BrowserManager) Click(sessionID, selector string, timeoutSecs int) error {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	s.acquire()
	defer s.release()
	return s.page.Click(selector, playwright.PageClickOptions{Timeout: msTimeout(timeoutSecs)})
}

// Fill sets the value of the first element matching selector.
func (m *BrowserManager) Fill(sessionID, selector, value string, timeoutSecs int) error {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	s.acquire()
	defer s.release()
	return s.page.Fill(selector, value, playwright.PageFillOptions{Timeout: msTimeout(timeoutSecs)})
}

// WaitForSelector waits until selector matches the given state (defaults to
// Playwright's own default, "visible", when state is empty).
func (m *BrowserManager) WaitForSelector(sessionID, selector, state string, timeoutSecs int) error {
	opts := playwright.PageWaitForSelectorOptions{Timeout: msTimeout(timeoutSecs)}
	switch state {
	case "attached":
		opts.State = playwright.WaitForSelectorStateAttached
	case "detached":
		opts.State = playwright.WaitForSelectorStateDetached
	case "hidden":
		opts.State = playwright.WaitForSelectorStateHidden
	case "visible", "":
		opts.State = playwright.WaitForSelectorStateVisible
	default:
		return fmt.Errorf("unknown wait state %q: must be attached, detached, hidden, or visible", state)
	}
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	s.acquire()
	defer s.release()
	_, err = s.page.WaitForSelector(selector, opts)
	return err
}

// Screenshot captures the session's current page and writes it to path
// (validated to be inside workspaceRoot). fullPage controls whether the
// entire scrollable page is captured vs just the current viewport. Path is
// validated before touching the session, so a rejected path never launches
// Chromium or consumes a session slot for a command that was always going
// to fail.
func (m *BrowserManager) Screenshot(sessionID, path string, fullPage bool) error {
	clean, err := m.guardScreenshotPath(path)
	if err != nil {
		return err
	}
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	s.acquire()
	defer s.release()
	_, err = s.page.Screenshot(playwright.PageScreenshotOptions{
		Path:     playwright.String(clean),
		FullPage: playwright.Bool(fullPage),
	})
	return err
}

// GetText returns the page's rendered text (Content mode) or the innerText
// of a single selector match (selector mode, when selector is non-empty).
func (m *BrowserManager) GetText(sessionID, selector string) (string, error) {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return "", err
	}
	s.acquire()
	defer s.release()
	if selector != "" {
		return s.page.TextContent(selector)
	}
	return s.page.InnerText("body")
}

// GetContent returns the full HTML content of the session's current page.
func (m *BrowserManager) GetContent(sessionID string) (string, error) {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return "", err
	}
	s.acquire()
	defer s.release()
	return s.page.Content()
}

// Evaluate runs a JavaScript expression in the page context and returns the
// raw result value (page.Evaluate returns interface{} — JSON-marshaling it
// to a string, when the caller needs one over the wire, is runner.go's
// handleBrowser's job, not this method's).
func (m *BrowserManager) Evaluate(sessionID, expression string) (interface{}, error) {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return nil, err
	}
	s.acquire()
	defer s.release()
	return s.page.Evaluate(expression)
}

// Close closes and removes the given session. Idempotent: closing an
// already-closed or nonexistent session_id is not an error. Waits for any
// in-flight action on this session to finish first — see closeSession.
func (m *BrowserManager) Close(sessionID string) {
	m.closeSession(sessionID)
}

// Launch ensures a session exists for sessionID without performing any
// other action — the explicit "launch" action lets a caller open a session
// before its first real navigation, e.g. to set cookies via other actions
// first. Returns an error only if the browser itself fails to start or the
// session pool is full.
func (m *BrowserManager) Launch(sessionID string) error {
	_, err := m.getOrCreateSession(sessionID)
	return err
}
