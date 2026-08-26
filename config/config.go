// Package config loads and validates the runner configuration.
//
// Config file location (searched in order):
//  1. Path set by --config flag
//  2. $VECTRIFY_RUNNER_CONFIG env var
//  3. ~/.vectrify-runner/config.yaml  (default)
//
// Minimal config.yaml example:
//
//	api_url:        wss://api.vectrify.ai/api/v1/runner/ws
//	runner_key:     vrun_...
//	workspace_root: /home/user/projects
//	allow_shell:    true
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is stamped at build time via -ldflags.
var Version = "dev"

// Config holds all runner settings loaded from config.yaml.
type Config struct {
	// APIURL is the WebSocket URL of the Vectrify API runner endpoint.
	// Example: wss://api.vectrify.ai/api/v1/runner/ws
	APIURL string `yaml:"api_url"`

	// RunnerKey is the vrun_ key generated when the runner was registered.
	RunnerKey string `yaml:"runner_key"`

	// WorkspaceRoot is the absolute path that scopes all file operations.
	// The runner rejects any path outside this directory.
	WorkspaceRoot string `yaml:"workspace_root"`

	// AllowShell enables unrestricted shell command execution.
	// When false, runner_shell calls are rejected with a clear error.
	AllowShell bool `yaml:"allow_shell"`

	// LogLevel controls verbosity: "debug" | "info" | "warn" | "error".
	// Defaults to "info".
	LogLevel string `yaml:"log_level"`

	// ReconnectMaxBackoff is the maximum number of seconds to wait between
	// reconnect attempts.  Defaults to 60.
	ReconnectMaxBackoff int `yaml:"reconnect_max_backoff"`

	// LogFile is an optional path to write logs to.
	// When empty, logs go to stdout (fine for terminals and Linux/macOS services).
	// Set automatically by install.ps1 on Windows since services have no stdout.
	LogFile string `yaml:"log_file"`

	// ConfigPath is the absolute path this config was loaded from. Not part of
	// the YAML file itself (yaml:"-") — set by Load() so the running process
	// can rewrite its own config later (e.g. the update_key command self-updates
	// runner_key in place without needing root, since the file is already
	// owned by the user this process runs as).
	ConfigPath string `yaml:"-"`
}

// Load reads the config from the given path, applying defaults for
// optional fields.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	cfg.applyDefaults()

	abs, err := filepath.Abs(path)
	if err == nil {
		cfg.ConfigPath = abs
	} else {
		cfg.ConfigPath = path
	}

	return &cfg, nil
}

// UpdateRunnerKey rewrites the runner_key line in the config file on disk to
// newKey, leaving every other line untouched, then updates the in-memory
// value. Used by the update_key command so a live runner can rotate its own
// key without any file permission issues — the process already owns this
// file (it was chown'd to the running user at install time).
func (c *Config) UpdateRunnerKey(newKey string) error {
	if c.ConfigPath == "" {
		return fmt.Errorf("config path is unknown; cannot self-update")
	}
	data, err := os.ReadFile(c.ConfigPath)
	if err != nil {
		return fmt.Errorf("reading config file %q: %w", c.ConfigPath, err)
	}

	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "runner_key:") {
			lines[i] = "runner_key:            " + newKey
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, "runner_key:            "+newKey)
	}

	out := strings.Join(lines, "\n")
	if err := os.WriteFile(c.ConfigPath, []byte(out), 0600); err != nil {
		return fmt.Errorf("writing config file %q: %w", c.ConfigPath, err)
	}

	c.RunnerKey = newKey
	return nil
}

// DefaultConfigPath returns the default location for the config file.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".vectrify-runner", "config.yaml")
}

// Platform returns the current OS identifier in the format expected by the API.
func Platform() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "darwin"
	default:
		return "linux"
	}
}

func (c *Config) validate() error {
	if c.APIURL == "" {
		return fmt.Errorf("api_url is required")
	}
	if c.RunnerKey == "" {
		return fmt.Errorf("runner_key is required")
	}
	if !isValidRunnerKey(c.RunnerKey) {
		return fmt.Errorf("runner_key must start with 'vrun_'")
	}
	if c.WorkspaceRoot == "" {
		return fmt.Errorf("workspace_root is required")
	}
	abs, err := filepath.Abs(c.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("workspace_root is not a valid path: %w", err)
	}
	c.WorkspaceRoot = abs
	return nil
}

func (c *Config) applyDefaults() {
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.ReconnectMaxBackoff <= 0 {
		c.ReconnectMaxBackoff = 60
	}
}

func isValidRunnerKey(k string) bool {
	return len(k) > 5 && k[:5] == "vrun_"
}
