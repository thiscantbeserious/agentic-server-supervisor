// loop.go: Loop() — startup preflight, agy-home seeding, the tick-seq
// counter, and the signal-driven interval loop (R2).
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
)

// stateDirError maps to exit 69 (C2's state-dir specialization of startup
// validation).
type stateDirError struct{ path string }

func (e *stateDirError) Error() string {
	return fmt.Sprintf("%s: STATE_DIR missing or unwritable", e.path)
}

// preflightError maps to exit 78 — "ERROR naming the exact path" (R2 step 2).
type preflightError struct {
	path   string
	reason string
}

func (e *preflightError) Error() string {
	return fmt.Sprintf("%s: %s", e.path, e.reason)
}

// preflight is R2's startup sequence steps 2 and 5, run once before the
// loop and once before a single --once tick.
func preflight(cfg *config.Config) error {
	info, err := os.Stat(cfg.StateDir)
	if err != nil || !info.IsDir() {
		return &stateDirError{cfg.StateDir}
	}
	probe := filepath.Join(cfg.StateDir, ".preflight-probe")
	if err := os.WriteFile(probe, []byte{}, 0o600); err != nil {
		return &stateDirError{cfg.StateDir}
	}
	os.Remove(probe)

	tmpProbe := filepath.Join(cfg.TmpDir, ".preflight-probe")
	if err := os.WriteFile(tmpProbe, []byte{}, 0o600); err != nil {
		return &preflightError{cfg.TmpDir, "not writable"}
	}
	os.Remove(tmpProbe)

	if !dirReadableNonEmpty(cfg.HostJournalDir) && !dirReadableNonEmpty(cfg.HostJournalVolatileDir) {
		return &preflightError{cfg.HostJournalDir, "neither journal directory is readable and non-empty"}
	}

	uptimePath := filepath.Join(cfg.HostProc, "uptime")
	if _, err := os.Stat(uptimePath); err != nil {
		return &preflightError{uptimePath, "not readable"}
	}

	for _, dir := range []string{"history", "active-alerts", "outbox", "raw-alerts", "deep-queue"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, dir), 0o700); err != nil {
			return &stateDirError{cfg.StateDir}
		}
	}
	return nil
}

func dirReadableNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

type warner interface{ Warn(string, ...any) }

// seedAgyHome is R2 step 4: MkdirAll($AGY_HOME, 0700), copy
// $AGY_SECRET_DIR's regular files into it (mode 0600), then point $HOME
// at it. A missing or empty secret dir is a WARN, not fatal — the
// raw-alert path must survive without the LLM.
func seedAgyHome(cfg *config.Config, logger warner) {
	if err := os.MkdirAll(cfg.AgyHome, 0o700); err != nil {
		logger.Warn("runtime could not create AGY_HOME", "error", err)
		return
	}
	entries, err := os.ReadDir(cfg.AgySecretDir)
	if err != nil || len(entries) == 0 {
		logger.Warn("runtime agy credentials absent — analysis will fall back")
	} else {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(cfg.AgySecretDir, e.Name()))
			if rerr != nil {
				continue
			}
			os.WriteFile(filepath.Join(cfg.AgyHome, e.Name()), data, 0o600)
		}
	}
	os.Setenv("HOME", cfg.AgyHome)
}

// nextTickSeq is R3.1: read, increment, write atomically. Missing or
// unparseable starts at 1 and WARNs.
func nextTickSeq(cfg *config.Config, logger warner) int64 {
	path := filepath.Join(cfg.StateDir, "tick-seq")
	var seq int64 = 1
	// R3.1: "Missing or unparseable ⇒ start at 1 and WARN" — both cases,
	// not only the unparseable one.
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		logger.Warn("tick-seq missing, starting at 1")
	} else if n, perr := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); perr == nil {
		seq = n + 1
	} else {
		logger.Warn("tick-seq unparseable, restarting at 1")
	}

	if tmp, err := os.CreateTemp(cfg.StateDir, ".tmp-tick-seq-*"); err == nil {
		tmp.WriteString(strconv.FormatInt(seq, 10))
		tmp.Close()
		os.Chmod(tmp.Name(), 0o644)
		os.Rename(tmp.Name(), path)
	}
	return seq
}

// assertBinDirReadOnly is R2 step 3: prove read_only:true took effect.
// Writable ⇒ WARN, continue — never block ticks on a lint.
func assertBinDirReadOnly(logger warner) {
	probe := "/usr/local/bin/.preflight-probe"
	if err := os.WriteFile(probe, []byte{}, 0o600); err == nil {
		os.Remove(probe)
		logger.Warn("runtime /usr/local/bin is writable — read_only:true may not be in effect")
	}
}

// StartupPreflight is R2's startup sequence steps 2-5: filesystem
// preflight (STATE_DIR, /tmp, a readable journal dir, $HOST_PROC/uptime,
// and the C4 subdirectory MkdirAlls), the /usr/local/bin read-only lint,
// and agy-home seeding. R2 requires this "once before --loop starts
// ticking, and once before the single tick in --once" — step 1
// (config.Load) and step 6 (signal.NotifyContext, --loop only) are the
// caller's job. Returns the C2 exit code: 69 for a state-dir failure, 78
// for any other preflight failure, 0 on success.
func StartupPreflight(cfg *config.Config) (int, error) {
	logger := newLogger(cfg)

	if err := preflight(cfg); err != nil {
		var sderr *stateDirError
		if errors.As(err, &sderr) {
			logger.Error("STATE_DIR missing or unwritable", "path", sderr.path)
			return 69, err
		}
		var pferr *preflightError
		if errors.As(err, &pferr) {
			logger.Error("startup preflight failed", "path", pferr.path, "reason", pferr.reason)
		}
		return 78, err
	}

	assertBinDirReadOnly(logger)
	seedAgyHome(cfg, logger)
	return 0, nil
}

// Loop runs StartupPreflight once, then ticks every TICK_INTERVAL until
// SIGTERM/SIGINT. It terminates only with 0 (signal), 78 (config/mount
// preflight) or 69 (state dir) — every other tick failure is logged and
// the loop continues (R2).
func Loop(ctx context.Context, cfg *config.Config, d Deps) (int, error) {
	logger := newLogger(cfg)

	if code, err := StartupPreflight(cfg); err != nil {
		return code, err
	}

	sigCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var ticks int64
	for {
		select {
		case <-sigCtx.Done():
			logger.Info("runtime stopped", "ticks", ticks)
			return 0, nil
		default:
		}

		seq := nextTickSeq(cfg, logger)
		// A tick already in flight gets 5s after shutdown is requested,
		// then its context is cancelled (R2 step 6).
		tickCtx, cancel := gracefulTickContext(sigCtx)
		res := Tick(tickCtx, cfg, seq, d)
		cancel()
		ticks++
		if res.ExitCode != 0 {
			logger.Warn("tick rc=" + strconv.Itoa(res.ExitCode))
		}

		select {
		case <-sigCtx.Done():
			logger.Info("runtime stopped", "ticks", ticks)
			return 0, nil
		case <-time.After(cfg.TickInterval):
		}
	}
}

// gracefulTickContext derives a tick-scoped context that survives the
// parent's cancellation for 5s (R2 step 6: "a tick already in flight gets
// 5s, then its context is cancelled"), so a slow section gets a grace
// period instead of being cut off the instant a signal arrives.
func gracefulTickContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	go func() {
		select {
		case <-parent.Done():
			select {
			case <-time.After(5 * time.Second):
				cancel()
			case <-stop:
			}
		case <-stop:
		}
	}()
	return ctx, func() { close(stop); cancel() }
}
