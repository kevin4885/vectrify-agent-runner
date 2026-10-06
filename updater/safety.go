package updater

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// smokeTestTimeout bounds how long the candidate binary gets to start and
// print its version. Generous because the first execution of a freshly
// downloaded file can be slowed by antivirus scanning it.
const smokeTestTimeout = 30 * time.Second

// smokeTest executes the downloaded-and-checksum-verified candidate binary at
// path with `-version` and requires it to run, exit 0, and report
// wantVersion. It runs BEFORE the running binary is touched.
//
// ROOT-CAUSE NOTE: a checksum proves the bytes match what was published; it
// does not prove the file can run on THIS machine. The v1.0.17 incident
// ("... is not a valid Win32 application") and AV quarantine of a fresh exe
// are both cases where a verified file is still unusable, and installing it
// turns "update failed, keep running" into "service permanently down". Running
// the candidate once first converts those into a logged failed update that
// leaves the working install untouched.
func smokeTest(path, wantVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), smokeTestTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "-version").CombinedOutput()
	got := strings.TrimSpace(string(out))
	if len(got) > 300 {
		got = got[:300] + "..."
	}
	if ctx.Err() != nil {
		return fmt.Errorf("candidate binary did not respond to -version within %s", smokeTestTimeout)
	}
	if err != nil {
		return fmt.Errorf("candidate binary failed to run (%v); output: %q", err, got)
	}
	if !strings.Contains(got, wantVersion) {
		return fmt.Errorf("candidate binary reports %q, expected version %s", got, wantVersion)
	}
	return nil
}

// badVersionFile records a release that was installed, failed to stay up, and
// was rolled back. It lives next to the executable (like the update lock).
// Its content is the bad version string.
const badVersionFile = ".vectrify-runner-bad-version"

// markBadVersion records version as one the updater must not install again.
func markBadVersion(exePath, version string) error {
	p := filepath.Join(filepath.Dir(exePath), badVersionFile)
	return os.WriteFile(p, []byte(version+"\n"), 0o644)
}

// isBadVersion reports whether version was previously rolled back. Without
// this, a rollback would just re-download and re-install the same broken
// release on the next hourly check, forever.
func isBadVersion(exePath, version string) bool {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(exePath), badVersionFile))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == version
}
