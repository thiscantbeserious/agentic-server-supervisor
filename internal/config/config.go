// Package config is the single env loader (C1, C3). Load() reads every
// environment variable listed in CONTRACTS.md §C3 once; a malformed,
// out-of-range, or non-numeric-where-numeric value returns an *Error
// naming the variable, cmd/sentinel maps that to exit 78. Downstream
// packages receive *Config, never call os.Getenv themselves.
package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	// A slim runtime image ships no system zoneinfo; without this the
	// time.LoadLocation(cfg.TZ) call below (and any future binary that
	// links config without cmd/sentinel/main.go, which already imports
	// this) would fail on a fresh container.
	_ "time/tzdata"
)

// Error is a configuration error naming the offending variable (C3: "exit
// 78 naming the variable"; C7: never print the value, only the name, the
// Reason describes the constraint, never the offending input).
type Error struct {
	Var    string
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Var, e.Reason)
}

func errf(name, format string, args ...any) *Error {
	return &Error{Var: name, Reason: fmt.Sprintf(format, args...)}
}

// noMax marks a range as having no documented upper bound (C3: "every
// other numeric variable > 0", no ceiling given). An explicit sentinel
// forces every call site to state its bounds instead of leaving one out
// by accident.
const noMax = math.MaxInt

// maxConvertedDuration is C3's post-conversion ceiling: every value that
// becomes a time.Duration must be <= 24h. Without it a seconds-valued var
// multiplied by time.Second can overflow int64 nanoseconds into a
// NEGATIVE duration, making a timeout fire instantly instead of erroring.
const maxConvertedDuration = 24 * time.Hour

// maxSecondsBeforeOverflow is the largest second count that stays inside
// maxConvertedDuration once multiplied by time.Second, checked before the
// multiplication so the comparison itself never overflows.
const maxSecondsBeforeOverflow = int(maxConvertedDuration / time.Second)

type Config struct {
	TickInterval           time.Duration
	TickWindow             time.Duration
	TickWindowRaw          string // as configured, e.g. "10m", facts.meta.window echoes this, never TickWindow.String() (C3)
	DeepWindow             time.Duration
	DeepWindowRaw          string
	SectionTimeout         time.Duration
	JournalMaxRecords      int
	FactsMaxBytes          int
	ServicesMaxBytes       int
	StateDir               string
	HostJournalDir         string
	HostJournalVolatileDir string
	HostProc               string
	HostRoot               string
	HostRasdaemon          string
	Hostname               string
	AgyBin                 string
	AgyHome                string
	AgySecretDir           string
	AgyPrintTimeout        time.Duration
	AgyHardTimeout         time.Duration
	HistoryN               int
	PromptMaxBytes         int
	HistoryKeep            int
	DeepEnabled            bool
	DeepTimeout            time.Duration
	RawAlertMaxPriority    int
	RawAlertMaxLines       int
	RawAlertRepeatSeconds  int
	RawAlertMarkerTTLHours int
	DegradedAlertAfter     time.Duration
	RenotifyAlertSec       int
	RenotifyWatchSec       int
	StaleAlertSec          int
	HeartbeatHour          int
	OutboxMax              int
	OutboxSMTPAfter        int
	AppriseURL             string
	AppriseKey             string
	AppriseConfigFile      string
	NotifyTimeout          time.Duration
	NotifyBodyMax          int
	MailriseHost           string
	MailrisePort           int
	MailriseUser           string
	MailrisePass           string
	SentinelMailFrom       string
	SentinelMailTo         string
	LogLevel               string
	TmpDir                 string
	TZ                     string
	Loc                    *time.Location
	Now                    time.Time
	TickSeq                int64
}

// Load reads every C3 variable once from the process environment.
func Load() (*Config, error) {
	var cfg Config
	var err error

	if cfg.TickInterval, err = loadSecondsRange("TICK_INTERVAL", 300, 60, 3600); err != nil {
		return nil, err
	}
	if cfg.TickWindow, cfg.TickWindowRaw, err = loadDurationRaw("TICK_WINDOW", "10m"); err != nil {
		return nil, err
	}
	if cfg.TickWindow <= cfg.TickInterval {
		return nil, errf("TICK_WINDOW", "must be greater than TICK_INTERVAL")
	}
	if cfg.DeepWindow, cfg.DeepWindowRaw, err = loadDurationRaw("DEEP_WINDOW", "24h"); err != nil {
		return nil, err
	}
	if cfg.SectionTimeout, err = loadSecondsRange("SECTION_TIMEOUT", 10, 1, noMax); err != nil {
		return nil, err
	}
	if cfg.JournalMaxRecords, err = loadIntRange("JOURNAL_MAX_RECORDS", 20000, 1, noMax); err != nil {
		return nil, err
	}
	if cfg.FactsMaxBytes, err = loadIntRange("FACTS_MAX_BYTES", 262144, 1, noMax); err != nil {
		return nil, err
	}
	if cfg.ServicesMaxBytes, err = loadIntRange("SERVICES_MAX_BYTES", 65536, 1, noMax); err != nil {
		return nil, err
	}

	cfg.StateDir = loadString("STATE_DIR", "/state")
	cfg.HostJournalDir = loadString("HOST_JOURNAL_DIR", "/host/journal")
	cfg.HostJournalVolatileDir = loadString("HOST_JOURNAL_VOLATILE_DIR", "/host/journal-volatile")
	cfg.HostProc = loadString("HOST_PROC", "/host/proc")
	cfg.HostRoot = loadString("HOST_ROOT", "/host/root")
	cfg.HostRasdaemon = loadString("HOST_RASDAEMON", "/host/rasdaemon")
	cfg.Hostname = resolveHostname(cfg.HostRoot, cfg.HostProc)

	cfg.AgyBin = loadString("AGY_BIN", "agy")
	// /state/agy-home, NOT tmpfs: agy refreshes its OAuth token as it runs,
	// and headless mode cannot re-authenticate once that refresh is lost,
	// a tmpfs HOME would leave the analyzer permanently down after the
	// first container restart (contracts/runtime.md, live-gate finding).
	cfg.AgyHome = loadString("AGY_HOME", "/state/agy-home")
	cfg.AgySecretDir = loadString("AGY_SECRET_DIR", "/run/secrets/agy")
	if cfg.AgyPrintTimeout, err = loadDuration("AGY_PRINT_TIMEOUT", "120s"); err != nil {
		return nil, err
	}
	if cfg.AgyHardTimeout, err = loadDuration("AGY_HARD_TIMEOUT", "150s"); err != nil {
		return nil, err
	}
	if min := cfg.AgyPrintTimeout + 30*time.Second; cfg.AgyHardTimeout < min {
		cfg.AgyHardTimeout = min
	}

	if cfg.HistoryN, err = loadIntRange("HISTORY_N", 5, 1, noMax); err != nil {
		return nil, err
	}
	if cfg.PromptMaxBytes, err = loadIntRange("PROMPT_MAX_BYTES", 20000, 1, noMax); err != nil {
		return nil, err
	}
	if cfg.HistoryKeep, err = loadIntRange("HISTORY_KEEP", 50, 1, noMax); err != nil {
		return nil, err
	}
	if cfg.DeepEnabled, err = loadBool01("DEEP_ENABLED", true); err != nil {
		return nil, err
	}
	if cfg.DeepTimeout, err = loadDuration("DEEP_TIMEOUT", "30s"); err != nil {
		return nil, err
	}

	if cfg.RawAlertMaxPriority, err = loadIntRange("RAW_ALERT_MAX_PRIORITY", 2, 0, 7); err != nil {
		return nil, err
	}
	if cfg.RawAlertMaxLines, err = loadIntRange("RAW_ALERT_MAX_LINES", 20, 1, 20); err != nil {
		return nil, err
	}
	// These four stay plain int seconds in Config and only become durations
	// in state/runtime, but C3's 24h bound is on the variable, not on the
	// Go type Config happens to store it in (commit c5cab9a), Load bounds
	// them here so a downstream consumer can't forget to.
	if cfg.RawAlertRepeatSeconds, err = loadIntRange("RAW_ALERT_REPEAT_SECONDS", 3600, 1, maxSecondsBeforeOverflow); err != nil {
		return nil, err
	}
	if cfg.RawAlertMarkerTTLHours, err = loadIntRange("RAW_ALERT_MARKER_TTL_HOURS", 168, 1, 8760); err != nil {
		return nil, err
	}
	if cfg.DegradedAlertAfter, err = loadSecondsRange("DEGRADED_ALERT_AFTER", 900, 0, 86400); err != nil {
		return nil, err
	}
	if cfg.RenotifyAlertSec, err = loadIntRange("RENOTIFY_ALERT_SEC", 3600, 1, maxSecondsBeforeOverflow); err != nil {
		return nil, err
	}
	if cfg.RenotifyWatchSec, err = loadIntRange("RENOTIFY_WATCH_SEC", 21600, 1, maxSecondsBeforeOverflow); err != nil {
		return nil, err
	}
	if cfg.StaleAlertSec, err = loadIntRange("STALE_ALERT_SEC", 86400, 1, maxSecondsBeforeOverflow); err != nil {
		return nil, err
	}
	if cfg.HeartbeatHour, err = loadIntRange("HEARTBEAT_HOUR", 8, 0, 23); err != nil {
		return nil, err
	}
	if cfg.OutboxMax, err = loadIntRange("OUTBOX_MAX", 50, 1, noMax); err != nil {
		return nil, err
	}
	if cfg.OutboxSMTPAfter, err = loadIntRange("OUTBOX_SMTP_AFTER", 3, 1, noMax); err != nil {
		return nil, err
	}

	cfg.AppriseURL = loadString("APPRISE_URL", "http://apprise:8000")
	cfg.AppriseKey = loadString("APPRISE_KEY", "sentinel")
	cfg.AppriseConfigFile = loadString("APPRISE_CONFIG_FILE", "/config/sentinel.cfg")
	if cfg.NotifyTimeout, err = loadSecondsRange("NOTIFY_TIMEOUT", 15, 1, noMax); err != nil {
		return nil, err
	}
	if cfg.NotifyBodyMax, err = loadIntRange("NOTIFY_BODY_MAX", 3500, 1, noMax); err != nil {
		return nil, err
	}
	cfg.MailriseHost = loadString("MAILRISE_HOST", "mailrise")
	if cfg.MailrisePort, err = loadIntRange("MAILRISE_PORT", 8025, 1, 65535); err != nil {
		return nil, err
	}
	cfg.MailriseUser = loadString("MAILRISE_USER", "")
	cfg.MailrisePass = loadString("MAILRISE_PASS", "")
	cfg.SentinelMailFrom = loadString("SENTINEL_MAIL_FROM", "sentinel@mailrise.xyz")
	cfg.SentinelMailTo = loadString("SENTINEL_MAIL_TO", "sentinel@mailrise.xyz")

	cfg.LogLevel = loadString("LOG_LEVEL", "INFO")
	switch cfg.LogLevel {
	case "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return nil, errf("LOG_LEVEL", "must be one of DEBUG, INFO, WARN, ERROR")
	}

	cfg.TmpDir = loadString("TMPDIR", "/tmp")

	cfg.TZ = loadString("TZ", "UTC")
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		return nil, errf("TZ", "not a valid time zone")
	}
	cfg.Loc = loc

	if raw := os.Getenv("SENTINEL_NOW"); raw != "" {
		now, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return nil, errf("SENTINEL_NOW", "must be RFC3339")
		}
		cfg.Now = now.UTC()
	}
	// SENTINEL_NOW unset ⇒ Now stays the zero Time. C3 calls it a test-only
	// override and contracts/state.md §S.2 makes the consumer rule explicit
	// ("zero ⇒ time.Now()"), so consumers must be able to tell "no override"
	// from a real one. Config is loaded once per process: stamping time.Now()
	// here would freeze the clock for every tick of `tick --loop`.

	return &cfg, nil
}

func loadString(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// loadIntRange reads an integer variable, applying def when unset and
// rejecting anything outside [min,max], every numeric C3 variable states
// its bounds explicitly (see the noMax sentinel above).
func loadIntRange(name string, def, min, max int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errf(name, "must be an integer")
	}
	if n < min || n > max {
		return 0, errf(name, "must be between %d and %d", min, max)
	}
	return n, nil
}

// loadSecondsRange reads an integer-seconds variable into a time.Duration,
// applying the same [min,max] bound as loadIntRange.
func loadSecondsRange(name string, def, min, max int) (time.Duration, error) {
	n, err := loadIntRange(name, def, min, max)
	if err != nil {
		return 0, err
	}
	if n > maxSecondsBeforeOverflow {
		return 0, errf(name, "must be <= 24h once converted to a duration")
	}
	return time.Duration(n) * time.Second, nil
}

// loadDuration parses a Go-style duration with no rewriting of the input
// (C3): the caller-supplied string goes straight to time.ParseDuration.
// Every duration variable must be strictly positive (C3 ranges table).
func loadDuration(name, def string) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		raw = def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errf(name, "must be a duration parseable by time.ParseDuration (e.g. \"300s\", \"24h\")")
	}
	if d <= 0 {
		return 0, errf(name, "must be > 0s")
	}
	if d > maxConvertedDuration {
		return 0, errf(name, "must be <= 24h once converted to a duration")
	}
	return d, nil
}

// loadDurationRaw is loadDuration plus the exact configured string
// (C3: "Config exposes the parsed time.Duration and the raw configured
// string", meta.window echoes the raw form, "10m" not "10m0s").
func loadDurationRaw(name, def string) (time.Duration, string, error) {
	raw := os.Getenv(name)
	if raw == "" {
		raw = def
	}
	d, err := loadDuration(name, def)
	if err != nil {
		return 0, "", err
	}
	return d, raw, nil
}

func loadBool01(name string, def bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	switch raw {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, errf(name, "must be \"0\" or \"1\"")
	}
}

// resolveHostname implements the chain from C1/C3 (quoted from collect §2):
// $SENTINEL_HOSTNAME -> $HOST_ROOT/etc/hostname -> $HOST_PROC/sys/kernel/hostname
// -> "unknown". Never os.Hostname().
func resolveHostname(hostRoot, hostProc string) string {
	if h := os.Getenv("SENTINEL_HOSTNAME"); h != "" {
		return h
	}
	if b, err := os.ReadFile(filepath.Join(hostRoot, "etc", "hostname")); err == nil {
		if h := strings.TrimSpace(string(b)); h != "" {
			return h
		}
	}
	if b, err := os.ReadFile(filepath.Join(hostProc, "sys", "kernel", "hostname")); err == nil {
		if h := strings.TrimSpace(string(b)); h != "" {
			return h
		}
	}
	return "unknown"
}
