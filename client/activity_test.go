package client

import (
	"sync"
	"testing"
	"time"

	"vectrify/agent-runner/protocol"
)

func TestActivity_FreshClient_CountsAsActive(t *testing.T) {
	c := newTestClient(4, 2, time.Second)
	c.touch() // New() does this; the test constructor bypasses New()
	busy, last := c.Activity()
	if busy {
		t.Error("nothing is running yet")
	}
	if last.IsZero() || time.Since(last) > 5*time.Second {
		t.Errorf("a new client must report recent activity, got %v", last)
	}
}

func TestActivity_BusyWhileCommandRuns_AndFinishRefreshesLastActive(t *testing.T) {
	c := newTestClient(4, 2, time.Second)
	started, release := make(chan struct{}), make(chan struct{})
	c.dispatchFunc = func(protocol.RawCommand, func(interface{}), func()) {
		close(started)
		<-release
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); c.dispatchOne(rawCmd("a", "shell"), "a", "shell", discardSend, func() {}) }()
	<-started

	busy, _ := c.Activity()
	if !busy {
		t.Fatal("a running command must report busy")
	}

	// make lastActive look old, then let the command finish: finishing must refresh it.
	c.lastActive.Store(time.Now().Add(-time.Hour).UnixNano())
	close(release)
	wg.Wait()

	busy, last := c.Activity()
	if busy {
		t.Error("nothing should be running after the command finished")
	}
	if time.Since(last) > 5*time.Second {
		t.Errorf("finishing a command must count as activity (quiet period runs from the END); last=%v ago", time.Since(last))
	}
}

// A command waiting for a slot is activity too, or an update could slip in
// between "queued" and "running".
func TestActivity_WaiterCountsAsBusy(t *testing.T) {
	c := newTestClient(1, 1, time.Second)
	if !c.tryReserveWaiter() {
		t.Fatal("reserve")
	}
	if busy, _ := c.Activity(); !busy {
		t.Fatal("a pending waiter must report busy")
	}
	c.releaseWaiter()
	if busy, _ := c.Activity(); busy {
		t.Fatal("no waiters left: not busy")
	}
}