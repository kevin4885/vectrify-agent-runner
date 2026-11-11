package executor

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// buildShellInvocation — pure unit tests (run on every OS)
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildShellInvocation_Unix_IsBashDashC(t *testing.T) {
	name, args, cleanup := buildShellInvocation("linux", `echo "hi" 'there'`, t.TempDir(), nil)
	defer cleanup()
	if name != "bash" || len(args) != 2 || args[0] != "-c" || args[1] != `echo "hi" 'there'` {
		t.Fatalf("unexpected unix invocation: %q %q", name, args)
	}
}

func TestBuildShellInvocation_Windows_CommandNeverOnCommandLine(t *testing.T) {
	dir := t.TempDir()
	command := "Write-Output 'say \"hi\" \\\" there'; $x = @\"\nq \"w\"\n\"@"
	name, args, cleanup := buildShellInvocation("windows", command, dir, nil)
	defer cleanup()

	if name != "powershell" {
		t.Fatalf("name = %q, want powershell", name)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "say") {
		t.Errorf("user command leaked onto the powershell command line: %q", joined)
	}
	// The whole point: the bootstrap has no double quotes, so no argv
	// escaping layer can mangle it.
	if strings.Contains(args[len(args)-1], `"`) {
		t.Errorf("bootstrap contains a double quote: %q", args[len(args)-1])
	}
	if args[len(args)-2] != "-Command" {
		t.Errorf("expected -Command before the bootstrap, got %q", args)
	}

	// The command file holds the command byte-for-byte (no BOM, no CRLF mangling).
	files, _ := filepath.Glob(filepath.Join(dir, scriptFilePrefix+"*"))
	if len(files) != 1 {
		t.Fatalf("expected exactly 1 command file, found %d", len(files))
	}
	got, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != command {
		t.Errorf("command file content mismatch:\n got %q\nwant %q", got, command)
	}
}

func TestBuildShellInvocation_Windows_CleanupRemovesFileAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	_, _, cleanup := buildShellInvocation("windows", "'x'", dir, nil)
	cleanup()
	cleanup()
	files, _ := filepath.Glob(filepath.Join(dir, scriptFilePrefix+"*"))
	if len(files) != 0 {
		t.Errorf("command file not removed by cleanup: %v", files)
	}
}

func TestBuildShellInvocation_Windows_FallsBackToInlineWhenDirUnwritable(t *testing.T) {
	// A path *under a regular file* can never be created as a directory.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, args, cleanup := buildShellInvocation("windows", "Write-Output 'inline'", filepath.Join(blocker, "sub"), nil)
	defer cleanup()
	if name != "powershell" {
		t.Fatalf("name = %q", name)
	}
	if !strings.HasSuffix(args[len(args)-1], "Write-Output 'inline'") {
		t.Errorf("fallback should pass the command inline, got %q", args[len(args)-1])
	}
}

func TestBuildShellInvocation_Windows_PathWithSingleQuoteIsEscaped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "it's here")
	_, args, cleanup := buildShellInvocation("windows", "'x'", dir, nil)
	defer cleanup()
	b := args[len(args)-1]
	if !strings.Contains(b, "it''s here") {
		t.Errorf("single quote in path not doubled in bootstrap: %q", b)
	}
}

func TestSweepStaleScripts_RemovesOnlyOldCommandFiles(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, scriptFilePrefix+"old.txt")
	fresh := filepath.Join(dir, scriptFilePrefix+"fresh.txt")
	other := filepath.Join(dir, "unrelated-old.txt")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-3 * time.Hour)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	sweepStaleScripts(dir, time.Hour)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale command file should have been swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh command file should remain: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("non-command file must never be swept: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Windows integration: friction items from the runner_shell report
// ─────────────────────────────────────────────────────────────────────────────

// runShell is a small wrapper: run cmd, require a result, return output+code.
func runShell(t *testing.T, cmd, dir string) (string, int) {
	t.Helper()
	s, _ := newTestShell(t)
	chunks, res, _, got := runBounded(t, s, cmd, dir, 30, 40*time.Second)
	if !got {
		t.Fatalf("no result for %q", cmd)
	}
	return combinedOutput(chunks), res.ExitCode
}

// Item 2: quote/backslash/here-string text must arrive intact.
func TestShellRun_Windows_QuotingSurvivesIntact(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()

	cases := map[string]string{
		"double quotes in pattern": `Select-String -InputObject 'a"b' -Pattern '"' | ForEach-Object { 'MATCH:' + $_.Line }`,
		"backslash before quote":   `Write-Output 'C:\temp\' ; Write-Output "x\"`,
		"escaped quote sequence":   `Write-Output 'say "hi" \" there'`,
		"here-string with quotes":  "$x = @\"\nhello \"world\" \\\" end\n\"@\n$x",
		"percent caret ampersand":  `Write-Output '%PATH% ^ & | < >'`,
	}
	want := map[string]string{
		"double quotes in pattern": `MATCH:a"b`,
		"backslash before quote":   `C:\temp\`,
		"escaped quote sequence":   `say "hi" \" there`,
		"here-string with quotes":  `hello "world" \" end`,
		"percent caret ampersand":  `%PATH% ^ & | < >`,
	}
	for name, cmd := range cases {
		out, code := runShell(t, cmd, dir)
		if code != 0 {
			t.Errorf("%s: exit code %d, output %q", name, code, out)
		}
		if !strings.Contains(out, want[name]) {
			t.Errorf("%s: output %q does not contain %q", name, out, want[name])
		}
	}
}

// Long commands must not hit the command-line length limit.
func TestShellRun_Windows_VeryLongCommand(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	var sb strings.Builder
	sb.WriteString("Write-Output 'start'\n")
	for i := 0; i < 600; i++ {
		sb.WriteString("# " + strings.Repeat("x", 98) + "\n") // ~60k chars total
	}
	sb.WriteString("Write-Output 'end'")
	out, code := runShell(t, sb.String(), t.TempDir())
	if code != 0 || !strings.Contains(out, "start") || !strings.Contains(out, "end") {
		t.Errorf("long command failed: code=%d out=%q", code, out)
	}
}

// Exit-code semantics must match `powershell -Command` exactly.
func TestShellRun_Windows_ExitCodeSemantics(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()
	cases := []struct {
		name string
		cmd  string
		want int
	}{
		{"ok", "'fine'", 0},
		{"exit N", "'a'; exit 5; 'b'", 5},
		{"native failure as last statement", "cmd /c exit 3", 1},
		{"native failure then success", "cmd /c exit 3; 'x'", 0},
		{"write-error last", "Write-Error 'oops'", 1},
		{"missing command", "nosuchcmd-xyz", 1},
		{"silently failing last", "Get-Item nosuch -ErrorAction SilentlyContinue", 1},
		{"throw", "throw 'bad'", 1},
		{"parse error", "if (", 1},
		{"ErrorActionPreference Stop", "$ErrorActionPreference='Stop'; Get-Item nosuch; 'unreached'", 1},
		{"try/catch swallows", "try { throw 'x' } catch { 'caught' }", 0},
		{"last is $false value", "$false", 0},
		{"failing last inside if", "if ($true) { cmd /c exit 3 }", 1},
		{"exit after failure", "cmd /c exit 3; exit 0", 0},
	}
	for _, tc := range cases {
		_, code := runShell(t, tc.cmd, dir)
		if code != tc.want {
			t.Errorf("%s: exit code = %d, want %d", tc.name, code, tc.want)
		}
	}
}

// Item 1: relative .NET paths must follow cd / Set-Location / pushd / popd.
func TestShellRun_Windows_DotNetCwdFollowsLocation(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := "cd sub; [IO.File]::WriteAllText('rel-cd.txt', 'x'); " +
		"cd ..; Set-Location -Path sub; [IO.File]::WriteAllText('rel-sl.txt', 'x'); " +
		"cd ..; pushd sub; [IO.File]::WriteAllText('rel-push.txt', 'x'); popd; " +
		"[IO.File]::WriteAllText('rel-root.txt', 'x')"
	out, code := runShell(t, cmd, dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, p := range []string{
		filepath.Join(sub, "rel-cd.txt"),
		filepath.Join(sub, "rel-sl.txt"),
		filepath.Join(sub, "rel-push.txt"),
		filepath.Join(dir, "rel-root.txt"), // after popd we are back at the root
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to exist: %v", p, err)
		}
	}
}

// The proxy must not change error behaviour of the location cmdlets.
func TestShellRun_Windows_LocationProxyKeepsErrorBehaviour(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()

	out, code := runShell(t, "cd nosuchdir", dir)
	if code != 1 || !strings.Contains(out, "Cannot find path") {
		t.Errorf("cd to missing dir: code=%d out=%q (want 1 + 'Cannot find path')", code, out)
	}
	out, code = runShell(t, "pushd nosuchdir", dir)
	if code != 1 || !strings.Contains(out, "Cannot find path") {
		t.Errorf("pushd to missing dir: code=%d out=%q", code, out)
	}
	// -PassThru, pipeline input and named stacks keep working.
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code = runShell(t, "(cd sub -PassThru).Path; 'sub' | Set-Location -ErrorAction SilentlyContinue; Push-Location . -StackName s1; Pop-Location -StackName s1", dir)
	if code != 0 || !strings.Contains(out, "sub") {
		t.Errorf("passthru/pipeline/stack: code=%d out=%q", code, out)
	}
}

// Item 4: BOM-less UTF-8 files read back correctly with no -Encoding.
func TestShellRun_Windows_GetContentDefaultsToUTF8(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "u.txt"), []byte("a \u2014 b \u2500 caf\u00e9"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		"Get-Content u.txt",
		"Get-Content u.txt -Raw",
		"Select-String -Path u.txt -Pattern 'a' | ForEach-Object { $_.Line }",
		"cat u.txt",
	} {
		out, code := runShell(t, cmd, dir)
		if code != 0 || !strings.Contains(out, "a \u2014 b \u2500 caf\u00e9") {
			t.Errorf("%q: code=%d out=%q", cmd, code, out)
		}
	}
	// An explicit -Encoding still wins over the default.
	out, _ := runShell(t, "Get-Content u.txt -Encoding Byte -TotalCount 1", dir)
	if !strings.Contains(out, "97") {
		t.Errorf("explicit -Encoding Byte not honoured: %q", out)
	}
}

// Command files must not accumulate after normal runs.
func TestShellRun_Windows_CommandFileConsumedAndRemoved(t *testing.T) {
	skipIfNotWindows(t)
	// Not parallel on purpose: swaps the package-level shellScriptDir.
	scripts := t.TempDir()
	orig := shellScriptDir
	shellScriptDir = func() string { return scripts }
	defer func() { shellScriptDir = orig }()

	runShell(t, "Write-Output 'one'", t.TempDir())
	runShell(t, "exit 3", t.TempDir())
	runShell(t, "throw 'x'", t.TempDir())

	files, _ := filepath.Glob(filepath.Join(scripts, scriptFilePrefix+"*"))
	if len(files) != 0 {
		t.Errorf("command files left behind: %v", files)
	}
}

// Timeout must still kill the tree, report -1, and leave no command file.
func TestShellRun_Windows_TimeoutStillReportedAndCleaned(t *testing.T) {
	skipIfNotWindows(t)
	scripts := t.TempDir()
	orig := shellScriptDir
	shellScriptDir = func() string { return scripts }
	defer func() { shellScriptDir = orig }()

	s, dir := newTestShell(t)
	chunks, res, _, got := runBounded(t, s, "Start-Sleep -Seconds 60", dir, 3, 30*time.Second)
	if !got {
		t.Fatal("no result")
	}
	if res.ExitCode != -1 || !strings.Contains(combinedOutput(chunks), "timed out") {
		t.Errorf("timeout not reported: code=%d out=%q", res.ExitCode, combinedOutput(chunks))
	}
	files, _ := filepath.Glob(filepath.Join(scripts, scriptFilePrefix+"*"))
	if len(files) != 0 {
		t.Errorf("command files left behind after timeout: %v", files)
	}
}

// runner_process uses the same launcher: quoting + exit code must hold there too.
func TestProcessManager_Windows_UsesSameLauncher(t *testing.T) {
	skipIfNotWindows(t)
	m, dir := newTestProcessManager(t, 5, time.Hour)
	_, err := m.Start("q", `Write-Output 'say "hi" \" there'; Start-Sleep -Seconds 30`, dir)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	logs, err := m.Logs("q", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, `say "hi" \" there`) {
		t.Errorf("quoting mangled under runner_process: %q", logs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Width, colour environment, parse-error reporting
// ─────────────────────────────────────────────────────────────────────────────

// runShellStreams is runShell with stdout and stderr kept apart.
func runShellStreams(t *testing.T, cmd, dir string) (stdout, stderr string, code int) {
	t.Helper()
	s, _ := newTestShell(t)
	chunks, res, _, got := runBounded(t, s, cmd, dir, 30, 40*time.Second)
	if !got {
		t.Fatalf("no result for %q", cmd)
	}
	var so, se strings.Builder
	for _, c := range chunks {
		if c.Stream == "stderr" {
			se.WriteString(c.Data)
		} else {
			so.WriteString(c.Data)
		}
	}
	return so.String(), se.String(), res.ExitCode
}

func longestLine(s string) int {
	longest := 0
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if len(l) > longest {
			longest = len(l)
		}
	}
	return longest
}

// unsetenv removes key for the duration of the test (t.Setenv registers the
// restore of the original value; the Unsetenv then clears it).
func unsetenv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestShellEnv_NoColorRules(t *testing.T) {
	count := func(env []string) (n int, val string) {
		for _, kv := range env {
			if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, "NO_COLOR") {
				n++
				val = v
			}
		}
		return
	}
	t.Run("unset adds NO_COLOR=1", func(t *testing.T) {
		unsetenv(t, "NO_COLOR")
		env := shellEnv()
		if n, v := count(env); n != 1 || v != "1" {
			t.Errorf("NO_COLOR entries=%d value=%q, want 1 entry of 1", n, v)
		}
	})
	t.Run("user value wins", func(t *testing.T) {
		t.Setenv("NO_COLOR", "custom")
		if n, v := count(shellEnv()); n != 1 || v != "custom" {
			t.Errorf("NO_COLOR entries=%d value=%q, want 1 entry of custom", n, v)
		}
	})
	t.Run("set to empty counts as defined", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		if n, v := count(shellEnv()); n != 1 || v != "" {
			t.Errorf("NO_COLOR entries=%d value=%q, want 1 empty entry", n, v)
		}
	})
	t.Run("key case is ignored on Windows", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("environment names are case-insensitive only on Windows")
		}
		unsetenv(t, "NO_COLOR")
		t.Setenv("No_Color", "mixed")
		if n, v := count(shellEnv()); n != 1 || v != "mixed" {
			t.Errorf("NO_COLOR entries=%d value=%q, want 1 entry of mixed", n, v)
		}
	})
	t.Run("inherited env is preserved and FORCE_COLOR untouched", func(t *testing.T) {
		unsetenv(t, "NO_COLOR")
		unsetenv(t, "FORCE_COLOR")
		t.Setenv("VEC_P2_MARKER", "keepme")
		env := shellEnv()
		want := len(os.Environ()) + 1
		if len(env) != want {
			t.Errorf("len(env) = %d, want %d (inherited + NO_COLOR)", len(env), want)
		}
		found := false
		for _, kv := range env {
			if kv == "VEC_P2_MARKER=keepme" {
				found = true
			}
			if strings.HasPrefix(strings.ToUpper(kv), "FORCE_COLOR=") {
				t.Errorf("FORCE_COLOR must never be set: %q", kv)
			}
		}
		if !found {
			t.Error("inherited variable missing from shellEnv()")
		}
	})
}

func TestWinLauncher_NoDoubleQuotes(t *testing.T) {
	if strings.Contains(winBootstrapFmt, `"`) {
		t.Errorf("winBootstrapFmt contains a double quote: %q", winBootstrapFmt)
	}
	if strings.Contains(winPreamble, `"`) {
		t.Errorf("winPreamble contains a double quote: %q", winPreamble)
	}
	if strings.Contains(winInlineNoteCmd, `"`) {
		t.Errorf("winInlineNoteCmd contains a double quote: %q", winInlineNoteCmd)
	}
}

func TestWinPreamble_WidthIsTryCatchGuarded(t *testing.T) {
	const assign = "$Host.UI.RawUI.BufferSize = New-Object System.Management.Automation.Host.Size(1024, 3000)"
	if !strings.Contains(winHostWidth, "Size(1024, 3000)") {
		t.Errorf("width constant is not 1024x3000: %q", winHostWidth)
	}
	if !strings.Contains(winPreamble, "try { "+assign+" } catch {}") {
		t.Errorf("buffer assignment is not wrapped in try { } catch {}: %q", winPreamble)
	}
	// Both launch paths carry the preamble, so both get the width.
	_, args, cleanup := buildShellInvocation("windows", "'x'", t.TempDir(), nil)
	defer cleanup()
	if !strings.Contains(args[len(args)-1], assign) {
		t.Errorf("file-based bootstrap lacks the width setting: %q", args[len(args)-1])
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, args, cleanup2 := buildShellInvocation("windows", "'x'", filepath.Join(blocker, "sub"), nil)
	defer cleanup2()
	if !strings.Contains(args[len(args)-1], assign) {
		t.Errorf("inline-fallback args lack the width setting: %q", args[len(args)-1])
	}
}

func TestShellRun_Windows_OutputNotWrappedAtHostWidth(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()

	out, code := runShell(t, `(('a' * 100) + ' FAILED ' + ('b' * 192)) | Select-String FAILED`, dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if n := longestLine(out); n < 300 {
		t.Errorf("Select-String match was wrapped: longest line %d, want >= 300:\n%s", n, out)
	}

	out, code = runShell(t, `[pscustomobject]@{ Col = ('z' * 250) } | Format-Table`, dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if n := longestLine(out); n < 250 {
		t.Errorf("Format-Table was wrapped/truncated: longest line %d, want >= 250:\n%s", n, out)
	}
}

func TestShellRun_Windows_FollowingCommandRunsWhenWidthCannotBeSet(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	// Same guard text as the preamble, but with an assignment that throws:
	// proves a failing buffer set does not stop what follows.
	out, code := runShell(t, `try { $Host.UI.RawUI.BufferSize = New-Object System.Management.Automation.Host.Size(-1, -1) } catch {}; Write-Output 'still-ran'`, t.TempDir())
	if code != 0 || !strings.Contains(out, "still-ran") {
		t.Errorf("command after a failed width set did not run: code=%d out=%q", code, out)
	}
}

func TestShellRun_Windows_NoColorEnvReachesChild(t *testing.T) {
	skipIfNotWindows(t)
	// Not parallel: changes the process environment.
	dir := t.TempDir()
	probe := `'NC=[' + $env:NO_COLOR + '] FC=[' + $env:FORCE_COLOR + ']'`

	unsetenv(t, "NO_COLOR")
	unsetenv(t, "FORCE_COLOR")
	out, code := runShell(t, probe, dir)
	if code != 0 || !strings.Contains(out, "NC=[1] FC=[]") {
		t.Errorf("unset: code=%d out=%q, want NC=[1] FC=[]", code, out)
	}

	t.Setenv("NO_COLOR", "custom")
	out, code = runShell(t, probe, dir)
	if code != 0 || !strings.Contains(out, "NC=[custom] FC=[]") {
		t.Errorf("custom: code=%d out=%q, want NC=[custom] FC=[]", code, out)
	}
}

func TestShellRun_Windows_ParseErrorReportsOnlyParserMessage(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	stdout, stderr, code := runShellStreams(t, `$x = 1; if ($x -eq 1 { "bad" }`, t.TempDir())
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	for _, want := range []string{"Unexpected token '{'", "char:22"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %q", want, stderr)
		}
	}
	low := strings.ToLower(stderr)
	for _, bad := range []string{"exception calling", "parseexception", "scriptblock", "__vec"} {
		if strings.Contains(low, bad) {
			t.Errorf("stderr leaks launcher internals (%q): %q", bad, stderr)
		}
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should be empty, got %q", stdout)
	}
}

func TestShellRun_Windows_ParseErrorRunsNothingAndKeepsUserLineNumbers(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()

	stdout, _, code := runShellStreams(t, `Write-Output before; if ( {`, dir)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if strings.Contains(stdout, "before") {
		t.Errorf("statement before the syntax error must not run: %q", stdout)
	}

	_, stderr, code := runShellStreams(t, "Write-Output one\nWrite-Output two\nif ( {\nWrite-Output four", dir)
	if code != 1 {
		t.Errorf("multi-line: exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "At line:3") {
		t.Errorf("multi-line parse error should report line 3 of the user's command: %q", stderr)
	}
}

func TestShellRun_Windows_RuntimeErrorsUnchangedByParseGuard(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()

	_, stderr, code := runShellStreams(t, `throw "boom"`, dir)
	if code != 1 || !strings.Contains(stderr, "boom") {
		t.Errorf("throw: code=%d stderr=%q, want 1 + boom", code, stderr)
	}
	if _, _, code := runShellStreams(t, `exit 7`, dir); code != 7 {
		t.Errorf("exit 7: code = %d", code)
	}
	if _, _, code := runShellStreams(t, `Get-Item C:\does-not-exist`, dir); code != 1 {
		t.Errorf("failing last statement: code = %d, want 1", code)
	}
	if _, _, code := runShellStreams(t, `'ok'`, dir); code != 0 {
		t.Errorf("success: code = %d, want 0", code)
	}
}

func TestProcessManager_Windows_ChildGetsNoColor(t *testing.T) {
	skipIfNotWindows(t)
	// Not parallel: changes the process environment.
	unsetenv(t, "NO_COLOR")
	m, dir := newTestProcessManager(t, 5, time.Hour)
	if _, err := m.Start("nc", `Write-Output ('NC=[' + $env:NO_COLOR + ']'); Start-Sleep -Seconds 30`, dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	logs, err := m.Logs("nc", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !strings.Contains(logs, "NC=[1]") {
		t.Errorf("runner_process child did not get NO_COLOR=1: %q", logs)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Round 2: padding regression, unreadable command file, inline-fallback note
// ─────────────────────────────────────────────────────────────────────────────

// The host width must stay narrow enough that the default table view does not
// pad every row to the full buffer width (4096 turned a 5-file Get-ChildItem
// into ~33k characters).
func TestShellRun_Windows_TableOutputIsNotPadded(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	dir := t.TempDir()

	out, code := runShell(t, `Get-ChildItem C:\Windows\System32\drivers\etc | Out-String`, dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if len(out) >= 12000 {
		t.Errorf("directory listing is padded: %d chars, want < 12000", len(out))
	}
	if n := longestLine(out); n > 1100 {
		t.Errorf("longest line is %d chars, want <= 1100", n)
	}

	out, code = runShell(t, `Get-ChildItem C:\Windows\System32 -File | Select-Object -First 30`, dir)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if len(out) >= 60000 {
		t.Errorf("30-file listing is %d chars, want < 60000 (API cap is 100000)", len(out))
	}
}

// runInvocation runs a powershell invocation built by buildShellInvocation,
// keeping stdout and stderr apart.
func runInvocation(t *testing.T, name string, args []string, dir string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	c.Dir = dir
	c.Env = shellEnv()
	var so, se bytes.Buffer
	c.Stdout, c.Stderr = &so, &se
	err := c.Run()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run %s: %v", name, err)
		}
		code = ee.ExitCode()
	}
	return so.String(), se.String(), code
}

func TestShellRun_Windows_UnreadableCommandFileFailsWithExit1(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	scripts := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran.txt")

	name, args, cleanup := buildShellInvocation("windows", "Set-Content -LiteralPath '"+marker+"' -Value x; Write-Output 'should-not-run'", scripts, nil)
	defer cleanup()
	files, _ := filepath.Glob(filepath.Join(scripts, scriptFilePrefix+"*"))
	if len(files) != 1 {
		t.Fatalf("expected 1 staged file, found %d", len(files))
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runInvocation(t, name, args, t.TempDir())
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "runner: command file unreadable:") {
		t.Errorf("stderr lacks the unreadable-file message: %q", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout should be empty, got %q", stdout)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the command ran even though its file was unreadable")
	}
}

// Documents the best-effort nature of the note: with a syntactically valid
// command the inline fallback prints it on stderr first. (On a syntax error
// PowerShell reports the error before anything runs, so the note is absent;
// the runner log still carries the warning.)
func TestShellRun_Windows_InlineFallbackPrintsNoteAndStillRuns(t *testing.T) {
	skipIfNotWindows(t)
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	const note = "runner: note: command file could not be staged; command ran inline (quoting and syntax-error positions may differ)"

	name, args, cleanup := buildShellInvocation("windows", "Write-Output 'inline-ran'; exit 4", filepath.Join(blocker, "sub"), nil)
	defer cleanup()
	stdout, stderr, code := runInvocation(t, name, args, t.TempDir())
	if code != 4 {
		t.Errorf("exit code = %d, want 4 (stderr %q)", code, stderr)
	}
	if !strings.HasPrefix(stderr, note+"\r\n") && !strings.HasPrefix(stderr, note+"\n") {
		t.Errorf("stderr should begin with the note line, got %q", stderr)
	}
	if !strings.Contains(stdout, "inline-ran") {
		t.Errorf("the command's own output is missing: %q", stdout)
	}

	// The file-based path never prints the note.
	name, args, cleanup2 := buildShellInvocation("windows", "Write-Output 'file-ran'", t.TempDir(), nil)
	defer cleanup2()
	stdout, stderr, code = runInvocation(t, name, args, t.TempDir())
	if code != 0 || !strings.Contains(stdout, "file-ran") {
		t.Errorf("file-based run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stderr, "runner: note") {
		t.Errorf("file-based path printed the inline note: %q", stderr)
	}
}

func TestBuildShellInvocation_InlineFallbackWarnsViaGivenLogger(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	_, _, cleanup := buildShellInvocation("windows", "'x'", filepath.Join(blocker, "sub"), lg)
	defer cleanup()
	if !strings.Contains(buf.String(), "could not stage command file") {
		t.Errorf("warning not written to the supplied logger: %q", buf.String())
	}

	// nil logger must not panic (falls back to slog.Default()).
	_, _, cleanup2 := buildShellInvocation("windows", "'x'", filepath.Join(blocker, "sub"), nil)
	defer cleanup2()
}