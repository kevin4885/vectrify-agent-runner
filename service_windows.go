//go:build windows

package main

import (
	"log/slog"
	"os"

	"golang.org/x/sys/windows/svc"

	"vectrify/agent-runner/client"
)

// winSvc implements svc.Handler so vectrify-runner can be registered and
// managed by the Windows Service Control Manager.
type winSvc struct {
	log    *slog.Logger
	client *client.Client
}

// Execute is the entry point called by the Windows SCM when the service starts.
// It runs the WebSocket loop in a background goroutine and blocks waiting for
// stop/shutdown control requests.
func (s *winSvc) Execute(_ []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	// Run the connection loop in the background so this goroutine stays free
	// to handle SCM control requests.
	go s.client.RunForever()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for c := range r {
		switch c.Cmd {
		case svc.Stop, svc.Shutdown:
			s.log.Info("windows service: stop requested")
			status <- svc.Status{State: svc.StopPending}
			os.Exit(0)
		}
	}
	return false, 0
}

// runService detects whether the process was launched by the Windows SCM and
// runs in service mode if so, otherwise falls back to interactive (terminal) mode.
func runService(log *slog.Logger, c *client.Client) {
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		runInteractive(log, c)
		return
	}
	log.Info("starting as windows service")
	if err := svc.Run("VectrifyRunner", &winSvc{log: log, client: c}); err != nil {
		log.Error("service run failed", "err", err)
		os.Exit(1)
	}
}
