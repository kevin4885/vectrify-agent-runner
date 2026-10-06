// Vectrify Agent Runner
//
// A lightweight daemon that connects to the Vectrify API over a persistent
// WebSocket and executes commands on the local machine: file operations,
// shell commands (when enabled), and git operations.
//
// Usage:
//
//	vectrify-runner [--config /path/to/config.yaml]
//
// The config file defaults to ~/.vectrify-runner/config.yaml.
// See config/config.go for the full config reference.
package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mxschmitt/playwright-go"

	"vectrify/agent-runner/client"
	"vectrify/agent-runner/config"
	"vectrify/agent-runner/runner"
	"vectrify/agent-runner/updater"
)

func main() {
	installBrowsers := flag.Bool("install-browsers", false, "Download the Playwright driver + Chromium browser binaries needed for browser commands, then exit. Optional — the runner auto-installs these on the first browser command if missing, so this flag is only useful to pre-warm the install (avoid the ~300MB download delay on that first command) or to run it ahead of time during provisioning. Browser automation is gated by allow_shell — there is no separate allow_browser setting.")
	configPath := flag.String("config", "", "Path to config.yaml (default: ~/.vectrify-runner/config.yaml)")
	showVersion := flag.Bool("version", false, "Print the runner version and exit. Used by the auto-updater to smoke-test a downloaded binary before installing it.")
	// Post-update watchdog (Windows). Started detached by the updater, never by
	// a human; see updater/watchdog.go.
	watchdog := flag.Bool(updater.FlagWatchdog, false, "internal: supervise the service hand-over after an auto-update")
	wdService := flag.String(updater.FlagWatchdogService, "", "internal: SCM service name to supervise")
	wdOldPID := flag.Uint(updater.FlagWatchdogOldPID, 0, "internal: PID of the pre-update process")
	wdVersion := flag.String(updater.FlagWatchdogVersion, "", "internal: version that was just installed")
	wdPrevious := flag.String(updater.FlagWatchdogPrevious, "", "internal: path of the previous binary (rollback target)")
	wdTarget := flag.String(updater.FlagWatchdogTarget, "", "internal: path of the installed (new) binary the service runs")
	flag.Parse()

	if *showVersion {
		fmt.Printf("vectrify-runner %s\n", config.Version)
		os.Exit(0)
	}

	if *installBrowsers {
		fmt.Println("Downloading Playwright driver + Chromium browser binaries (this may take a minute)...")
		if err := playwright.Install(&playwright.RunOptions{Browsers: []string{"chromium"}}); err != nil {
			fmt.Fprintf(os.Stderr, "Error installing browsers: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Done. Browser commands are now available on runners with allow_shell: true.")
		os.Exit(0)
	}

	if *configPath == "" {
		*configPath = config.DefaultConfigPath()
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "Config file should be at: %s\n", *configPath)
		fmt.Fprintf(os.Stderr, "Example config.yaml:\n")
		fmt.Fprintf(os.Stderr, "  api_url:        wss://api.vectrify.ai/api/v1/runner/ws\n")
		fmt.Fprintf(os.Stderr, "  runner_key:     vrun_...\n")
		fmt.Fprintf(os.Stderr, "  workspace_root: /home/user/projects\n")
		fmt.Fprintf(os.Stderr, "  allow_shell:    true\n")
		os.Exit(1)
	}

	// Logger
	logLevel := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	if cfg.LogFile == "" {
		cfg.LogFile = defaultLogFile()
	}
	var logWriter io.Writer = os.Stdout
	if cfg.LogFile != "" {
		rw, err := newRotatingWriter(cfg.LogFile, logMaxBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening log file %q: %v\n", cfg.LogFile, err)
			os.Exit(1)
		}
		defer rw.Close()
		logWriter = rw
	}
	log := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: logLevel}))

	// Config.Load() runs before the logger exists, so any self-correction it
	// performed (e.g. clamping an out-of-range max_heavy_concurrency) is
	// recorded in cfg.Warnings instead of being logged directly. Surface it
	// now so a bad tuning value is visible in the log, not silently
	// swallowed just because the runner chose to boot anyway.
	for _, w := range cfg.Warnings {
		log.Warn("config warning", "detail", w)
	}

	if *watchdog {
		// Supervisor mode: no WebSocket, no updater, no service registration.
		// Just make sure the service comes back healthy (or roll it back).
		out := updater.RunWatchdogProcess(*wdService, *wdVersion, *wdPrevious, *wdTarget, uint32(*wdOldPID), log)
		if out == updater.OutcomeGaveUp {
			os.Exit(1)
		}
		os.Exit(0)
	}

	log.Info("vectrify agent runner starting",
		"version",        config.Version,
		"platform",       config.Platform(),
		"workspace_root", cfg.WorkspaceRoot,
		"allow_shell",    cfg.AllowShell,
	)

	r := runner.New(cfg, log)
	c := client.New(cfg, r, log)
	runService(log, c, r)
}

// runInteractive runs the client with OS signal handling for graceful shutdown.
// Used on all platforms when running directly in a terminal (not as a service daemon).
func runInteractive(log *slog.Logger, c *client.Client, r *runner.Runner) {
	updater.Start(config.Version, log, c.Drain, r.Shutdown)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig)
		r.Shutdown()
		os.Exit(0)
	}()
	c.RunForever()
}
