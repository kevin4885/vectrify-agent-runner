package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// UPDATER_SMOKE_BIN, when set, points at a real runner build stamped with
// UPDATER_SMOKE_VERSION; it exercises smokeTest against the genuine article.
func TestSmokeTest_RealBinary(t *testing.T) {
	bin, ver := os.Getenv("UPDATER_SMOKE_BIN"), os.Getenv("UPDATER_SMOKE_VERSION")
	if bin == "" || ver == "" {
		t.Skip("UPDATER_SMOKE_BIN / UPDATER_SMOKE_VERSION not set")
	}
	if err := smokeTest(bin, ver); err != nil {
		t.Fatalf("good candidate rejected: %v", err)
	}
	if err := smokeTest(bin, "0.0.0-not-this"); err == nil {
		t.Fatal("candidate reporting the wrong version must be rejected")
	}
}

func TestSmokeTest_RejectsGarbageFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vectrify-runner.exe.new.1.2")
	writeFile(t, p, "this is not an executable")
	if err := smokeTest(p, "1.0.0"); err == nil {
		t.Fatal("a file that cannot execute must fail the smoke test")
	}
}

func TestSmokeTest_RejectsWrongProgram(t *testing.T) {
	prog := "true"
	if runtime.GOOS == "windows" {
		prog = "whoami"
	}
	path, err := exec.LookPath(prog)
	if err != nil {
		t.Skipf("%s not available", prog)
	}
	if err := smokeTest(path, "1.0.0"); err == nil {
		t.Fatal("a program that does not report the expected version must fail")
	}
}