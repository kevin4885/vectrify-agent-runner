package executor

// shell_launch.go — the ONE place that decides how a command string becomes
// an OS process invocation. Both Shell.Run (runner_shell) and
// ProcessManager.Start (runner_process) call shellInvocation, so the two
// tools can never drift apart in quoting, encoding, or working-directory
// behaviour.
//
// Unix: `bash -c <command>` — unchanged.
//
// Windows (Windows PowerShell 5.1): the user's command is NOT passed on the
// powershell command line. Doing so means it travels through Go's
// argv-escaping and then PowerShell's own command-line re-parsing, and
// quote / backslash sequences in the user's text can be mangled or break
// the parse of the whole command. Instead:
//
//  1. the command is written verbatim (UTF-8, no BOM) to a private temp file
//  2. powershell is started with a short, constant bootstrap (-Command)
//     that contains NO double quotes, so there is nothing to mis-escape
//  3. the bootstrap reads the file, deletes it, and dot-sources the text
//     as a script block in the global scope
//
// Why not -EncodedCommand: on PowerShell 5.1 it emits `#< CLIXML` blobs on
// stderr (progress/error streams are serialised), is capped near 16k
// characters by the CreateProcess command-line limit, and base64-UTF16
// powershell invocations are a classic antivirus heuristic.
//
// Why not -File: -File changes exit-code semantics (a failing last statement
// no longer yields exit code 1). The bootstrap preserves the exact
// `-Command` semantics: exit code 1 when the last statement fails, `exit N`
// honoured, terminating errors give 1.
//
// The bootstrap also fixes three friction points that cost agents tool
// calls on Windows:
//
//   - .NET's process current directory does not follow PowerShell's
//     location, so `cd sub; [IO.File]::WriteAllText('x.txt', ...)` wrote to
//     the wrong place. Set-/Push-/Pop-Location are replaced by proxy
//     functions (generated from the real cmdlets with ProxyCommand, so
//     parameters, pipeline input and error behaviour are identical) that
//     also set [Environment]::CurrentDirectory afterwards.
//   - Get-Content / Select-String default to the ANSI code page when a file
//     has no BOM, turning UTF-8 em dashes into mojibake. They now default to
//     -Encoding UTF8 (files with a BOM are still detected; an explicit
//     -Encoding still wins). Write-side defaults are deliberately NOT
//     changed: UTF8 on PowerShell 5.1 writes a BOM.
//   - Console input/output encoding is forced to UTF-8 (pre-existing).

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// winUTF8Preamble forces UTF-8 for console I/O so Unicode in file content
// (em dashes, box drawing, ...) is not mangled by the system code page.
const winUTF8Preamble = "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; " +
	"$OutputEncoding = [System.Text.Encoding]::UTF8; "

// winReadDefaults makes the read-side cmdlets treat BOM-less files as UTF-8.
const winReadDefaults = "$PSDefaultParameterValues['Get-Content:Encoding'] = 'UTF8'; " +
	"$PSDefaultParameterValues['Select-String:Encoding'] = 'UTF8'; "

// winLocationSync replaces Set-/Push-/Pop-Location with proxies that also
// keep [Environment]::CurrentDirectory in step. Everything is in try/catch:
// if anything here fails the command still runs, just without the sync.
// Contains no double quotes (see the file comment).
const winLocationSync = "foreach ($__n in 'Set-Location','Push-Location','Pop-Location') { try { " +
	"$__c = $ExecutionContext.InvokeCommand.GetCommand('Microsoft.PowerShell.Management\\' + $__n, 'Cmdlet'); " +
	"$__b = [System.Management.Automation.ProxyCommand]::Create((New-Object System.Management.Automation.CommandMetadata $__c)); " +
	"$__b = $__b.Replace('$steppablePipeline.End()', '$steppablePipeline.End(); try { [Environment]::CurrentDirectory = " +
	"(Microsoft.PowerShell.Management\\Get-Location -PSProvider FileSystem).ProviderPath } catch {}'); " +
	"Set-Item -Path ('Function:\\global:' + $__n) -Value ([scriptblock]::Create($__b)) } catch {} }; " +
	"Remove-Variable __n,__c,__b -ErrorAction SilentlyContinue; "

// winHostWidth widens the host buffer. In a redirected, non-interactive
// session the host reports a 99-column buffer, and the formatting cmdlets
// (Select-String match display, Format-Table, error records) hard-wrap at
// it. Guarded: if the host refuses, the command still runs, just wrapped.
// Contains no double quotes (see the file comment).
const winHostWidth = "try { $Host.UI.RawUI.BufferSize = New-Object System.Management.Automation.Host.Size(4096, 3000) } catch {}; "

// winPreamble is everything that runs before the user's command.
const winPreamble = winUTF8Preamble + winHostWidth + winReadDefaults + winLocationSync

// winBootstrapFmt reads the command file (%s = path, single-quote escaped),
// deletes it, then dot-sources the text. The trailing `;$global:... = $?`
// appended to the script text records whether the user's LAST statement
// succeeded, and the final `if` turns that into exit code 1 — exactly what
// `powershell -Command` does natively.
//
// Only the [scriptblock]::Create call is wrapped in try/catch. A parse
// error in the user's text would otherwise surface as "Exception calling
// Create ..." followed by a second error block pointing into this
// bootstrap. The catch prints just the parser's own message (the inner
// ParseException, whose positions are relative to the user's text) to
// stderr and exits 1; nothing from the user's command has run. Runtime
// errors are not caught here, so they behave exactly as before.
const winBootstrapFmt = "$__vecF = '%s'; " +
	"$__vecT = [IO.File]::ReadAllText($__vecF, [Text.Encoding]::UTF8); " +
	"Remove-Item -LiteralPath $__vecF -Force -ErrorAction SilentlyContinue; " +
	"$global:__vecOk = $true; " +
	"try { $__vecS = [scriptblock]::Create($__vecT + [char]10 + ';$global:__vecOk = $?') } " +
	"catch { $__vecE = $_.Exception; if ($__vecE.InnerException) { $__vecE = $__vecE.InnerException }; " +
	"[Console]::Error.WriteLine($__vecE.Message); exit 1 }; " +
	". $__vecS; " +
	"if (-not $global:__vecOk) { exit 1 }"

// shellEnv is the environment for every shell / process child: the runner's
// own environment (unchanged) plus NO_COLOR=1, so CLIs that honour it do not
// emit ANSI colour into captured output. If NO_COLOR is already defined —
// even set to empty — the user's value wins. Keys compare case-insensitively
// (Windows environment names are case-insensitive). FORCE_COLOR is
// deliberately never set: some tools treat any value as "force colour on".
func shellEnv() []string {
	env := os.Environ()
	for _, kv := range env {
		if key, _, _ := strings.Cut(kv, "="); strings.EqualFold(key, "NO_COLOR") {
			return env
		}
	}
	return append(env, "NO_COLOR=1")
}

// scriptFilePrefix / staleScriptAge: command files are normally deleted by
// the bootstrap within milliseconds. A file only survives if powershell was
// killed before reading it; those are swept when the next one is written.
const (
	scriptFilePrefix = "cmd-"
	staleScriptAge   = time.Hour
)

// shellScriptDir returns the directory for command files. A var so tests can
// redirect it.
var shellScriptDir = func() string {
	return filepath.Join(os.TempDir(), "vectrify-runner-scripts")
}

// shellInvocation returns the executable, its arguments, and a cleanup func
// for running command through the platform shell. cleanup is idempotent and
// only removes a command file that powershell has not already consumed; it
// is safe to call after the process has finished. ProcessManager must NOT
// call it after a successful Start (the file may not have been read yet) —
// the bootstrap deletes the file itself, and leftovers are swept.
func shellInvocation(command string) (name string, args []string, cleanup func()) {
	return buildShellInvocation(runtime.GOOS, command, shellScriptDir())
}

func buildShellInvocation(goos, command, scriptDir string) (string, []string, func()) {
	noop := func() {}
	if goos != "windows" {
		return "bash", []string{"-c", command}, noop
	}

	base := []string{"-NoProfile", "-NonInteractive", "-Command"}

	path, err := writeCommandScript(scriptDir, command)
	if err != nil {
		// Could not stage the command file (read-only/full temp dir, ...).
		// Fall back to passing the command inline — the previous behaviour —
		// rather than failing a command that would have worked.
		return "powershell", append(base, winPreamble+command), noop
	}

	bootstrap := winPreamble + fmt.Sprintf(winBootstrapFmt, strings.ReplaceAll(path, "'", "''"))
	return "powershell", append(base, bootstrap), func() { _ = os.Remove(path) }
}

// writeCommandScript stores command as UTF-8 (no BOM) in a new file under
// dir and returns its path. Best-effort sweeps stale files first.
func writeCommandScript(dir, command string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sweepStaleScripts(dir, staleScriptAge)

	// ".txt", not ".ps1": the file is read as data, never executed, and a
	// .ps1 invites script-scanning heuristics for no benefit.
	f, err := os.CreateTemp(dir, scriptFilePrefix+"*.txt")
	if err != nil {
		return "", err
	}
	path := f.Name()
	if _, err := f.WriteString(command); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// sweepStaleScripts removes command files in dir older than maxAge.
func sweepStaleScripts(dir string, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), scriptFilePrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
