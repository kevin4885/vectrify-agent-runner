// Package executor: browser.go implements Playwright-driven browser
// automation ("browser" command type) — navigate, click, fill, screenshot,
// extract text/HTML, evaluate JS, wait-for-selector — against a small pool
// of stateful sessions that persist across multiple commands until closed
// or reaped for inactivity.
//
// Gated behind config.AllowBrowser (mirrors AllowShell's gating pattern —
// see runner/runner.go handleBrowser). Requires the Playwright driver +
// Chromium binaries to already be installed on this machine; see
// `vectrify-runner -install-browsers` in main.go.
//
// Stealth: every launched context disables the Blink automation-controlled
// flag, uses a realistic UA/viewport/locale/timezone tuple, and injects the
// evasion script embedded in github.com/jonfriesen/playwright-go-stealth
// into every new page. Note: we deliberately use only that package's
// embedded stealth.StealthJS string constant via our own AddInitScript call
// rather than its stealth.Inject(page) helper — that helper's signature is
// pinned to the older github.com/playwright-community/playwright-go module
// path, which is a different Go type identity than the
// github.com/mxschmitt/playwright-go module this file uses (same upstream
// project, renamed on GitHub; the newer module path is required here
// because the driver version pinned by the old path's latest release
// (v0.4201.1) points at Playwright driver binaries no longer hosted —
// `playwright.Install()` 404s against them). This raises the bar against
// basic/medium bot detection but is NOT a guarantee against advanced
// systems (Cloudflare Turnstile with behavioral scoring, Akamai,
// PerimeterX/DataDome) — see CLAUDE.md.
package executor

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mxschmitt/playwright-go"
	stealth "github.com/jonfriesen/playwright-go-stealth"
)

// stealthUserAgent is a realistic, current desktop Chrome UA string used for
// every launched context instead of Playwright's default headless UA
// (which itself is a well-known bot-detection signal). Paired with the
// matching Sec-Ch-Ua headers below and the stealth init script.
const stealthUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// browserSession holds one open Playwright browser context + page, kept
// alive across multiple "browser" commands until explicitly closed or
// reaped for inactivity.
type browserSession struct {
	context    playwright.BrowserContext
	page       playwright.Page
	lastUsedAt time.Time
}

// BrowserManager owns a single shared Playwright + Browser process and a
// bounded pool of sessions keyed by an API-supplied session_id. One
// BrowserManager is created per Runner (see runner.New) and lives for the
// process lifetime.
type BrowserManager struct {
	workspaceRoot  string
	log            *slog.Logger
	maxSessions    int
	idleTimeout    time.Duration
	headless       bool

	mu         sync.Mutex
	pw         *playwright.Playwright
	browser    playwright.Browser
	sessions   map[string]*browserSession
	reaperOnce sync.Once
	stopReaper chan struct{}
}

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
// once. Must be called with mu held.
func (m *BrowserManager) ensureStarted() error {
	if m.browser != nil {
		return nil
	}
	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf(
			"starting playwright driver: %w (has `vectrify-runner -install-browsers` been run on this machine?)", err,
		)
	}
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
	m.pw = pw
	m.browser = browser
	m.reaperOnce.Do(func() { go m.reapIdleLoop() })
	return nil
}

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

func (m *BrowserManager) reapOnce() {
	m.mu.Lock()
	var toClose []struct {
		id string
		s  *browserSession
	}
	now := time.Now()
	for id, s := range m.sessions {
		if now.Sub(s.lastUsedAt) > m.idleTimeout {
			toClose = append(toClose, struct {
				id string
				s  *browserSession
			}{id, s})
		}
	}
	for _, e := range toClose {
		delete(m.sessions, e.id)
	}
	m.mu.Unlock()

	for _, e := range toClose {
		m.log.Info("browser: closing idle session", "session_id", e.id,
			"idle_for", now.Sub(e.s.lastUsedAt).Round(time.Second).String())
		_ = e.s.context.Close()
	}
}

// Shutdown closes every open session and the shared browser + driver. Called
// once during runner shutdown so no Chromium process is left running after
// the runner process exits.
func (m *BrowserManager) Shutdown() {
	close(m.stopReaper)
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = make(map[string]*browserSession)
	browser := m.browser
	pw := m.pw
	m.browser = nil
	m.pw = nil
	m.mu.Unlock()

	for id, s := range sessions {
		if err := s.context.Close(); err != nil {
			m.log.Warn("browser: error closing session during shutdown", "session_id", id, "err", err)
		}
	}
	if browser != nil {
		_ = browser.Close()
	}
	if pw != nil {
		_ = pw.Stop()
	}
}

// getOrCreateSession returns the existing session for sessionID, or creates
// a new one if it does not exist. Enforces maxSessions on creation.
func (m *BrowserManager) getOrCreateSession(sessionID string) (*browserSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[sessionID]; ok {
		s.lastUsedAt = time.Now()
		return s, nil
	}

	if err := m.ensureStarted(); err != nil {
		return nil, err
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

	if err := page.AddInitScript(playwright.Script{Content: playwright.String(stealth.StealthJS)}); err != nil {
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
// success).
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
	if err := s.context.Close(); err != nil {
		m.log.Warn("browser: error closing session", "session_id", sessionID, "err", err)
	}
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

// Goto navigates the session's page to url.
func (m *BrowserManager) Goto(sessionID, url string, timeoutSecs int) error {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	_, err = s.page.Goto(url, playwright.PageGotoOptions{Timeout: msTimeout(timeoutSecs)})
	return err
}

// Click clicks the first element matching selector.
func (m *BrowserManager) Click(sessionID, selector string, timeoutSecs int) error {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	return s.page.Click(selector, playwright.PageClickOptions{Timeout: msTimeout(timeoutSecs)})
}

// Fill sets the value of the first element matching selector.
func (m *BrowserManager) Fill(sessionID, selector, value string, timeoutSecs int) error {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	return s.page.Fill(selector, value, playwright.PageFillOptions{Timeout: msTimeout(timeoutSecs)})
}

// WaitForSelector waits until selector matches the given state (defaults to
// Playwright's own default, "visible", when state is empty).
func (m *BrowserManager) WaitForSelector(sessionID, selector, state string, timeoutSecs int) error {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
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
	_, err = s.page.WaitForSelector(selector, opts)
	return err
}

// Screenshot captures the session's current page and writes it to path
// (validated to be inside workspaceRoot). fullPage controls whether the
// entire scrollable page is captured vs just the current viewport.
func (m *BrowserManager) Screenshot(sessionID, path string, fullPage bool) error {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return err
	}
	clean, err := m.guardScreenshotPath(path)
	if err != nil {
		return err
	}
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
	return s.page.Content()
}

// Evaluate runs a JavaScript expression in the page context and returns the
// result JSON-marshaled to a string (page.Evaluate returns interface{};
// callers over the wire need a string, not a Go value).
func (m *BrowserManager) Evaluate(sessionID, expression string) (interface{}, error) {
	s, err := m.getOrCreateSession(sessionID)
	if err != nil {
		return nil, err
	}
	return s.page.Evaluate(expression)
}

// Close closes and removes the given session. Idempotent: closing an
// already-closed or nonexistent session_id is not an error.
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
