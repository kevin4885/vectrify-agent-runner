package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestConfig writes yaml content to a temp file and returns its path.
func writeTestConfig(t *testing.T, yamlBody string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yamlBody), 0600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

const minimalValidConfig = `
api_url: wss://api.example.com/ws
runner_key: vrun_abc123
workspace_root: .
`

// 7. Defaults applied when the new keys are absent — behavior must be
// byte-for-byte identical to the old hardcoded consts.
func TestLoad_DefaultsAppliedWhenAbsent(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MaxConcurrency != defaultMaxConcurrency {
		t.Errorf("MaxConcurrency = %d, want default %d", cfg.MaxConcurrency, defaultMaxConcurrency)
	}
	if cfg.MaxHeavyConcurrency != defaultMaxHeavyConcurrency {
		t.Errorf("MaxHeavyConcurrency = %d, want default %d", cfg.MaxHeavyConcurrency, defaultMaxHeavyConcurrency)
	}
	if cfg.SlotAcquireTimeoutSeconds != defaultSlotAcquireTimeoutSeconds {
		t.Errorf("SlotAcquireTimeoutSeconds = %d, want default %d", cfg.SlotAcquireTimeoutSeconds, defaultSlotAcquireTimeoutSeconds)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none for a config with no explicit tunables", cfg.Warnings)
	}
}

// Explicit values within valid ranges are honored as-is, no clamping.
func TestLoad_ExplicitValuesHonored(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig+`
max_concurrency: 10
max_heavy_concurrency: 5
slot_acquire_timeout_seconds: 7
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MaxConcurrency != 10 {
		t.Errorf("MaxConcurrency = %d, want 10", cfg.MaxConcurrency)
	}
	if cfg.MaxHeavyConcurrency != 5 {
		t.Errorf("MaxHeavyConcurrency = %d, want 5", cfg.MaxHeavyConcurrency)
	}
	if cfg.SlotAcquireTimeoutSeconds != 7 {
		t.Errorf("SlotAcquireTimeoutSeconds = %d, want 7", cfg.SlotAcquireTimeoutSeconds)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", cfg.Warnings)
	}
}

// max_heavy_concurrency > max_concurrency must be clamped (not rejected),
// with a warning recorded rather than failing to boot. Clamped to
// MaxConcurrency-1, never to MaxConcurrency itself — heavy must always
// leave at least one slot reserved for light commands.
func TestLoad_HeavyExceedsGlobal_ClampsWithWarning(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig+`
max_concurrency: 10
max_heavy_concurrency: 50
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want no error (self-correct, not fail)", err)
	}
	if cfg.MaxHeavyConcurrency != cfg.MaxConcurrency-1 {
		t.Fatalf("MaxHeavyConcurrency = %d, want clamped to MaxConcurrency-1 (%d)", cfg.MaxHeavyConcurrency, cfg.MaxConcurrency-1)
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly 1 clamp warning", cfg.Warnings)
	}
	if !strings.Contains(cfg.Warnings[0], "max_heavy_concurrency") || !strings.Contains(cfg.Warnings[0], "clamping") {
		t.Fatalf("Warnings[0] = %q, missing expected clamp explanation", cfg.Warnings[0])
	}
}

// max_heavy_concurrency exactly equal to max_concurrency must also be
// clamped (down to max_concurrency-1) — heavy is never allowed to equal the
// global limit, which would leave zero slots for light commands.
func TestLoad_HeavyEqualsGlobal_ClampsWithWarning(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig+`
max_concurrency: 10
max_heavy_concurrency: 10
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MaxHeavyConcurrency != 9 {
		t.Fatalf("MaxHeavyConcurrency = %d, want 9 (clamped below global)", cfg.MaxHeavyConcurrency)
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly 1 clamp warning", cfg.Warnings)
	}
}

// The default-derivation path (max_heavy_concurrency left unset) must not
// silently produce zero headroom for light commands when max_concurrency is
// customized to a small value — this is the anti-starvation guarantee
// extended to the *config defaulting* logic, not just the runtime admission
// path.
func TestLoad_HeavyUnset_DerivedFromCustomGlobal_LeavesHeadroom(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig+`
max_concurrency: 4
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MaxHeavyConcurrency <= 0 {
		t.Fatalf("MaxHeavyConcurrency = %d, want positive", cfg.MaxHeavyConcurrency)
	}
	if cfg.MaxHeavyConcurrency >= cfg.MaxConcurrency {
		t.Fatalf("MaxHeavyConcurrency = %d, MaxConcurrency = %d — light commands have zero reserved headroom", cfg.MaxHeavyConcurrency, cfg.MaxConcurrency)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none — deriving from a customized (but valid) max_concurrency is not a misconfiguration", cfg.Warnings)
	}
}

// Degenerate max_concurrency=1: no anti-starvation headroom is possible
// (there is only one slot, period), but Load() must still not fail and
// must still produce a usable positive heavy limit.
func TestLoad_HeavyUnset_DegenerateGlobalOfOne(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig+`
max_concurrency: 1
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MaxHeavyConcurrency != 1 {
		t.Fatalf("MaxHeavyConcurrency = %d, want 1 for the degenerate global=1 case", cfg.MaxHeavyConcurrency)
	}
}

// Zero and negative tunables must fall back to defaults rather than
// producing a zero/negative effective limit (which would wedge every
// dispatch behind a semaphore that can never be acquired).
func TestLoad_ZeroOrNegativeTunables_FallBackToDefaults(t *testing.T) {
	cases := []string{
		"max_concurrency: 0",
		"max_concurrency: -5",
		"max_heavy_concurrency: 0",
		"max_heavy_concurrency: -1",
		"slot_acquire_timeout_seconds: 0",
		"slot_acquire_timeout_seconds: -3",
	}
	for _, extra := range cases {
		t.Run(extra, func(t *testing.T) {
			path := writeTestConfig(t, minimalValidConfig+"\n"+extra+"\n")
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.MaxConcurrency <= 0 {
				t.Errorf("MaxConcurrency = %d, want positive default", cfg.MaxConcurrency)
			}
			if cfg.MaxHeavyConcurrency <= 0 {
				t.Errorf("MaxHeavyConcurrency = %d, want positive default", cfg.MaxHeavyConcurrency)
			}
			if cfg.SlotAcquireTimeoutSeconds <= 0 {
				t.Errorf("SlotAcquireTimeoutSeconds = %d, want positive default", cfg.SlotAcquireTimeoutSeconds)
			}
		})
	}
}

// Sanity: existing required-field validation still fails loudly (this is
// NOT part of the self-correcting tunables — a missing api_url is a real
// misconfiguration, not a tuning nit).

// Degenerate explicit-value clamp: max_concurrency=1 leaves no room to
// clamp max_heavy_concurrency below it (1-1=0 is not a usable sub-limit),
// so the explicit-value clamp path must fall back to MaxConcurrency itself
// (1) rather than 0, while still warning about the misconfiguration.
func TestLoad_HeavyExceedsGlobal_DegenerateGlobalOfOne(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig+`
max_concurrency: 1
max_heavy_concurrency: 5
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MaxHeavyConcurrency != 1 {
		t.Fatalf("MaxHeavyConcurrency = %d, want 1 for the degenerate global=1 explicit-clamp case", cfg.MaxHeavyConcurrency)
	}
	if len(cfg.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly 1 clamp warning", cfg.Warnings)
	}
}

// max_concurrency above the sanity ceiling must be clamped down (with a
// warning), not honored verbatim — a typo'd value must not silently remove
// the goroutine-growth ceiling this mechanism exists to enforce.
func TestLoad_MaxConcurrency_AboveSaneCeiling_Clamps(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig+`
max_concurrency: 5000000
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MaxConcurrency != maxSaneConcurrency {
		t.Fatalf("MaxConcurrency = %d, want clamped to %d", cfg.MaxConcurrency, maxSaneConcurrency)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("Warnings is empty, want a warning about the max_concurrency clamp")
	}
}

// slot_acquire_timeout_seconds above the sanity ceiling must be clamped
// down (with a warning) — an unreasonably large value would let dispatch
// hand-off goroutines (and Drain, which also waits on pendingWaiterCount())
// stay parked far longer than this knob is meant to tune.
func TestLoad_SlotAcquireTimeoutSeconds_AboveSaneCeiling_Clamps(t *testing.T) {
	path := writeTestConfig(t, minimalValidConfig+`
slot_acquire_timeout_seconds: 100000
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SlotAcquireTimeoutSeconds != maxSaneSlotAcquireTimeoutSeconds {
		t.Fatalf("SlotAcquireTimeoutSeconds = %d, want clamped to %d", cfg.SlotAcquireTimeoutSeconds, maxSaneSlotAcquireTimeoutSeconds)
	}
	if len(cfg.Warnings) == 0 {
		t.Fatal("Warnings is empty, want a warning about the slot_acquire_timeout_seconds clamp")
	}
}

func TestLoad_MissingRequiredField_StillErrors(t *testing.T) {
	path := writeTestConfig(t, `
runner_key: vrun_abc123
workspace_root: .
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load() with missing api_url = nil error, want error")
	}
}
