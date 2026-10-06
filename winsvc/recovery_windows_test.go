//go:build windows

package winsvc

import (
	"testing"
	"time"

	"golang.org/x/sys/windows/svc/mgr"
)

func TestAllRestart(t *testing.T) {
	r := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: time.Second}
	n := mgr.RecoveryAction{Type: mgr.NoAction, Delay: time.Second}
	cases := []struct {
		name string
		in   []mgr.RecoveryAction
		want bool
	}{
		{"nil", nil, false},
		{"empty", []mgr.RecoveryAction{}, false},
		{"all restart", []mgr.RecoveryAction{r, r, r}, true},
		// The exact state found on the affected machine: delays present,
		// every action type "none".
		{"all none (the incident)", []mgr.RecoveryAction{n, n, n}, false},
		{"restart then none", []mgr.RecoveryAction{r, n}, false},
		{"none then restart", []mgr.RecoveryAction{n, r}, false},
		{"reboot is not restart", []mgr.RecoveryAction{{Type: mgr.ComputerReboot}}, false},
	}
	for _, c := range cases {
		if got := AllRestart(c.in); got != c.want {
			t.Errorf("%s: AllRestart = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDesiredRecoveryActions_AreAllRestart(t *testing.T) {
	if !AllRestart(DesiredRecoveryActions()) {
		t.Fatal("the policy we install must itself satisfy AllRestart")
	}
}
