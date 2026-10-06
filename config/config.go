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

// Default values for the dispatch concurrency tunables below. Kept as the
// applyDefaults() fallback so behavior is byte-for-byte unchanged from the
// old compile-time consts when the corresponding YAML keys are absent —
// tuning these no longer requires a rebuild, but an untouched config.yaml
// must keep behaving exactly as before.
const (
	defaultMaxConcurrency            = 32
	defaultMaxHeavyConcurrency       = 24
	defaultSlotAcquireTimeoutSeconds = 3

	// Auto-update tunables (see the updater package). Both default to 5 minutes.
	defaultUpdateCheckIntervalSeconds = 300
	defaultUpdateIdleSeconds          = 300

	// minUpdateCheckIntervalSeconds floors update_check_interval_seconds. The
	// check is a cheap redirect, not an API call, but a typo of "1" should not
	// make every runner hit github.com once a second.
	minUpdateCheckIntervalSeconds = 30
	// maxSaneUpdateSeconds caps both update knobs at one day: a larger value is
	// almost certainly a typo and would effectively disable auto-update.
	maxSaneUpdateSeconds = 86400

	// maxSaneConcurrency is a sanity ceiling on max_concurrency, clamped
	// (with a warning) rather than honored verbatim. Without it, a typo'd
	// config value (e.g. an extra zero: "3200000") would silently remove
	// the entire goroutine-growth ceiling this mechanism exists to enforce,
	// and — since deriveDefaultHeavyConcurrency and downstream capacity
	// arithmetic scale with MaxConcurrency — risk integer overflow or
	// pathological resource use on an absurd input. 1000 is far beyond any
	// realistic customer machine's ability to usefully run that many
	// concurrent shell/file operations, while comfortably above the 32
	// default for any legitimate tuning need.
	maxSaneConcurrency = 1000

	// maxSaneSlotAcquireTimeoutSeconds is a sanity ceiling on
	// slot_acquire_timeout_seconds, clamped (with a warning) rather than
	// honored verbatim. Every dispatch hand-off goroutine that ends up
	// waiting holds one of the (also bounded, but separate)
	// maxPendingSlotWaiters slots for up to this long — a typo'd huge value
	// (e.g. "100000" seconds, ~28h) would let those waiter slots — and
	// therefore Drain(), which also waits on pendingWaiterCount() — get
	// stuck for an unreasonable time instead of the "few seconds" this knob
	// is meant to tune. 300s (5 minutes) is far beyond any legitimate
	// burst-absorption need while still self-correcting an obviously wrong
	// value instead of refusing to boot.
	maxSaneSlotAcquireTimeoutSeconds = 300

	// defaultMaxBrowserSessions is the default size of the browser session
	// pool (see BrowserManager in executor/browser.go). Each session holds
	// one Chromium BrowserContext + Page alive in memory for as long as it
	// is open, so this is intentionally small — a customer machine is not
	// expected to run many concurrent agent-driven browsers at once, and a
	// runaway number of sessions would multiply the ~300MB Chromium
	// footprint per instance.
	defaultMaxBrowserSessions = 3

	// maxSaneBrowserSessions is a sanity ceiling on max_browser_sessions,
	// clamped (with a warning) rather than honored verbatim, for the same
	// reason as maxSaneConcurrency above: a typo'd config value should not
	// silently let the runner spawn an unbounded number of real browser
	// processes/contexts.
	maxSaneBrowserSessions = 20

	// defaultBrowserIdleTimeoutSeconds is how long an opened browser session
	// may sit unused before the idle reaper closes it automatically. Exists
	// so a forgotten `launch` (no matching `close`) does not hold a real
	// Chromium process + memory open forever.
	defaultBrowserIdleTimeoutSeconds = 300

	// maxSaneBrowserIdleTimeoutSeconds bounds browser_idle_timeout_seconds;
	// same self-correction rationale as the other maxSane* constants.
	maxSaneBrowserIdleTimeoutSeconds = 3600

	// defaultMaxBackgroundProcesses is the default size of the background
	// process pool (see ProcessManager in executor/process.go). Small for
	// the same reason as defaultMaxBrowserSessions — a customer machine is
	// not expected to run many concurrent agent-started dev servers/long-
	// lived commands at once.
	defaultMaxBackgroundProcesses = 5

	// maxSaneBackgroundProcesses is a sanity ceiling on
	// max_background_processes, clamped (with a warning) rather than
	// honored verbatim, same rationale as maxSaneBrowserSessions.
	maxSaneBackgroundProcesses = 20

	// defaultBackgroundProcessMaxAgeSeconds is how long a background
	// process (or an exited-but-never-retrieved entry for one) may exist
	// before the reaper force-stops/cleans it up. Deliberately much longer
	// than defaultBrowserIdleTimeoutSeconds: a background process is
	// typically something like a dev server meant to stay up for an
	// entire multi-step agent workflow (start it, drive it with several
	// browser actions spread over minutes, then stop it explicitly) —
	// this is a safety net against a genuinely forgotten process, not a
	// tight idle timeout. Exists so a forgotten `start` (no matching
	// `stop`) does not hold a real OS process open forever.
	defaultBackgroundProcessMaxAgeSeconds = 3600

	// maxSaneBackgroundProcessMaxAgeSeconds bounds
	// background_process_max_age_seconds; same self-correction rationale
	// as the other maxSane* constants. 24h comfortably covers any
	// legitimate long-running dev-server-driving workflow while still
	// self-correcting an obviously wrong value (e.g. a typo'd extra zero)
	// instead of letting a background process run essentially forever.
	maxSaneBackgroundProcessMaxAgeSeconds = 86400
)

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
	// Defaults to true when the key is absent (see Load); set
	// allow_shell: false to opt out. When false, runner_shell calls are
	// rejected with a clear error.
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

	// MaxConcurrency is the maximum number of command goroutines that may run
	// simultaneously, across all command classes. Prevents unbounded goroutine
	// growth if the API sends a burst of commands (e.g. a server-side bug or
	// replay). Defaults to 32.
	MaxConcurrency int `yaml:"max_concurrency"`

	// MaxHeavyConcurrency is the sub-limit for "heavy" commands (shell,
	// file_transfer) — see client.classifyCommand. Reserving MaxConcurrency -
	// MaxHeavyConcurrency slots for "light" commands (file_op, git,
	// update_key, and any unknown type) means a burst of slow/stuck shells
	// can never starve the fast editor-style commands of every slot, which is
	// the exact failure mode a production incident produced. Must be
	// strictly less than MaxConcurrency so at least one slot always stays
	// reserved for light commands; applyDefaults() enforces this by
	// deriving a value when unset (see defaultHeavyConcurrency) or clamping
	// (with a warning) an explicit value that is too high. Defaults to 24
	// when MaxConcurrency is left at its own default of 32.
	MaxHeavyConcurrency int `yaml:"max_heavy_concurrency"`

	// SlotAcquireTimeoutSeconds is how long a dispatch may wait for a free
	// concurrency slot before being rejected as "runner busy", instead of
	// being rejected instantly the moment the limit is hit. This absorbs the
	// normal, brief oversubscription that happens when sub-agents fan out
	// several tool calls at once. Defaults to 3.
	SlotAcquireTimeoutSeconds int `yaml:"slot_acquire_timeout_seconds"`

	// UpdateCheckIntervalSeconds is how often the runner looks for a new
	// release. Defaults to 300 (5 minutes); minimum 30, maximum 86400.
	UpdateCheckIntervalSeconds int `yaml:"update_check_interval_seconds"`

	// UpdateIdleSeconds is how long the runner must be quiet (no command in
	// flight, none received or finished) before it will apply a found update,
	// so an update never interrupts or lands right after someone using the
	// runner. Defaults to 300 (5 minutes); maximum 86400. A freshly started
	// runner counts as active, so it never updates in its first idle window.
	UpdateIdleSeconds int `yaml:"update_idle_seconds"`

	// MaxBrowserSessions caps how many browser sessions (each one Chromium
	// BrowserContext + Page kept alive across multiple "browser" commands)
	// may be open at the same time. Defaults to 3.
	MaxBrowserSessions int `yaml:"max_browser_sessions"`

	// BrowserIdleTimeoutSeconds is how long an open browser session may sit
	// unused (no browser command referencing its session_id) before it is
	// automatically closed by the idle reaper. Defaults to 300 (5 minutes).
	BrowserIdleTimeoutSeconds int `yaml:"browser_idle_timeout_seconds"`

	// BrowserHeadless controls whether launched browsers run headless
	// (no visible window) or headed. Defaults to true. Set false only for
	// local debugging on a machine with a display.
	BrowserHeadless *bool `yaml:"browser_headless"`

	// MaxBackgroundProcesses caps how many long-lived, detached background
	// processes (see executor/process.go's ProcessManager — the "process"
	// command type: start/stop/list/logs) may be tracked at the same time.
	// Defaults to 5.
	MaxBackgroundProcesses int `yaml:"max_background_processes"`

	// BackgroundProcessMaxAgeSeconds is how long a background process (or
	// an exited-but-never-retrieved entry for one) may exist before the
	// reaper force-stops/cleans it up. Defaults to 3600 (1 hour).
	BackgroundProcessMaxAgeSeconds int `yaml:"background_process_max_age_seconds"`

	// ConfigPath is the absolute path this config was loaded from. Not part of
	// the YAML file itself (yaml:"-") — set by Load() so the running process
	// can rewrite its own config later (e.g. the update_key command self-updates
	// runner_key in place without needing root, since the file is already
	// owned by the user this process runs as).
	ConfigPath string `yaml:"-"`

	// Warnings accumulates human-readable messages produced while applying
	// defaults/validation (e.g. a clamped max_heavy_concurrency). Not part of
	// the YAML file (yaml:"-"). Load() runs before the logger is constructed
	// in main.go, so it cannot log directly; main.go logs each entry via
	// slog once the logger exists instead of silently self-correcting.
	Warnings []string `yaml:"-"`
}

// Load reads the config from the given path, applying defaults for
// optional fields.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}

	// allow_shell defaults to true: seed it before unmarshalling so an absent
	// key stays true while an explicit allow_shell: false still disables it.
	cfg := Config{AllowShell: true}
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
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = defaultMaxConcurrency
	} else if c.MaxConcurrency > maxSaneConcurrency {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"max_concurrency (%d) exceeds the sanity ceiling of %d; clamping max_concurrency to %d",
			c.MaxConcurrency, maxSaneConcurrency, maxSaneConcurrency,
		))
		c.MaxConcurrency = maxSaneConcurrency
	}
	if c.UpdateCheckIntervalSeconds <= 0 {
		c.UpdateCheckIntervalSeconds = defaultUpdateCheckIntervalSeconds
	} else if c.UpdateCheckIntervalSeconds < minUpdateCheckIntervalSeconds {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"update_check_interval_seconds (%d) is below the minimum of %d; raising update_check_interval_seconds to %d",
			c.UpdateCheckIntervalSeconds, minUpdateCheckIntervalSeconds, minUpdateCheckIntervalSeconds,
		))
		c.UpdateCheckIntervalSeconds = minUpdateCheckIntervalSeconds
	} else if c.UpdateCheckIntervalSeconds > maxSaneUpdateSeconds {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"update_check_interval_seconds (%d) exceeds the sanity ceiling of %d; clamping update_check_interval_seconds to %d",
			c.UpdateCheckIntervalSeconds, maxSaneUpdateSeconds, maxSaneUpdateSeconds,
		))
		c.UpdateCheckIntervalSeconds = maxSaneUpdateSeconds
	}
	if c.UpdateIdleSeconds <= 0 {
		c.UpdateIdleSeconds = defaultUpdateIdleSeconds
	} else if c.UpdateIdleSeconds > maxSaneUpdateSeconds {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"update_idle_seconds (%d) exceeds the sanity ceiling of %d; clamping update_idle_seconds to %d",
			c.UpdateIdleSeconds, maxSaneUpdateSeconds, maxSaneUpdateSeconds,
		))
		c.UpdateIdleSeconds = maxSaneUpdateSeconds
	}
	if c.SlotAcquireTimeoutSeconds <= 0 {
		c.SlotAcquireTimeoutSeconds = defaultSlotAcquireTimeoutSeconds
	} else if c.SlotAcquireTimeoutSeconds > maxSaneSlotAcquireTimeoutSeconds {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"slot_acquire_timeout_seconds (%d) exceeds the sanity ceiling of %d; clamping slot_acquire_timeout_seconds to %d",
			c.SlotAcquireTimeoutSeconds, maxSaneSlotAcquireTimeoutSeconds, maxSaneSlotAcquireTimeoutSeconds,
		))
		c.SlotAcquireTimeoutSeconds = maxSaneSlotAcquireTimeoutSeconds
	}

	// MaxHeavyConcurrency has two distinct self-correction paths, and which
	// one applies matters: deriving from a *customized* MaxConcurrency is
	// not the same as clamping an *explicitly bad* MaxHeavyConcurrency.
	//
	//   - Unset (<=0): derive proportionally from the effective
	//     MaxConcurrency rather than falling back to the flat
	//     defaultMaxHeavyConcurrency (24) unconditionally. If we used the
	//     flat default here, a user who only customizes max_concurrency
	//     (e.g. to 4, on a resource-constrained machine) would get
	//     heavy=24, silently clamped to 4 below — leaving ZERO slots
	//     reserved for light commands, exactly the starvation this whole
	//     mechanism exists to prevent, and with a warning that confusingly
	//     names a key the user never set.
	//   - Explicitly set >= MaxConcurrency: clamp to MaxConcurrency-1 (never
	//     equal to MaxConcurrency — heavy must always leave at least one
	//     slot reserved for light commands) and warn, since this is a
	//     genuine misconfiguration the user should know about.
	if c.MaxHeavyConcurrency <= 0 {
		c.MaxHeavyConcurrency = deriveDefaultHeavyConcurrency(c.MaxConcurrency)
	} else if c.MaxHeavyConcurrency >= c.MaxConcurrency {
		clamped := c.MaxConcurrency - 1
		if clamped < 1 {
			// Degenerate case: MaxConcurrency itself is 1, so there is no
			// slot left to reserve for light commands no matter what we do.
			// A runner that refuses to boot over a bad tuning value is
			// worse than one that self-corrects — clamp rather than fail
			// Load(); main.go logs this Warnings entry once the logger
			// exists (Load() runs before it does).
			clamped = c.MaxConcurrency
		}
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"max_heavy_concurrency (%d) must be less than max_concurrency (%d) so at least one slot stays reserved for light commands; clamping max_heavy_concurrency to %d",
			c.MaxHeavyConcurrency, c.MaxConcurrency, clamped,
		))
		c.MaxHeavyConcurrency = clamped
	}

	if c.MaxBrowserSessions <= 0 {
		c.MaxBrowserSessions = defaultMaxBrowserSessions
	} else if c.MaxBrowserSessions > maxSaneBrowserSessions {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"max_browser_sessions (%d) exceeds the sanity ceiling of %d; clamping max_browser_sessions to %d",
			c.MaxBrowserSessions, maxSaneBrowserSessions, maxSaneBrowserSessions,
		))
		c.MaxBrowserSessions = maxSaneBrowserSessions
	}

	if c.BrowserIdleTimeoutSeconds <= 0 {
		c.BrowserIdleTimeoutSeconds = defaultBrowserIdleTimeoutSeconds
	} else if c.BrowserIdleTimeoutSeconds > maxSaneBrowserIdleTimeoutSeconds {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"browser_idle_timeout_seconds (%d) exceeds the sanity ceiling of %d; clamping browser_idle_timeout_seconds to %d",
			c.BrowserIdleTimeoutSeconds, maxSaneBrowserIdleTimeoutSeconds, maxSaneBrowserIdleTimeoutSeconds,
		))
		c.BrowserIdleTimeoutSeconds = maxSaneBrowserIdleTimeoutSeconds
	}
	if c.BrowserHeadless == nil {
		defaultHeadless := true
		c.BrowserHeadless = &defaultHeadless
	}

	if c.MaxBackgroundProcesses <= 0 {
		c.MaxBackgroundProcesses = defaultMaxBackgroundProcesses
	} else if c.MaxBackgroundProcesses > maxSaneBackgroundProcesses {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"max_background_processes (%d) exceeds the sanity ceiling of %d; clamping max_background_processes to %d",
			c.MaxBackgroundProcesses, maxSaneBackgroundProcesses, maxSaneBackgroundProcesses,
		))
		c.MaxBackgroundProcesses = maxSaneBackgroundProcesses
	}

	if c.BackgroundProcessMaxAgeSeconds <= 0 {
		c.BackgroundProcessMaxAgeSeconds = defaultBackgroundProcessMaxAgeSeconds
	} else if c.BackgroundProcessMaxAgeSeconds > maxSaneBackgroundProcessMaxAgeSeconds {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"background_process_max_age_seconds (%d) exceeds the sanity ceiling of %d; clamping background_process_max_age_seconds to %d",
			c.BackgroundProcessMaxAgeSeconds, maxSaneBackgroundProcessMaxAgeSeconds, maxSaneBackgroundProcessMaxAgeSeconds,
		))
		c.BackgroundProcessMaxAgeSeconds = maxSaneBackgroundProcessMaxAgeSeconds
	}
}

// IsBrowserHeadless returns the effective headless setting, defaulting to
// true. Exposed as a method (rather than requiring every caller to dereference
// the *bool) because applyDefaults() always populates BrowserHeadless before
// Load() returns, but tests or callers constructing a Config by hand (not via
// Load) might leave it nil.
func (c *Config) IsBrowserHeadless() bool {
	if c.BrowserHeadless == nil {
		return true
	}
	return *c.BrowserHeadless
}

// deriveDefaultHeavyConcurrency computes the MaxHeavyConcurrency to use when
// the config left it unset, scaling proportionally with a customized
// MaxConcurrency using the same ratio as the historical defaults
// (defaultMaxHeavyConcurrency / defaultMaxConcurrency = 24/32 = 0.75) rather
// than a flat constant. For the unmodified default (global=32) this returns
// exactly defaultMaxHeavyConcurrency (24), preserving prior behavior
// byte-for-byte. The result is always in [1, global-1] except in the
// degenerate global==1 case, where global-1 would be 0 — heavy is clamped
// up to 1 there since a positive sub-limit is required for any shell/
// file_transfer command to ever run at all, at the cost of not reserving
// any slot for light commands (unavoidable with only one total slot).
func deriveDefaultHeavyConcurrency(global int) int {
	derived := global * defaultMaxHeavyConcurrency / defaultMaxConcurrency
	if derived >= global {
		derived = global - 1
	}
	if derived < 1 {
		derived = 1
	}
	return derived
}

func isValidRunnerKey(k string) bool {
	return len(k) > 5 && k[:5] == "vrun_"
}
