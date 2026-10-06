//go:build windows

package winsvc

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// TestEnsureRestartPolicy_RepairsBrokenPolicy reproduces the production
// incident against a real (throwaway) SCM service: delays configured but every
// action type "none". Needs administrator rights, so it skips otherwise.
func TestEnsureRestartPolicy_RepairsBrokenPolicy(t *testing.T) {
	m, err := mgr.Connect()
	if err != nil {
		t.Skipf("cannot connect to SCM (need administrator): %v", err)
	}
	defer m.Disconnect()

	const name = "VectrifyRecoveryTest"
	if old, err := m.OpenService(name); err == nil {
		old.Delete()
		old.Close()
	}
	s, err := m.CreateService(name, `C:\Windows\System32\notepad.exe`, mgr.Config{StartType: mgr.StartManual})
	if err != nil {
		t.Skipf("cannot create test service (need administrator): %v", err)
	}
	defer func() { s.Delete(); s.Close() }()

	n := mgr.RecoveryAction{Type: mgr.NoAction, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{n, n, n}, 3600); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.RecoveryActions(); AllRestart(got) {
		t.Fatal("precondition: policy should be broken")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	repaired, err := EnsureRestartPolicy(name, log)
	if err != nil {
		t.Fatalf("EnsureRestartPolicy: %v", err)
	}
	if !repaired {
		t.Error("expected repaired=true")
	}
	got, err := s.RecoveryActions()
	if err != nil || !AllRestart(got) || len(got) != 3 {
		t.Fatalf("policy after repair = %v (err %v)", got, err)
	}

	// Idempotent: a healthy policy is left alone.
	repaired, err = EnsureRestartPolicy(name, log)
	if err != nil || repaired {
		t.Errorf("second call: repaired=%v err=%v, want false,nil", repaired, err)
	}

	// Controller basics against the same service.
	c := Controller{Name: name}
	st, err := c.Status()
	if err != nil || st.ProcessId != 0 {
		t.Errorf("Status of stopped service = %+v, %v", st, err)
	}
	if err := c.Stop(); err != nil { // stopping a stopped service is success
		t.Errorf("Stop on stopped service: %v", err)
	}
	_ = windows.ERROR_ACCESS_DENIED
}
