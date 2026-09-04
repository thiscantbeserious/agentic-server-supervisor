package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// knownEnvVars is every C3 name Load reads. It lives in the test file, not
// in config.go, since its only consumer is clearKnownEnv below (test
// scaffolding does not belong in production code).
var knownEnvVars = []string{
	"TICK_INTERVAL", "TICK_WINDOW", "DEEP_WINDOW", "SECTION_TIMEOUT",
	"FACTS_MAX_BYTES", "SERVICES_MAX_BYTES", "STATE_DIR", "HOST_JOURNAL_DIR",
	"HOST_JOURNAL_VOLATILE_DIR", "HOST_PROC", "HOST_ROOT", "HOST_RASDAEMON",
	"SENTINEL_HOSTNAME", "AGY_BIN", "AGY_HOME", "AGY_SECRET_DIR",
	"AGY_PRINT_TIMEOUT", "AGY_HARD_TIMEOUT", "HISTORY_N", "PROMPT_MAX_BYTES", "HISTORY_KEEP",
	"DEEP_ENABLED", "DEEP_TIMEOUT", "RAW_ALERT_MAX_PRIORITY",
	"RAW_ALERT_MAX_LINES", "RAW_ALERT_REPEAT_SECONDS", "RAW_ALERT_MARKER_TTL_HOURS",
	"RENOTIFY_ALERT_SEC", "RENOTIFY_WATCH_SEC", "STALE_ALERT_SEC", "HEARTBEAT_HOUR",
	"OUTBOX_MAX", "OUTBOX_SMTP_AFTER", "APPRISE_URL", "APPRISE_KEY",
	"APPRISE_CONFIG_FILE", "NOTIFY_TIMEOUT", "NOTIFY_BODY_MAX", "MAILRISE_HOST",
	"MAILRISE_PORT", "MAILRISE_USER", "MAILRISE_PASS", "SENTINEL_MAIL_FROM",
	"SENTINEL_MAIL_TO", "LOG_LEVEL", "TMPDIR", "TZ", "SENTINEL_NOW",
}

// setBaseEnv clears the process env of every C3 var this package reads and
// leaves nothing set, so each test controls exactly the variables it cares
// about (t.Setenv already isolates per-test, but Load() falls back to
// whatever happens to be in the ambient environment otherwise).
func clearKnownEnv(t *testing.T) {
	t.Helper()
	for _, name := range knownEnvVars {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func TestLoad_Defaults(t *testing.T) {
	clearKnownEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.TickInterval != 300*time.Second {
		t.Errorf("TickInterval = %v, want 300s", cfg.TickInterval)
	}
	if cfg.TickWindow != 10*time.Minute {
		t.Errorf("TickWindow = %v, want 10m", cfg.TickWindow)
	}
	if cfg.DeepWindow != 24*time.Hour {
		t.Errorf("DeepWindow = %v, want 24h", cfg.DeepWindow)
	}
	if cfg.StateDir != "/state" {
		t.Errorf("StateDir = %q, want /state", cfg.StateDir)
	}
	if cfg.FactsMaxBytes != 262144 {
		t.Errorf("FactsMaxBytes = %d, want 262144", cfg.FactsMaxBytes)
	}
	if cfg.AgyPrintTimeout != 210*time.Second {
		t.Errorf("AgyPrintTimeout = %v, want 210s", cfg.AgyPrintTimeout)
	}
	if cfg.AgyHardTimeout != 240*time.Second {
		t.Errorf("AgyHardTimeout = %v, want 240s", cfg.AgyHardTimeout)
	}
	if cfg.RawAlertMaxLines != 20 {
		t.Errorf("RawAlertMaxLines = %d, want 20", cfg.RawAlertMaxLines)
	}
	if cfg.HeartbeatHour != 8 {
		t.Errorf("HeartbeatHour = %d, want 8", cfg.HeartbeatHour)
	}
	if cfg.MailrisePort != 8025 {
		t.Errorf("MailrisePort = %d, want 8025", cfg.MailrisePort)
	}
	if cfg.LogLevel != "INFO" {
		t.Errorf("LogLevel = %q, want INFO", cfg.LogLevel)
	}
	if cfg.TZ != "UTC" {
		t.Errorf("TZ = %q, want UTC", cfg.TZ)
	}
	if !cfg.DeepEnabled {
		t.Errorf("DeepEnabled = false, want true (default 1)")
	}
}

func TestLoad_TickIntervalRange(t *testing.T) {
	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		{"below minimum", "59", true},
		{"at minimum", "60", false},
		{"at maximum", "3600", false},
		{"above maximum", "3601", true},
		{"non-numeric", "soon", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearKnownEnv(t)
			t.Setenv("TICK_INTERVAL", tc.val)
			t.Setenv("TICK_WINDOW", "2h") // keep TICK_WINDOW > TICK_INTERVAL out of the way of this test
			_, err := Load()
			if tc.wantErr && err == nil {
				t.Fatalf("Load() expected an error for TICK_INTERVAL=%q", tc.val)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if tc.wantErr {
				assertNamesVar(t, err, "TICK_INTERVAL")
			}
		})
	}
}

func TestLoad_TickWindowMustExceedTickInterval(t *testing.T) {
	clearKnownEnv(t)
	t.Setenv("TICK_INTERVAL", "300")
	t.Setenv("TICK_WINDOW", "5m") // 300s, not > 300s
	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected an error when TICK_WINDOW <= TICK_INTERVAL")
	}
	assertNamesVar(t, err, "TICK_WINDOW")
}

// C3: "Every duration value is parsed by time.ParseDuration with no
// rewriting of the input." The old "min" alias silently mis-parsed
// "10mins" as 10 *milliseconds* (ReplaceAll("mins","m"+"ins")->"10ms"),
// a 60,000x error with no diagnostic. Durations must now use Go syntax
// ("10m") and anything else is a config error naming the variable.
func TestLoad_TickWindowRejectsNonGoDurationSyntax(t *testing.T) {
	cases := []string{"10min", "10mins", "1h30min"}
	for _, val := range cases {
		t.Run(val, func(t *testing.T) {
			clearKnownEnv(t)
			t.Setenv("TICK_INTERVAL", "60")
			t.Setenv("TICK_WINDOW", val)
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() expected an error for TICK_WINDOW=%q (not time.ParseDuration syntax)", val)
			}
			assertNamesVar(t, err, "TICK_WINDOW")
		})
	}
}

func TestLoad_RawAlertMaxLinesRange(t *testing.T) {
	cases := []struct {
		val     string
		wantErr bool
	}{
		{"0", true},
		{"1", false},
		{"20", false},
		{"21", true},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			clearKnownEnv(t)
			t.Setenv("RAW_ALERT_MAX_LINES", tc.val)
			_, err := Load()
			if tc.wantErr && err == nil {
				t.Fatalf("Load() expected an error for RAW_ALERT_MAX_LINES=%q", tc.val)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if tc.wantErr {
				assertNamesVar(t, err, "RAW_ALERT_MAX_LINES")
			}
		})
	}
}

func TestLoad_AgyHardTimeoutRaisedWhenLow(t *testing.T) {
	clearKnownEnv(t)
	t.Setenv("AGY_PRINT_TIMEOUT", "120s")
	t.Setenv("AGY_HARD_TIMEOUT", "10s") // lower than print+30s
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	want := 150 * time.Second // 120s + 30s
	if cfg.AgyHardTimeout != want {
		t.Errorf("AgyHardTimeout = %v, want %v (raised to print+30s)", cfg.AgyHardTimeout, want)
	}
}

func TestLoad_AgyHardTimeoutKeptWhenAlreadyHigh(t *testing.T) {
	clearKnownEnv(t)
	t.Setenv("AGY_PRINT_TIMEOUT", "60s")
	t.Setenv("AGY_HARD_TIMEOUT", "500s")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.AgyHardTimeout != 500*time.Second {
		t.Errorf("AgyHardTimeout = %v, want 500s (untouched)", cfg.AgyHardTimeout)
	}
}

func TestLoad_MalformedNumericVars(t *testing.T) {
	numericVars := []string{
		"SECTION_TIMEOUT", "FACTS_MAX_BYTES", "SERVICES_MAX_BYTES", "HISTORY_N",
		"PROMPT_MAX_BYTES", "HISTORY_KEEP", "RAW_ALERT_MAX_PRIORITY", "RAW_ALERT_REPEAT_SECONDS",
		"RAW_ALERT_MARKER_TTL_HOURS", "RENOTIFY_ALERT_SEC", "RENOTIFY_WATCH_SEC",
		"STALE_ALERT_SEC", "HEARTBEAT_HOUR", "OUTBOX_MAX", "OUTBOX_SMTP_AFTER",
		"NOTIFY_TIMEOUT", "NOTIFY_BODY_MAX", "MAILRISE_PORT",
	}
	for _, name := range numericVars {
		t.Run(name, func(t *testing.T) {
			clearKnownEnv(t)
			t.Setenv(name, "not-a-number")
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() expected an error for %s=not-a-number", name)
			}
			assertNamesVar(t, err, name)
		})
	}
}

func TestLoad_MalformedDurationVars(t *testing.T) {
	durationVars := []string{"TICK_WINDOW", "DEEP_WINDOW", "AGY_PRINT_TIMEOUT", "AGY_HARD_TIMEOUT", "DEEP_TIMEOUT"}
	for _, name := range durationVars {
		t.Run(name, func(t *testing.T) {
			clearKnownEnv(t)
			t.Setenv(name, "not-a-duration")
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() expected an error for %s=not-a-duration", name)
			}
			assertNamesVar(t, err, name)
		})
	}
}

// TestLoad_C3Ranges is the table from CONTRACTS.md §C3: TICK_INTERVAL
// 60-3600, RAW_ALERT_MAX_PRIORITY 0-7, RAW_ALERT_MAX_LINES 1-20,
// HEARTBEAT_HOUR 0-23, and every other numeric variable strictly > 0
// (durations > 0s). One out-of-range case per variable.
func TestLoad_C3Ranges(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		badVar string
	}{
		{"TICK_INTERVAL below range", map[string]string{"TICK_INTERVAL": "59", "TICK_WINDOW": "2h"}, "TICK_INTERVAL"},
		{"TICK_INTERVAL above range", map[string]string{"TICK_INTERVAL": "3601", "TICK_WINDOW": "2h"}, "TICK_INTERVAL"},
		{"RAW_ALERT_MAX_PRIORITY below range", map[string]string{"RAW_ALERT_MAX_PRIORITY": "-1"}, "RAW_ALERT_MAX_PRIORITY"},
		{"RAW_ALERT_MAX_PRIORITY above range", map[string]string{"RAW_ALERT_MAX_PRIORITY": "8"}, "RAW_ALERT_MAX_PRIORITY"},
		{"RAW_ALERT_MAX_LINES zero", map[string]string{"RAW_ALERT_MAX_LINES": "0"}, "RAW_ALERT_MAX_LINES"},
		{"RAW_ALERT_MAX_LINES above range", map[string]string{"RAW_ALERT_MAX_LINES": "21"}, "RAW_ALERT_MAX_LINES"},
		{"HEARTBEAT_HOUR below range", map[string]string{"HEARTBEAT_HOUR": "-1"}, "HEARTBEAT_HOUR"},
		{"HEARTBEAT_HOUR above range", map[string]string{"HEARTBEAT_HOUR": "24"}, "HEARTBEAT_HOUR"},
		{"SECTION_TIMEOUT zero", map[string]string{"SECTION_TIMEOUT": "0"}, "SECTION_TIMEOUT"},
		{"SECTION_TIMEOUT negative", map[string]string{"SECTION_TIMEOUT": "-5"}, "SECTION_TIMEOUT"},
		{"FACTS_MAX_BYTES zero", map[string]string{"FACTS_MAX_BYTES": "0"}, "FACTS_MAX_BYTES"},
		{"FACTS_MAX_BYTES negative", map[string]string{"FACTS_MAX_BYTES": "-1"}, "FACTS_MAX_BYTES"},
		{"SERVICES_MAX_BYTES zero", map[string]string{"SERVICES_MAX_BYTES": "0"}, "SERVICES_MAX_BYTES"},
		{"HISTORY_N zero", map[string]string{"HISTORY_N": "0"}, "HISTORY_N"},
		{"PROMPT_MAX_BYTES zero", map[string]string{"PROMPT_MAX_BYTES": "0"}, "PROMPT_MAX_BYTES"},
		{"HISTORY_KEEP negative", map[string]string{"HISTORY_KEEP": "-7"}, "HISTORY_KEEP"},
		{"RAW_ALERT_REPEAT_SECONDS zero", map[string]string{"RAW_ALERT_REPEAT_SECONDS": "0"}, "RAW_ALERT_REPEAT_SECONDS"},
		{"RAW_ALERT_MARKER_TTL_HOURS zero", map[string]string{"RAW_ALERT_MARKER_TTL_HOURS": "0"}, "RAW_ALERT_MARKER_TTL_HOURS"},
		{"RENOTIFY_ALERT_SEC zero", map[string]string{"RENOTIFY_ALERT_SEC": "0"}, "RENOTIFY_ALERT_SEC"},
		{"RENOTIFY_WATCH_SEC zero", map[string]string{"RENOTIFY_WATCH_SEC": "0"}, "RENOTIFY_WATCH_SEC"},
		{"STALE_ALERT_SEC zero", map[string]string{"STALE_ALERT_SEC": "0"}, "STALE_ALERT_SEC"},
		{"OUTBOX_MAX negative", map[string]string{"OUTBOX_MAX": "-3"}, "OUTBOX_MAX"},
		{"OUTBOX_SMTP_AFTER zero", map[string]string{"OUTBOX_SMTP_AFTER": "0"}, "OUTBOX_SMTP_AFTER"},
		{"NOTIFY_TIMEOUT zero", map[string]string{"NOTIFY_TIMEOUT": "0"}, "NOTIFY_TIMEOUT"},
		{"NOTIFY_BODY_MAX zero", map[string]string{"NOTIFY_BODY_MAX": "0"}, "NOTIFY_BODY_MAX"},
		{"MAILRISE_PORT zero", map[string]string{"MAILRISE_PORT": "0"}, "MAILRISE_PORT"},
		{"DEEP_WINDOW zero duration", map[string]string{"DEEP_WINDOW": "0s"}, "DEEP_WINDOW"},
		{"AGY_PRINT_TIMEOUT zero duration", map[string]string{"AGY_PRINT_TIMEOUT": "0s"}, "AGY_PRINT_TIMEOUT"},
		{"AGY_HARD_TIMEOUT negative duration", map[string]string{"AGY_HARD_TIMEOUT": "-5s"}, "AGY_HARD_TIMEOUT"},
		{"DEEP_TIMEOUT zero duration", map[string]string{"DEEP_TIMEOUT": "0s"}, "DEEP_TIMEOUT"},
		{"TICK_WINDOW zero duration", map[string]string{"TICK_WINDOW": "0s"}, "TICK_WINDOW"},
		{"RAW_ALERT_MARKER_TTL_HOURS above range", map[string]string{"RAW_ALERT_MARKER_TTL_HOURS": "8761"}, "RAW_ALERT_MARKER_TTL_HOURS"},
		// The overflow value that motivated the <=24h post-conversion bound
		// (C3): a seconds-valued var is multiplied by time.Second, so a huge
		// value overflows int64 nanoseconds into a NEGATIVE duration, a
		// timeout would fire instantly instead of erroring at Load().
		{"SECTION_TIMEOUT overflows int64 ns when converted", map[string]string{"SECTION_TIMEOUT": "99999999999"}, "SECTION_TIMEOUT"},
		{"NOTIFY_TIMEOUT overflows int64 ns when converted", map[string]string{"NOTIFY_TIMEOUT": "99999999999"}, "NOTIFY_TIMEOUT"},
		{"TICK_WINDOW duration above 24h", map[string]string{"TICK_WINDOW": "25h"}, "TICK_WINDOW"},
		{"DEEP_WINDOW duration above 24h", map[string]string{"DEEP_WINDOW": "25h"}, "DEEP_WINDOW"},
		{"AGY_PRINT_TIMEOUT duration above 24h", map[string]string{"AGY_PRINT_TIMEOUT": "25h"}, "AGY_PRINT_TIMEOUT"},
		{"DEEP_TIMEOUT duration above 24h", map[string]string{"DEEP_TIMEOUT": "25h"}, "DEEP_TIMEOUT"},
		// C3 (amended, commit c5cab9a): the 24h bound is on the variable,
		// not on the Go type Config stores it in, these four stay plain
		// int seconds in Config and only become durations in state/runtime,
		// but Load must still reject > 86400.
		{"RAW_ALERT_REPEAT_SECONDS above 86400", map[string]string{"RAW_ALERT_REPEAT_SECONDS": "86401"}, "RAW_ALERT_REPEAT_SECONDS"},
		{"RENOTIFY_ALERT_SEC above 86400", map[string]string{"RENOTIFY_ALERT_SEC": "86401"}, "RENOTIFY_ALERT_SEC"},
		{"RENOTIFY_WATCH_SEC above 86400", map[string]string{"RENOTIFY_WATCH_SEC": "999999"}, "RENOTIFY_WATCH_SEC"},
		{"STALE_ALERT_SEC above 86400", map[string]string{"STALE_ALERT_SEC": "86401"}, "STALE_ALERT_SEC"},
		{"JOURNAL_MAX_RECORDS zero", map[string]string{"JOURNAL_MAX_RECORDS": "0"}, "JOURNAL_MAX_RECORDS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearKnownEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatalf("Load() expected an error for %s", tc.name)
			}
			assertNamesVar(t, err, tc.badVar)
		})
	}
}

// TestLoad_C3RangesZeroIsLegalWhereARowSaysSo verifies the per-variable row
// wins over the catch-all ">0" row: RAW_ALERT_MAX_PRIORITY=0 (emerg, the
// most important raw-alert level) and HEARTBEAT_HOUR=0 (midnight) are both
// in-range zero values, and DEEP_ENABLED is the "0"|"1" boolean, not a
// numeric variable the catch-all row applies to at all.
func TestLoad_C3RangesZeroIsLegalWhereARowSaysSo(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{"RAW_ALERT_MAX_PRIORITY=0 (emerg)", "RAW_ALERT_MAX_PRIORITY", "0"},
		{"HEARTBEAT_HOUR=0 (midnight)", "HEARTBEAT_HOUR", "0"},
		{"DEEP_ENABLED=0 (disabled)", "DEEP_ENABLED", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearKnownEnv(t)
			t.Setenv(tc.env, tc.val)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() unexpected error for %s=%s: %v", tc.env, tc.val, err)
			}
			switch tc.env {
			case "RAW_ALERT_MAX_PRIORITY":
				if cfg.RawAlertMaxPriority != 0 {
					t.Errorf("RawAlertMaxPriority = %d, want 0", cfg.RawAlertMaxPriority)
				}
			case "HEARTBEAT_HOUR":
				if cfg.HeartbeatHour != 0 {
					t.Errorf("HeartbeatHour = %d, want 0", cfg.HeartbeatHour)
				}
			case "DEEP_ENABLED":
				if cfg.DeepEnabled {
					t.Errorf("DeepEnabled = true, want false (DEEP_ENABLED=0)")
				}
			}
		})
	}
}

// C7: "On an env error print the variable name only." Verify no config
// error leaks the offending value.
func TestLoad_ErrorNamesVariableOnly(t *testing.T) {
	clearKnownEnv(t)
	t.Setenv("SECTION_TIMEOUT", "-999999")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected an error")
	}
	if strings.Contains(err.Error(), "999999") {
		t.Fatalf("error must name the variable only, not the value: %v", err)
	}
	assertNamesVar(t, err, "SECTION_TIMEOUT")
}

func TestLoad_DeepEnabledMustBeZeroOrOne(t *testing.T) {
	clearKnownEnv(t)
	t.Setenv("DEEP_ENABLED", "yes")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected an error for DEEP_ENABLED=yes")
	}
	assertNamesVar(t, err, "DEEP_ENABLED")
}

func TestLoad_LogLevelEnum(t *testing.T) {
	clearKnownEnv(t)
	t.Setenv("LOG_LEVEL", "VERBOSE")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected an error for LOG_LEVEL=VERBOSE")
	}
	assertNamesVar(t, err, "LOG_LEVEL")
}

func TestLoad_TZInvalid(t *testing.T) {
	clearKnownEnv(t)
	t.Setenv("TZ", "Not/A_Zone")
	_, err := Load()
	if err == nil {
		t.Fatal("Load() expected an error for an invalid TZ")
	}
	assertNamesVar(t, err, "TZ")
}

func TestLoad_SentinelNowClock(t *testing.T) {
	clearKnownEnv(t)
	t.Setenv("SENTINEL_NOW", "2026-08-15T09:35:02Z")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	want := time.Date(2026, 8, 15, 9, 35, 2, 0, time.UTC)
	if !cfg.Now.Equal(want) {
		t.Errorf("Now = %v, want %v", cfg.Now, want)
	}
}

// C3 calls SENTINEL_NOW a test-only override and contracts/state.md §S.2 spells
// out the consumer rule: "Now (from SENTINEL_NOW, test-only; zero => time.Now())".
// Load() must therefore leave Now as the zero Time when the variable is unset.
// Stamping time.Now() here would be indistinguishable from a real override, and
// Config is loaded ONCE per process: in `tick --loop` every later tick would
// reuse the container's start time, so history filenames, stale-alert pruning,
// re-notify windows and the daily heartbeat would all freeze forever.
func TestLoad_SentinelNowUnsetLeavesNowZero(t *testing.T) {
	clearKnownEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if !cfg.Now.IsZero() {
		t.Errorf("Now = %v, want the zero Time so consumers fall back to the live clock", cfg.Now)
	}
}

func TestResolveHostname(t *testing.T) {
	t.Run("SENTINEL_HOSTNAME wins", func(t *testing.T) {
		clearKnownEnv(t)
		t.Setenv("SENTINEL_HOSTNAME", "explicit-host")
		hostRoot := t.TempDir()
		writeFile(t, filepath.Join(hostRoot, "etc/hostname"), "root-host\n")
		t.Setenv("HOST_ROOT", hostRoot)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.Hostname != "explicit-host" {
			t.Errorf("Hostname = %q, want explicit-host", cfg.Hostname)
		}
	})

	t.Run("falls back to HOST_ROOT/etc/hostname", func(t *testing.T) {
		clearKnownEnv(t)
		hostRoot := t.TempDir()
		writeFile(t, filepath.Join(hostRoot, "etc/hostname"), "root-host\n")
		t.Setenv("HOST_ROOT", hostRoot)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.Hostname != "root-host" {
			t.Errorf("Hostname = %q, want root-host", cfg.Hostname)
		}
	})

	t.Run("falls back to HOST_PROC/sys/kernel/hostname", func(t *testing.T) {
		clearKnownEnv(t)
		hostRoot := t.TempDir() // no etc/hostname inside
		hostProc := t.TempDir()
		writeFile(t, filepath.Join(hostProc, "sys/kernel/hostname"), "proc-host\n")
		t.Setenv("HOST_ROOT", hostRoot)
		t.Setenv("HOST_PROC", hostProc)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.Hostname != "proc-host" {
			t.Errorf("Hostname = %q, want proc-host", cfg.Hostname)
		}
	})

	t.Run("falls back to unknown", func(t *testing.T) {
		clearKnownEnv(t)
		t.Setenv("HOST_ROOT", t.TempDir())
		t.Setenv("HOST_PROC", t.TempDir())
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.Hostname != "unknown" {
			t.Errorf("Hostname = %q, want unknown", cfg.Hostname)
		}
	})
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertNamesVar(t *testing.T, err error, name string) {
	t.Helper()
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("error %v is not *config.Error", err)
	}
	if cerr.Var != name {
		t.Fatalf("error names variable %q, want %q", cerr.Var, name)
	}
}
