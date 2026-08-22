// loop.go: Loop(), startup preflight, agy-home seeding, the tick-seq
// counter, and the signal-driven interval loop (R2).
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
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

// preflightError maps to exit 78, "ERROR naming the exact path" (R2 step 2).
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

// checkJournalReadable exists because R2's filesystem preflight
// (dirReadableNonEmpty above) only proves
// the configured journal directory exists and has files in it, it does
// NOT prove journald will hand the unprivileged sentinel user any journal
// CONTENT. A stale JOURNAL_GID, an ineffective group_add, or a wrong
// HOST_JOURNAL_DIR mount all leave that filesystem-level check green while
// journalctl itself reads nothing, and an empty journal is indistinguishable
// from a quiet system, the quietest possible failure of a component whose
// job is noticing things.
//
// Measured against real journalctl in debian:trixie-slim (the R1 runtime
// base) before writing this: "journalctl -n1 --no-pager" on an empty or
// unreadable journal exits 0 and prints "-- No entries --\n" to STDOUT, not
// stderr, so neither "exit code only" nor "stdout non-empty" discriminates
// a real record from that sentinel line. "-o json" does: an empty journal
// under -o json writes zero bytes. This function therefore queries with
// "-o json" and treats non-zero exit OR zero-byte stdout as failure.
//
// Queries HOST_JOURNAL_DIR first, then HOST_JOURNAL_VOLATILE_DIR, the
// same "at least one readable" tolerance the filesystem check above uses
// (a fresh boot can legitimately have an empty persistent journal with
// only the volatile one populated), and fails only if neither yields a
// record.
func checkJournalReadable(ctx context.Context, cfg *config.Config) error {
	timeout := cfg.SectionTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	var lastReason string
	for _, dir := range []string{cfg.HostJournalDir, cfg.HostJournalVolatileDir} {
		if dir == "" {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(cctx, "journalctl", "-D", dir, "-n1", "-o", "json", "--no-pager")
		out, err := cmd.Output()
		cancel()
		if err != nil {
			lastReason = fmt.Sprintf("journalctl -D %s -n1 failed: %v", dir, err)
			continue
		}
		trimmed := bytes.TrimSpace(out)
		if len(trimmed) == 0 {
			lastReason = fmt.Sprintf("journalctl -D %s -n1 returned zero entries", dir)
			continue
		}
		// Defense in depth beyond "-o json": if journalctl ever falls back
		// to its human-readable "-- No entries --" sentinel line (that is
		// what a real, unreadable/empty journal prints WITHOUT -o json,
		// -o json is what makes that case zero bytes instead), a
		// non-empty-but-non-JSON line must still be rejected rather than
		// accepted as a real record.
		if !json.Valid(trimmed) {
			lastReason = fmt.Sprintf("journalctl -D %s -n1 returned non-JSON output", dir)
			continue
		}
		return nil // a real record from at least one directory
	}
	return &preflightError{
		cfg.HostJournalDir,
		lastReason + ", check JOURNAL_GID, group_add, or the /host/journal mount",
	}
}

type warner interface{ Warn(string, ...any) }

// seedAgyHome copies the mounted credential tree into $AGY_HOME/.gemini
// and points $HOME at $AGY_HOME. A missing or empty secret dir is a WARN,
// not fatal: the raw-alert path must survive without the LLM.
//
// The layout is agy's, measured on the target host rather than assumed:
//
//	~/.gemini/
//	  antigravity-cli/antigravity-oauth-token   <- the credential
//	  config/
//
// Two things follow, and the previous version got both wrong. Every
// top-level entry is a DIRECTORY, and it copied "regular files only", so
// it copied nothing while reporting nothing, because the directory was
// not empty. And agy reads the credential from $HOME/.gemini/, so a flat
// copy into $HOME would not have been found even if it had happened.
//
// Verified end to end before this was written: ~/.gemini copied into a
// fresh HOME, agy --print returned status SUCCESS with real token usage.
// contracts/runtime.md asked for exactly that measurement before relying
// on seeding, and until now nobody had run it.
func seedAgyHome(cfg *config.Config, logger warner) {
	if err := os.MkdirAll(cfg.AgyHome, 0o700); err != nil {
		logger.Warn("runtime could not create AGY_HOME", "error", err)
		return
	}
	entries, err := os.ReadDir(cfg.AgySecretDir)
	if err != nil || len(entries) == 0 {
		logger.Warn("runtime agy credentials absent, analysis will fall back")
		os.Setenv("HOME", cfg.AgyHome)
		return
	}

	dst := filepath.Join(cfg.AgyHome, ".gemini")
	if skipped, err := copyTree(cfg.AgySecretDir, dst); err != nil {
		logger.Warn("runtime could not seed agy credentials, analysis will fall back", "error", err)
	} else if len(skipped) > 0 {
		// Named, not silent: if the token was among them the operator
		// needs to know which file to make readable, and if it was not
		// then this is noise about a cache entry and says so.
		logger.Warn("runtime skipped unreadable files while seeding agy credentials",
			"count", len(skipped), "files", strings.Join(skipped, ","))
	}
	os.Setenv("HOME", cfg.AgyHome)
}

// copyTree copies src into dst recursively: directories 0700, files 0600.
// The source is a read-only mount, so nothing here writes back to it.
// Symlinks are skipped rather than followed, a credential mount is not a
// place to chase links out of.
func copyTree(src, dst string) ([]string, error) {
	var skipped []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular():
			return nil
		default:
			// Bootstrap only. agy owns $AGY_HOME once it runs: it
			// refreshes its token there and keeps
			// conversation_summaries.db and antigravity-cli/brain/
			// there. Re-copying the host tree on every start would
			// replace a refreshed token with the older host copy,
			// where no agy runs to keep it current, and would delete
			// accumulated memory on a restart. What is missing is
			// still copied, so a first seed interrupted halfway
			// completes on the next start.
			if _, serr := os.Stat(target); serr == nil {
				return nil
			}
			// A file the container cannot read must not cost it the
			// credential. This mount is agy's whole state directory,
			// caches and a conversation database included, owned by
			// the operator with only some files carrying a group the
			// container shares. On the target host an unreadable
			// cache entry aborted the walk before the token was
			// reached.
			if cerr := copyFile(path, target); cerr != nil {
				if errors.Is(cerr, fs.ErrPermission) {
					rel, _ := filepath.Rel(src, path)
					skipped = append(skipped, rel)
					return nil
				}
				return cerr
			}
			return nil
		}
	})
	return skipped, err
}

// copyFile streams src to dst at mode 0600, creating the parent.
//
// Streamed rather than read whole: this tree is 16.6 MB across 43 files on
// the target host and 16.3 MB of that is a single bundled browser helper,
// inside a container capped at mem_limit 512m. Nothing here needs the file
// in memory, so nothing here holds it.
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// nextTickSeq is R3.1: read, increment, write atomically. Missing or
// unparseable starts at 1 and WARNs.
func nextTickSeq(cfg *config.Config, logger warner) int64 {
	path := filepath.Join(cfg.StateDir, "tick-seq")
	var seq int64 = 1
	// R3.1: "Missing or unparseable ⇒ start at 1 and WARN", both cases,
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
// Writable ⇒ WARN, continue, never block ticks on a lint.
func assertBinDirReadOnly(logger warner) {
	probe := "/usr/local/bin/.preflight-probe"
	if err := os.WriteFile(probe, []byte{}, 0o600); err == nil {
		os.Remove(probe)
		logger.Warn("runtime /usr/local/bin is writable, read_only:true may not be in effect")
	}
}

// StartupPreflight is R2's startup sequence steps 2-5: filesystem
// preflight (STATE_DIR, /tmp, a readable journal dir, $HOST_PROC/uptime,
// and the C4 subdirectory MkdirAlls), the /usr/local/bin read-only lint,
// and agy-home seeding. R2 requires this "once before --loop starts
// ticking, and once before the single tick in --once", step 1
// (config.Load) and step 6 (signal.NotifyContext, --loop only) are the
// caller's job. Returns the C2 exit code: 69 for a state-dir failure, 78
// for any other preflight failure, 0 on success.
func StartupPreflight(ctx context.Context, cfg *config.Config) (int, error) {
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

	// The directory-level check above proves the mount exists and is
	// listable; it does not prove journalctl can actually read journal
	// CONTENT through it. Run before the first tick so a stale
	// JOURNAL_GID fails loud at startup instead of silently reporting
	// all-clear forever.
	//
	// ctx is deliberately NOT the shutdown-signal context both callers
	// hold (Loop's sigCtx, --once's request context): wiring a
	// SIGTERM-cancellable context in here would make a real SIGTERM
	// arriving during preflight report exit 78 ("context canceled") from
	// checkJournalReadable's own exec.CommandContext, instead of the
	// exit 0 R2 contracts for a signal-driven shutdown ("--loop
	// terminates only with 0 (signal), 78 (config/mount preflight) ...").
	// checkJournalReadable already self-bounds via cfg.SectionTimeout (or
	// its own 10s default), so the accepted cost of NOT threading shutdown
	// cancellation through is a startup tail of at most that timeout,
	// comfortably inside the 15s stop_grace_period compose sets. Both
	// current callers pass context.Background() for exactly this reason;
	// ctx stays a parameter (rather than being dropped) so a future
	// caller with a different tradeoff, or a test that wants to bound the
	// whole preflight call itself, still can.
	if err := checkJournalReadable(ctx, cfg); err != nil {
		var pferr *preflightError
		if errors.As(err, &pferr) {
			logger.Error("journal not readable", "path", pferr.path, "reason", pferr.reason)
		}
		return 78, err
	}

	assertBinDirReadOnly(logger)
	seedAgyHome(cfg, logger)
	return 0, nil
}

// Loop runs StartupPreflight once, then ticks every TICK_INTERVAL until
// SIGTERM/SIGINT. It terminates only with 0 (signal), 78 (config/mount
// preflight) or 69 (state dir), every other tick failure is logged and
// the loop continues (R2).
func Loop(ctx context.Context, cfg *config.Config, d Deps) (int, error) {
	logger := newLogger(cfg)

	if code, err := StartupPreflight(context.Background(), cfg); err != nil {
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
