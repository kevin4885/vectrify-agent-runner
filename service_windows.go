//go:build windows

package main

import (
	"log/slog"
	"os"

	"golang.org/x/sys/windows/svc"

	"vectrify/agent-runner/client"
	"vectrify/agent-runner/config"
	"vectrify/agent-runner/runner"
	"vectrify/agent-runner/updater"
	"vectrify/agent-runner/winsvc"
)

// winSvc implements svc.Handler so vectrify-runner can be registered and
// managed by the Windows Service Control Manager.
type winSvc struct {
	log    *slog.Logger
	client *client.Client
	runner *runner.Runner
	uo     updater.Options
}

// Execute is the entry point called by the Windows SCM when the service starts.
// It runs the WebSocket loop in a background goroutine and blocks waiting for
// stop/shutdown control requests.
func (s *winSvc) Execute(_ []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	// Verify (and, with the rights, repair) this service's crash-recovery
	// policy. Auto-update exits non-cleanly and relies on the SCM restarting
	// us; a policy of "take no action" once left the runner down for ~11 hours
	// after an update. Runs in the background so a slow SCM can never delay
	// start-up, and never blocks or fails the service.
	go s.checkRecoveryPolicy()

	updater.Start(config.Version, s.log, s.client.Drain, s.runner.Shutdown, s.uo)

	// Run the connection loop in the background so this goroutine stays free
	// to handle SCM control requests.
	go s.client.RunForever()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for c := range r {
		switch c.Cmd {
		case svc.Stop, svc.Shutdown:
			s.log.Info("windows service: stop requested")
			status <- svc.Status{State: svc.StopPending}
			s.runner.Shutdown()
			os.Exit(0)
		}
	}
	return false, 0
}

// checkRecoveryPolicy makes sure the SCM will restart this service if it dies.
func (s *winSvc) checkRecoveryPolicy() {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("recovery policy check panicked", "panic", p)
		}
	}()
	name, err := winsvc.OwnName()
	if err != nil {
		s.log.Warn("could not determine own service name; skipping recovery policy check", "err", err)
		return
	}
	if _, err := winsvc.EnsureRestartPolicy(name, s.log); err != nil {
		s.log.Warn("service recovery policy is not set to restart on crash and could not be repaired; auto-update falls back to its watchdog",
			"service", name, "err", err)
	}
}

// runService detects whether the process was launched by the Windows SCM and
// runs in service mode if so, otherwise falls back to interactive (terminal) mode.
func runService(log *slog.Logger, c *client.Client, r *runner.Runner, uo updater.Options) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		runInteractive(log, c, r, uo)
		return
	}
	log.Info("starting as windows service")
	if err := svc.Run("VectrifyRunner", &winSvc{log: log, client: c, runner: r, uo: uo}); err != nil {
		log.Error("service run failed", "err", err)
		os.Exit(1)
	}
}
