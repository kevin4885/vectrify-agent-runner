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

	"vectrify/agent-runner/client"
	"vectrify/agent-runner/config"
	"vectrify/agent-runner/runner"
	"vectrify/agent-runner/updater"
)

func main() {
	configPath := flag.String("config", "", "Path to config.yaml (default: ~/.vectrify-runner/config.yaml)")
	flag.Parse()

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
		fmt.Fprintf(os.Stderr, "  allow_shell:    false\n")
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
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening log file %q: %v\n", cfg.LogFile, err)
			os.Exit(1)
		}
		defer f.Close()
		logWriter = f
	}
	log := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: logLevel}))

	log.Info("vectrify agent runner starting",
		"version",        config.Version,
		"platform",       config.Platform(),
		"workspace_root", cfg.WorkspaceRoot,
		"allow_shell",    cfg.AllowShell,
	)

	r := runner.New(cfg.WorkspaceRoot, log)
	c := client.New(cfg, r, log)
	runService(log, c)
}

// runInteractive runs the client with OS signal handling for graceful shutdown.
// Used on all platforms when running directly in a terminal (not as a service daemon).
func runInteractive(log *slog.Logger, c *client.Client) {
	updater.Start(config.Version, log)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig)
		os.Exit(0)
	}()
	c.RunForever()
}
