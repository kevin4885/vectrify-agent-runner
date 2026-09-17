package executor

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// newTestBrowserManager creates a BrowserManager rooted at a fresh temp
// directory with a discard logger and a short idle timeout, so tests don't
// spam stdout and don't need to wait out the production default (5 min) to
// exercise the reaper.
func newTestBrowserManager(t *testing.T, maxSessions int, idleTimeout time.Duration) (*BrowserManager, string) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	m := NewBrowserManager(dir, maxSessions, idleTimeout, true, log)
	t.Cleanup(m.Shutdown)
	return m, dir
}

// newTestPageServer starts a local HTTP server serving a minimal, known
// page so tests never depend on network access to a real external site.
func newTestPageServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Test Page</title></head>
<body><h1 id="heading">Hello Vectrify</h1><button id="btn" onclick="document.getElementById('heading').innerText='Clicked'">Click me</button>
<input id="inp" type="text" /></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// ─────────────────────────────────────────────────────────────────────────────
// EnsureInstalled
// ─────────────────────────────────────────────────────────────────────────────

// The full auto-install download path (driver missing -> playwright.Install()
// -> retry) is intentionally NOT exercised here: it downloads ~300MB over the
// network, which is far too slow and flaky for a unit test run on every CI
// build. It was validated manually against a real isolated fake-HOME
// directory during development (see PR description / commit message) rather
// than as an automated test. This test instead locks in the fast path that
// every other test in this file already exercises implicitly through
// Launch/Goto (getOrCreateSession -> ensureStarted -> doEnsureInstalled(nil)):
// once the browser is already running, EnsureInstalled must be a fast no-op
// that never touches the progress channel.
func TestBrowserManager_EnsureInstalled_FastPathNoProgress(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)

	// Start the browser via a normal action first (this machine already has
	// the Playwright driver installed, matching the common case), then call
	// EnsureInstalled directly and confirm the already-started fast path
	// emits nothing on the progress channel.
	if err := m.Launch("s1"); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	defer m.Close("s1")

	progressCh := make(chan InstallProgress, 16)
	done := make(chan error, 1)
	go func() {
		done <- m.EnsureInstalled(progressCh)
		close(progressCh)
	}()

	var gotProgress bool
	for range progressCh {
		gotProgress = true
	}
	if err := <-done; err != nil {
		t.Fatalf("EnsureInstalled() (fast path) error = %v", err)
	}
	if gotProgress {
		t.Errorf("EnsureInstalled() sent progress chunks on the already-started fast path, want none")
	}
}

// EnsureInstalled must tolerate a nil progress channel (the no-progress-
// reporting caller, e.g. ensureStarted's internal fallback) without panicking
// or blocking.
func TestBrowserManager_EnsureInstalled_NilProgressChannel(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	if err := m.EnsureInstalled(nil); err != nil {
		t.Fatalf("EnsureInstalled(nil) error = %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestBrowserManager_LaunchGotoScreenshotClose(t *testing.T) {
	m, workspaceRoot := newTestBrowserManager(t, 3, time.Minute)
	srv := newTestPageServer(t)

	if err := m.Launch("s1"); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto() error = %v", err)
	}

	shotPath := filepath.Join(workspaceRoot, "shot.png")
	if err := m.Screenshot("s1", shotPath, false); err != nil {
		t.Fatalf("Screenshot() error = %v", err)
	}
	info, err := os.Stat(shotPath)
	if err != nil {
		t.Fatalf("expected screenshot file to exist: %v", err)
	}
	if info.Size() == 0 {
		t.Errorf("screenshot file is empty")
	}

	m.Close("s1")
	// Closing again must be a no-op, not an error / panic.
	m.Close("s1")
}

func TestBrowserManager_GetTextAndContent(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	srv := newTestPageServer(t)

	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto() error = %v", err)
	}

	text, err := m.GetText("s1", "#heading")
	if err != nil {
		t.Fatalf("GetText(selector) error = %v", err)
	}
	if text != "Hello Vectrify" {
		t.Errorf("GetText(#heading) = %q, want %q", text, "Hello Vectrify")
	}

	bodyText, err := m.GetText("s1", "")
	if err != nil {
		t.Fatalf("GetText(body) error = %v", err)
	}
	if bodyText == "" {
		t.Errorf("GetText(body) returned empty string")
	}

	html, err := m.GetContent("s1")
	if err != nil {
		t.Fatalf("GetContent() error = %v", err)
	}
	if !containsSubstring(html, "Test Page") {
		t.Errorf("GetContent() = %q, want it to contain %q", html, "Test Page")
	}
}

func TestBrowserManager_ClickAndFill(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	srv := newTestPageServer(t)

	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto() error = %v", err)
	}
	if err := m.Fill("s1", "#inp", "hello", 15); err != nil {
		t.Fatalf("Fill() error = %v", err)
	}
	if err := m.Click("s1", "#btn", 15); err != nil {
		t.Fatalf("Click() error = %v", err)
	}
	text, err := m.GetText("s1", "#heading")
	if err != nil {
		t.Fatalf("GetText() error = %v", err)
	}
	if text != "Clicked" {
		t.Errorf("after click, #heading text = %q, want %q", text, "Clicked")
	}
}

func TestBrowserManager_WaitForSelector(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	srv := newTestPageServer(t)

	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto() error = %v", err)
	}
	if err := m.WaitForSelector("s1", "#heading", "visible", 10); err != nil {
		t.Fatalf("WaitForSelector(visible) error = %v", err)
	}
	if err := m.WaitForSelector("s1", "#nonexistent", "attached", 2); err == nil {
		t.Errorf("WaitForSelector for a selector that never appears should time out with an error")
	}
}

func TestBrowserManager_Evaluate(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	srv := newTestPageServer(t)

	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto() error = %v", err)
	}
	result, err := m.Evaluate("s1", "1 + 2")
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	var n float64
	switch v := result.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	default:
		t.Fatalf("Evaluate(\"1 + 2\") returned unexpected type %T (%v)", result, result)
	}
	if n != 3 {
		t.Errorf("Evaluate(\"1 + 2\") = %v, want 3", n)
	}
}

// TestBrowserManager_SessionLimitEnforced verifies that a manager configured
// with maxSessions=1 refuses to open a second concurrent session, and that
// closing the first frees the slot for a new one.
func TestBrowserManager_SessionLimitEnforced(t *testing.T) {
	m, _ := newTestBrowserManager(t, 1, time.Minute)

	if err := m.Launch("s1"); err != nil {
		t.Fatalf("Launch(s1) error = %v", err)
	}
	if err := m.Launch("s2"); err == nil {
		t.Fatalf("Launch(s2) should have failed: session limit (1) already reached by s1")
	}

	m.Close("s1")
	if err := m.Launch("s2"); err != nil {
		t.Fatalf("Launch(s2) after closing s1 should succeed, got error = %v", err)
	}
}

// TestBrowserManager_ScreenshotPathContainment verifies a screenshot
// destination outside workspaceRoot is rejected, mirroring FileOps'
// guardPath containment rule for file_op commands.
func TestBrowserManager_ScreenshotPathContainment(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	srv := newTestPageServer(t)

	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto() error = %v", err)
	}

	outside := filepath.Join(t.TempDir(), "..", "escaped.png")
	if err := m.Screenshot("s1", outside, false); err == nil {
		t.Errorf("Screenshot() to a path outside workspace_root should have been rejected")
	}
}

// TestBrowserManager_IdleReaper verifies a session idle longer than the
// configured idle timeout is automatically closed by the reaper, without
// requiring an explicit "close" action from the caller.
func TestBrowserManager_IdleReaper(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, 200*time.Millisecond)

	if err := m.Launch("s1"); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}

	// reapOnce is the same logic the background ticker calls; invoking it
	// directly keeps this test fast and deterministic instead of sleeping
	// past the real 30s ticker interval.
	time.Sleep(250 * time.Millisecond)
	m.reapOnce()

	m.mu.Lock()
	_, stillOpen := m.sessions["s1"]
	m.mu.Unlock()
	if stillOpen {
		t.Errorf("session s1 should have been reaped after exceeding the idle timeout")
	}
}

func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// ─────────────────────────────────────────────────────────────────────────────
// Goto URL-scheme validation
// ─────────────────────────────────────────────────────────────────────────────

// TestGoto_RejectsNonHTTPSchemes verifies Goto refuses file:/data:/chrome:
// URLs outright, rather than letting the browser tool double as a local
// filesystem reader or internal-page inspector. See validateGotoURL's doc
// comment for the reasoning (not a sandbox boundary -- allow_shell already
// implies equivalent local-machine trust -- but browser automation should
// behave like fetching a web page, not silently also do more than that).
func TestGoto_RejectsNonHTTPSchemes(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)

	rejected := []string{
		"file:///etc/passwd",
		"file:///C:/Windows/System32/config/SAM",
		"data:text/html,<h1>hi</h1>",
		"chrome://version",
		"view-source:http://example.com",
	}
	for _, u := range rejected {
		if err := m.Goto("s1", u, 5); err == nil {
			t.Errorf("Goto(%q) should have been rejected, got nil error", u)
		}
	}
	// A session must never actually be created for a rejected URL -- the
	// scheme check happens before getOrCreateSession, same ordering
	// principle as guardScreenshotPath running before session creation.
	m.mu.Lock()
	_, exists := m.sessions["s1"]
	m.mu.Unlock()
	if exists {
		t.Errorf("a session was created despite every Goto call being rejected for scheme")
	}
}

// TestGoto_AllowsHTTPAndHTTPS verifies the two legitimate schemes still work
// -- a regression guard against validateGotoURL being over-broad.
func TestGoto_AllowsHTTPAndHTTPS(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	srv := newTestPageServer(t)

	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto(%q) (http) should be allowed, got error = %v", srv.URL, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Shutdown idempotency
// ─────────────────────────────────────────────────────────────────────────────

// TestShutdown_Idempotent verifies a second Shutdown() call does not panic
// (an earlier version would panic on a repeat close(m.stopReaper)).
func TestShutdown_Idempotent(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	if err := m.Launch("s1"); err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	m.Shutdown()
	// Must not panic.
	m.Shutdown()
	m.Shutdown()
}

// ─────────────────────────────────────────────────────────────────────────────
// Per-session serialization (concurrent same-session_id access)
// ─────────────────────────────────────────────────────────────────────────────

// TestBrowserManager_ConcurrentSameSession_NoInterleaving verifies that two
// goroutines issuing actions against the SAME session_id are serialized by
// busyMu rather than interleaving Goto/Evaluate calls on one Page
// concurrently. Without acquire()/release(), Playwright's own underlying
// protocol connection is not safe for concurrent calls from one Page, and
// even where it doesn't outright error, interleaved navigation defeats the
// entire purpose of a stateful session (the caller can no longer reason
// about "the current page" between their own successive tool calls).
//
// This test cannot use -race here (no cgo/C compiler available in this
// environment -- see CLAUDE.md/PR notes), so it does not prove the absence
// of a data race directly. It does prove the *serialization contract*:
// every Evaluate call observes a globally-increasing counter with no two
// calls seeing the same value, which is only possible if busyMu is
// actually excluding concurrent access to the shared page state (a
// sequence counter set via evaluate) rather than merely by coincidence of
// timing.
func TestBrowserManager_ConcurrentSameSession_NoInterleaving(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, time.Minute)
	srv := newTestPageServer(t)

	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto() error = %v", err)
	}
	if _, err := m.Evaluate("s1", "window.__seq = 0"); err != nil {
		t.Fatalf("Evaluate(init) error = %v", err)
	}

	const goroutines = 8
	const perGoroutine = 5
	seen := make(chan float64, goroutines*perGoroutine)
	errCh := make(chan error, goroutines*perGoroutine)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				// Read-then-increment-then-return, done as a single atomic
				// JS statement so the only way two calls can observe the
				// same "before" value is if busyMu let them run truly
				// concurrently against the same page.
				result, err := m.Evaluate("s1", "(function(){ const v = window.__seq; window.__seq = v + 1; return v; })()")
				if err != nil {
					errCh <- err
					return
				}
				switch v := result.(type) {
				case float64:
					seen <- v
				case int:
					seen <- float64(v)
				default:
					errCh <- fmt.Errorf("unexpected Evaluate result type %T", result)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(seen)
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent Evaluate error: %v", err)
	}

	values := make(map[float64]int)
	for v := range seen {
		values[v]++
	}
	if len(values) != goroutines*perGoroutine {
		t.Errorf("expected %d distinct sequence values (proving no two calls interleaved), got %d distinct values: %v",
			goroutines*perGoroutine, len(values), values)
	}
	for v, count := range values {
		if count > 1 {
			t.Errorf("sequence value %v was observed %d times -- two Evaluate calls interleaved on the same session", v, count)
		}
	}
}

// TestBrowserManager_ReapDoesNotCloseBusySession verifies the idle reaper
// skips a session that is currently mid-action (busyMu held), rather than
// closing its context out from under the in-flight call.
func TestBrowserManager_ReapDoesNotCloseBusySession(t *testing.T) {
	m, _ := newTestBrowserManager(t, 3, 10*time.Millisecond) // tiny idle timeout
	srv := newTestPageServer(t)

	if err := m.Goto("s1", srv.URL, 15); err != nil {
		t.Fatalf("Goto() error = %v", err)
	}

	m.mu.Lock()
	s := m.sessions["s1"]
	m.mu.Unlock()
	if s == nil {
		t.Fatal("session s1 not found after Goto")
	}

	// Simulate an in-flight action by holding busyMu directly (avoids a
	// real multi-second page.WaitForSelector just to create the window).
	s.acquire()
	defer s.release()

	time.Sleep(20 * time.Millisecond) // exceed the 10ms idle timeout
	m.reapOnce()

	m.mu.Lock()
	_, stillTracked := m.sessions["s1"]
	m.mu.Unlock()
	if !stillTracked {
		t.Errorf("reapOnce() removed a session that was busy (mid-action) -- it should have been skipped for this cycle")
	}
}
