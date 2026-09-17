package runner

import (
	"log/slog"
	"testing"
	"time"

	"vectrify/agent-runner/config"
	"vectrify/agent-runner/protocol"
)

// discardWriter is an io.Writer that drops everything -- used to keep test
// output quiet, matching executor/browser_test.go's own helper.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestRunner(t *testing.T, allowShell bool) *Runner {
	t.Helper()
	cfg := &config.Config{
		WorkspaceRoot:             t.TempDir(),
		AllowShell:                allowShell,
		MaxBrowserSessions:        3,
		BrowserIdleTimeoutSeconds: 300,
	}
	log := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	r := New(cfg, log)
	t.Cleanup(r.Shutdown)
	return r
}

// collectSends records every message passed to send() in order, so tests
// can assert on the exact ResultMsg/StreamMsg sequence a handler produced.
type collectSends struct {
	msgs []interface{}
}

func (c *collectSends) send(msg interface{}) {
	c.msgs = append(c.msgs, msg)
}

func (c *collectSends) lastResult() (protocol.ResultMsg, bool) {
	for i := len(c.msgs) - 1; i >= 0; i-- {
		if r, ok := c.msgs[i].(protocol.ResultMsg); ok {
			return r, true
		}
	}
	return protocol.ResultMsg{}, false
}

// TestHandleBrowser_RejectsWhenShellDisabled verifies browser commands are
// gated on AllowShell, with no fall-through to the browser manager at all --
// there is deliberately no separate allow_browser setting.
func TestHandleBrowser_RejectsWhenShellDisabled(t *testing.T) {
	r := newTestRunner(t, false)
	c := &collectSends{}

	r.handleBrowser("cmd1", protocol.RawCommand{"action": "launch", "session_id": "s1"}, c.send)

	result, ok := c.lastResult()
	if !ok {
		t.Fatal("expected a ResultMsg, got none")
	}
	if result.OK {
		t.Errorf("expected OK=false when allow_shell is false, got OK=true")
	}
	if !containsSubstring(result.Error, "allow_shell") {
		t.Errorf("expected error to mention allow_shell, got %q", result.Error)
	}

	// Confirm nothing else was sent -- rejected before EnsureInstalled/any
	// StreamMsg, i.e. no ~300MB install was even attempted for a runner
	// that isn't allowed to use it.
	if len(c.msgs) != 1 {
		t.Errorf("expected exactly 1 message (the rejection) sent, got %d: %+v", len(c.msgs), c.msgs)
	}
}

// TestHandleBrowser_MissingSessionID verifies a payload with no session_id
// is rejected with a clear error rather than passed through to the browser
// manager with an empty session_id.
func TestHandleBrowser_MissingSessionID(t *testing.T) {
	r := newTestRunner(t, true)
	c := &collectSends{}

	r.handleBrowser("cmd1", protocol.RawCommand{"action": "launch"}, c.send)

	result, ok := c.lastResult()
	if !ok {
		t.Fatal("expected a ResultMsg, got none")
	}
	if result.OK {
		t.Errorf("expected OK=false for a missing session_id, got OK=true")
	}
	if !containsSubstring(result.Error, "session_id") {
		t.Errorf("expected error to mention session_id, got %q", result.Error)
	}
}

// TestHandleBrowser_TimeoutClamp verifies an oversized timeout_seconds is
// silently clamped to maxShellTimeout rather than passed straight through
// to the browser action -- guards against a payload like
// {"timeout_seconds": 999999} parking a heavy dispatch slot for ~11 days.
// Exercised indirectly: goto with a huge timeout against a real (but
// immediately-failing, since the URL doesn't resolve to anything real
// quickly) target would take too long to assert on directly, so this
// instead verifies the clamp mutates raw["timeout_seconds"] before it
// reaches the action switch, which is the actual mechanism under test.
func TestHandleBrowser_TimeoutClamp(t *testing.T) {
	r := newTestRunner(t, true)

	raw := protocol.RawCommand{
		"action":          "goto",
		"session_id":      "s1",
		"url":             "http://127.0.0.1:1", // deliberately unroutable port, fails fast
		"timeout_seconds": 999999,
	}

	// Call the same clamp logic handleBrowser runs, isolated from the
	// install/dispatch machinery, by invoking handleBrowser and then
	// checking what ended up in raw after it returns. handleBrowser
	// mutates the map in place before dispatching to the action switch.
	c := &collectSends{}
	done := make(chan struct{})
	go func() {
		r.handleBrowser("cmd1", raw, c.send)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("handleBrowser did not return within 30s -- the timeout clamp may not be applied before the action runs")
	}

	got, ok := raw["timeout_seconds"].(int)
	if !ok {
		t.Fatalf("timeout_seconds is no longer an int after handleBrowser, got %T: %v", raw["timeout_seconds"], raw["timeout_seconds"])
	}
	if time.Duration(got)*time.Second > maxShellTimeout {
		t.Errorf("timeout_seconds = %d seconds, want clamped to <= %v", got, maxShellTimeout)
	}
	if got != int(maxShellTimeout.Seconds()) {
		t.Errorf("timeout_seconds = %d, want exactly %d (maxShellTimeout)", got, int(maxShellTimeout.Seconds()))
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

// TestHandleBrowser_EvaluateReadsScriptField verifies the "evaluate" action
// reads the "script" payload field -- this must match the field name the
// vectrify-api runner_browser tool actually sends (see
// app/engine/tools/runner/runner_browser.py's send_command call, which
// passes script=...). An earlier version of this handler read "expression"
// instead, a naming mismatch that would have made every evaluate call fail
// with "expression is required for evaluate" despite the caller having
// supplied a script.
func TestHandleBrowser_EvaluateReadsScriptField(t *testing.T) {
	r := newTestRunner(t, true)
	c := &collectSends{}

	// "script" with no real navigation target will fail deeper in
	// (creating a session/page still requires the browser to actually
	// start), but the point of this test is specifically that we get past
	// the "script is required" / "expression is required" validation --
	// i.e. the field name itself is accepted. Use an empty script value to
	// isolate exactly that check without needing a real page.
	r.handleBrowser("cmd1", protocol.RawCommand{
		"action":     "evaluate",
		"session_id": "s1",
		"script":     "1 + 1",
	}, c.send)

	result, ok := c.lastResult()
	if !ok {
		t.Fatal("expected a ResultMsg, got none")
	}
	if containsSubstring(result.Error, "expression is required") {
		t.Fatalf("handleBrowser still validates the old 'expression' field name instead of 'script': %q", result.Error)
	}
	if containsSubstring(result.Error, "script is required") {
		t.Fatalf("handleBrowser rejected a script that was actually supplied -- field name mismatch: %q", result.Error)
	}
}
