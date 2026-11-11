package executor

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// isolateProcessLogDir points os.TempDir() at a fresh directory for the test
// so processLogDir() is private to it, and returns that log directory.
func isolateProcessLogDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("TMPDIR", tmp)
	return processLogDir()
}

// logFilesIn returns the names of the "*.log" files directly in dir.
func logFilesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), processLogSuffix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func logPathOf(t *testing.T, m *ProcessManager, id string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	mp, ok := m.processes[id]
	if !ok {
		t.Fatalf("process %q not tracked", id)
	}
	return mp.logPath
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// recordDiscarded installs processDiscardedHook for the test and returns a
// func that yields the commands discardUnregistered disposed of.
func recordDiscarded(t *testing.T) func() []*exec.Cmd {
	t.Helper()
	var mu sync.Mutex
	var cmds []*exec.Cmd
	processDiscardedHook = func(c *exec.Cmd) {
		mu.Lock()
		cmds = append(cmds, c)
		mu.Unlock()
	}
	t.Cleanup(func() { processDiscardedHook = nil })
	return func() []*exec.Cmd {
		mu.Lock()
		defer mu.Unlock()
		return append([]*exec.Cmd(nil), cmds...)
	}
}

// holdStartsAtBarrier makes the first n Starts that reach the registration
// point wait there until all n have arrived, so n launched processes race for
// the pool at the same instant. The hook is removed at test end.
func holdStartsAtBarrier(t *testing.T, n int) {
	t.Helper()
	var mu sync.Mutex
	arrived := 0
	release := make(chan struct{})
	processBeforeRegisterHook = func() {
		mu.Lock()
		arrived++
		if arrived == n {
			close(release)
		}
		mu.Unlock()
		select {
		case <-release:
		case <-time.After(30 * time.Second):
		}
	}
	t.Cleanup(func() { processBeforeRegisterHook = nil })
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

// assertReaped fails unless c has been killed AND waited on: ProcessState is
// only populated once Wait has completed, which requires the process to have
// exited; and signalling the released process handle must then fail
// (os.ErrProcessDone on Unix, "invalid argument" on Windows).
func assertReaped(t *testing.T, c *exec.Cmd) {
	t.Helper()
	if c.ProcessState == nil {
		t.Errorf("pid %d: cmd.Wait() was never completed (leaked child)", c.Process.Pid)
		return
	}
	if err := c.Process.Signal(os.Kill); err == nil {
		t.Errorf("pid %d still signalable after discard", c.Process.Pid)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// readLogTail / Logs
// ─────────────────────────────────────────────────────────────────────────────

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.log")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadLogTail_LargeFileReturnsOnlyTailWindow(t *testing.T) {
	var sb strings.Builder
	for i := 0; sb.Len() < 3*1000; i++ {
		sb.WriteString("line-")
		sb.WriteString(itoa(i))
		sb.WriteString("\n")
	}
	content := sb.String()
	p := writeTemp(t, []byte(content))

	data, clipped, err := readLogTail(p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !clipped {
		t.Errorf("clipped = false for a file 3x the window")
	}
	if len(data) > 1000 {
		t.Errorf("len(data) = %d, want <= 1000 (must not read the whole file)", len(data))
	}
	if !strings.HasSuffix(content, string(data)) || len(data) < 990 {
		t.Errorf("data is not the real tail of the file (len %d)", len(data))
	}
}

func TestReadLogTail_Utf8Boundary(t *testing.T) {
	// "€" is 3 bytes (E2 82 AC); every window size from 1..len lands the
	// window start on each of the three byte positions of a rune.
	content := strings.Repeat("€", 10)
	p := writeTemp(t, []byte(content))
	for max := int64(1); max < int64(len(content)); max++ {
		data, clipped, err := readLogTail(p, max)
		if err != nil {
			t.Fatal(err)
		}
		if !clipped {
			t.Errorf("max=%d: clipped = false", max)
		}
		if int64(len(data)) > max {
			t.Errorf("max=%d: len(data) = %d exceeds window", max, len(data))
		}
		if !utf8.Valid(data) {
			t.Errorf("max=%d: result is not valid UTF-8: % x", max, data)
		}
		if len(data) > 0 && !utf8.RuneStart(data[0]) {
			t.Errorf("max=%d: result starts with a continuation byte: % x", max, data)
		}
		if !strings.HasSuffix(content, string(data)) {
			t.Errorf("max=%d: result is not a suffix of the file", max)
		}
	}
}

func TestReadLogTail_SmallAndEmptyFilesUnchanged(t *testing.T) {
	content := "héllo €\nwörld\n"
	data, clipped, err := readLogTail(writeTemp(t, []byte(content)), 1000)
	if err != nil || clipped || string(data) != content {
		t.Errorf("small file: data=%q clipped=%v err=%v, want unchanged", data, clipped, err)
	}
	// Exactly window-sized: not clipped.
	data, clipped, err = readLogTail(writeTemp(t, []byte(content)), int64(len(content)))
	if err != nil || clipped || string(data) != content {
		t.Errorf("exact-size file: data=%q clipped=%v err=%v, want unchanged", data, clipped, err)
	}
	data, clipped, err = readLogTail(writeTemp(t, nil), 1000)
	if err != nil || clipped || len(data) != 0 {
		t.Errorf("empty file: data=%q clipped=%v err=%v, want empty", data, clipped, err)
	}
}

func TestReadLogTail_MissingFileErrors(t *testing.T) {
	if _, _, err := readLogTail(filepath.Join(t.TempDir(), "nope.log"), 10); err == nil {
		t.Errorf("expected an error for a missing file")
	}
}

func TestProcessManager_LogsClipsHugeLogAtUtf8Boundary(t *testing.T) {
	isolateProcessLogDir(t)
	m, _ := newTestProcessManager(t, 5, time.Hour)
	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 3x the cap of 3-byte runes, ending in a 4-byte marker so the tail window
	// (size-maxLogBytes = 100004, not a multiple of 3) starts mid-rune.
	body := strings.Repeat("€", 3*maxLogBytes/3) + "END\n"
	f, err := os.OpenFile(logPathOf(t, m, "p1"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	f.Close()

	out, err := m.Logs("p1", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	const marker = "<earlier output clipped>\n"
	if !strings.HasPrefix(out, marker) {
		t.Fatalf("clipped output must start with %q", marker)
	}
	rest := strings.TrimPrefix(out, marker)
	if len(rest) > maxLogBytes {
		t.Errorf("tail is %d bytes, want <= %d", len(rest), maxLogBytes)
	}
	if !utf8.ValidString(rest) || strings.ContainsRune(rest, utf8.RuneError) {
		t.Errorf("tail is not valid UTF-8 / contains U+FFFD")
	}
	if !strings.HasSuffix(rest, "END\n") {
		t.Errorf("tail does not end with the real end of the log")
	}
}

func TestProcessManager_LogsTailLinesOnHugeLogHasNoClipMarker(t *testing.T) {
	isolateProcessLogDir(t)
	m, _ := newTestProcessManager(t, 5, time.Hour)
	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f, err := os.OpenFile(logPathOf(t, m, "p1"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(strings.Repeat("x", 3*maxLogBytes) + "\nlast-1\nlast-2\n")
	f.Close()

	out, err := m.Logs("p1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if out != "last-1\nlast-2" {
		t.Errorf("Logs(tail_lines=2) = %q, want the last two whole lines only", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Log file cleanup
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_StopDeletesLogFile(t *testing.T) {
	dir := isolateProcessLogDir(t)
	m, _ := newTestProcessManager(t, 5, time.Hour)
	if _, err := m.Start("p1", longRunningCommand(), ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lp := logPathOf(t, m, "p1")
	if !fileExists(lp) {
		t.Fatalf("log file should exist while running")
	}
	if !m.Stop("p1") {
		t.Fatalf("Stop returned false")
	}
	if fileExists(lp) {
		t.Errorf("log file %s still exists after Stop", lp)
	}
	if left := logFilesIn(t, dir); len(left) != 0 {
		t.Errorf("leftover log files after Stop: %v", left)
	}
}

func TestProcessManager_ShutdownDeletesAllLogFiles(t *testing.T) {
	dir := isolateProcessLogDir(t)
	m, _ := newTestProcessManager(t, 5, time.Hour)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := m.Start(id, longRunningCommand(), ""); err != nil {
			t.Fatalf("Start(%s): %v", id, err)
		}
	}
	if n := len(logFilesIn(t, dir)); n != 3 {
		t.Fatalf("expected 3 log files while running, got %d", n)
	}
	m.Shutdown()
	if left := logFilesIn(t, dir); len(left) != 0 {
		t.Errorf("leftover log files after Shutdown: %v", left)
	}
}

func TestProcessManager_ReaperDeletesLogOfNaturallyExitedProcess(t *testing.T) {
	dir := isolateProcessLogDir(t)
	m, _ := newTestProcessManager(t, 5, 200*time.Millisecond)
	if _, err := m.Start("p1", quickExitCommand("done", 0), ""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	lp := logPathOf(t, m, "p1")
	time.Sleep(350 * time.Millisecond)
	m.reapOnce()
	if fileExists(lp) {
		t.Errorf("reaper left log file %s behind", lp)
	}
	if left := logFilesIn(t, dir); len(left) != 0 {
		t.Errorf("leftover log files after reap: %v", left)
	}
}

func TestSweepStaleProcessLogs(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-3 * time.Hour)

	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	staleA := mk(processLogPrefix+"p1-1"+processLogSuffix, old)
	staleB := mk(processLogPrefix+"p2-2"+processLogSuffix, old)
	fresh := mk(processLogPrefix+"p3-3"+processLogSuffix, time.Now())
	otherLog := mk("someone-elses.log", old)             // wrong prefix
	otherExt := mk(processLogPrefix+"p4-4.txt", old)     // wrong suffix
	noPrefix := mk("notes"+processLogPrefix+".log", old) // prefix not at start

	// A directory with a matching name (and a matching file inside) must be
	// left alone: the sweep is not recursive.
	subdir := filepath.Join(dir, processLogPrefix+"dir"+processLogSuffix)
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(subdir, processLogPrefix+"inner"+processLogSuffix)
	if err := os.WriteFile(inner, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(inner, old, old)

	// A symlink with a matching name pointing outside must be neither
	// followed nor removed (skip where symlinks are unavailable).
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, processLogPrefix+"link"+processLogSuffix)
	haveLink := os.Symlink(outside, link) == nil

	sweepStaleProcessLogs(dir, time.Hour)

	for _, p := range []string{staleA, staleB} {
		if fileExists(p) {
			t.Errorf("stale log %s should have been swept", p)
		}
	}
	for _, p := range []string{fresh, otherLog, otherExt, noPrefix, inner, outside} {
		if !fileExists(p) {
			t.Errorf("%s must not be deleted by the sweep", p)
		}
	}
	if haveLink {
		if _, err := os.Lstat(link); err != nil {
			t.Errorf("symlink must not be removed by the sweep: %v", err)
		}
	}
}

func TestNewProcessManager_SweepsStaleLogsAndToleratesMissingDir(t *testing.T) {
	dir := isolateProcessLogDir(t)

	// Log dir does not exist yet: construction must not fail or panic.
	m := NewProcessManager(t.TempDir(), 2, time.Hour, newTestLogger())
	m.Shutdown()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	stale := filepath.Join(dir, processLogPrefix+"old-1"+processLogSuffix)
	other := filepath.Join(dir, "unrelated.log")
	for _, p := range []string{stale, other} {
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, old, old)
	}
	fresh := filepath.Join(dir, processLogPrefix+"new-2"+processLogSuffix)
	os.WriteFile(fresh, []byte("x"), 0o644)

	m = NewProcessManager(t.TempDir(), 2, time.Hour, newTestLogger())
	t.Cleanup(m.Shutdown)

	if fileExists(stale) {
		t.Errorf("stale prefixed log should be swept at construction")
	}
	if !fileExists(other) || !fileExists(fresh) {
		t.Errorf("unrelated and fresh files must remain (other=%v fresh=%v)", fileExists(other), fileExists(fresh))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Concurrency: limit and duplicate-id races
// ─────────────────────────────────────────────────────────────────────────────

func TestProcessManager_ConcurrentStartsNeverExceedLimit(t *testing.T) {
	dir := isolateProcessLogDir(t)
	const limit, attempts = 2, 6
	m, _ := newTestProcessManager(t, limit, time.Hour)
	discarded := recordDiscarded(t)
	holdStartsAtBarrier(t, attempts)

	var wg sync.WaitGroup
	gate := make(chan struct{})
	errs := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			_, errs[i] = m.Start("p"+itoa(i), longRunningCommand(), "")
		}(i)
	}
	close(gate)
	wg.Wait()

	ok, limited := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
			ok++
		case strings.Contains(err.Error(), "background process limit reached (max_background_processes=2)"):
			limited++
		default:
			t.Errorf("attempt %d: unexpected error %v", i, err)
		}
	}
	if ok != limit || limited != attempts-limit {
		t.Errorf("ok=%d limited=%d, want ok=%d limited=%d", ok, limited, limit, attempts-limit)
	}
	if n := len(m.List()); n > limit {
		t.Errorf("manager holds %d processes, limit is %d", n, limit)
	}
	rejected := discarded()
	if len(rejected) == 0 {
		t.Errorf("expected at least one launched-then-rejected child to exercise the atomic limit check")
	}
	for _, c := range rejected {
		assertReaped(t, c)
	}
	if n := len(logFilesIn(t, dir)); n != limit {
		t.Errorf("%d log files while running, want %d (rejected starts must delete theirs)", n, limit)
	}
}

func TestProcessManager_DuplicateIDRaceKillsAndWaitsLoser(t *testing.T) {
	dir := isolateProcessLogDir(t)
	m, _ := newTestProcessManager(t, 5, time.Hour)
	discarded := recordDiscarded(t)
	holdStartsAtBarrier(t, 2)

	var wg sync.WaitGroup
	gate := make(chan struct{})
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			_, errs[i] = m.Start("same", longRunningCommand(), "")
		}(i)
	}
	close(gate)
	wg.Wait()

	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !strings.Contains(err.Error(), "already in use") {
			t.Errorf("loser error = %v, want the duplicate-id error", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d starts won, want exactly 1", wins)
	}
	if n := len(m.List()); n != 1 {
		t.Errorf("manager holds %d processes, want 1", n)
	}
	losers := discarded()
	if len(losers) != 1 {
		t.Fatalf("expected the loser to be launched and discarded, got %d discarded", len(losers))
	}
	assertReaped(t, losers[0])
	if n := len(logFilesIn(t, dir)); n != 1 {
		t.Errorf("%d log files, want 1 (the loser's must be deleted)", n)
	}
}
