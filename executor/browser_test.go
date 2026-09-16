package executor

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
