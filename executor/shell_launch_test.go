package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// buildShellInvocation — pure unit tests (run on every OS)
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildShellInvocation_Unix_IsBashDashC(t *testing.T) {
	name, args, cleanup := buildShellInvocation("linux", `echo "hi" 'there'`, t.TempDir())
	defer cleanup()
	if name != "bash" || len(args) != 2 || args[0] != "-c" || args[1] != `echo "hi" 'there'` {
		t.Fatalf("unexpected unix invocation: %q %q", name, args)
	}
}

func TestBuildShellInvocation_Windows_CommandNeverOnCommandLine(t *testing.T) {
	dir := t.TempDir()
	command := "Write-Output 'say \"hi\" \\\" there'; $x = @\"\nq \"w\"\n\"@"
	name, args, cleanup := buildShellInvocation("windows", command, dir)
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
	_, _, cleanup := buildShellInvocation("windows", "'x'", dir)
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
	name, args, cleanup := buildShellInvocation("windows", "Write-Output 'inline'", filepath.Join(blocker, "sub"))
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
	_, args, cleanup := buildShellInvocation("windows", "'x'", dir)
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
