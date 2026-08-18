// Command sentinel is the whole supervisor binary: subcommand dispatch and
// the one exit-code map (C2). This is the ONLY file in the codebase that
// calls os.Exit; every runX function below returns (int, error) instead.
//
// Every subcommand is fully wired: collect/analyze/state (T3-T5) and
// notify/tick/health (T6) all call into their owning package
// (internal/collect, internal/analyze, internal/state, internal/notify,
// internal/runtime) rather than parsing flags into a stub.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	// Embeds the IANA time zone database so config.Load's TZ validation
	// (time.LoadLocation) never depends on system zoneinfo being present —
	// the debian-slim runtime image is CGO_ENABLED=0 and may not ship it.
	_ "time/tzdata"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/logging"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/notify"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/runtime"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/state"
)

// version is set at build time in later TODOs (T7 CI); "dev" is the
// unreleased-binary default.
var version = "dev"

func main() {
	os.Exit(guard(os.Stderr, func() int { return run(os.Args[1:]) }))
}

// guard runs f and converts a panic into exit code 1 (C2: "internal failure …
// recovered panic"). It takes the writer and the func so the recovery path is
// testable without a panic hook in the production dispatch.
func guard(stderr io.Writer, f func() int) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(stderr, "sentinel: internal error:", r)
			code = 1
		}
	}()
	return f()
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "sentinel: missing subcommand")
		printUsage()
		return 64
	}

	if args[0] == "--version" {
		fmt.Println("sentinel " + version)
		return 0
	}

	sub, rest := args[0], args[1:]

	var (
		code int
		err  error
	)
	switch sub {
	case "tick":
		code, err = runTick(rest)
	case "collect":
		code, err = runCollect(rest)
	case "analyze":
		code, err = runAnalyze(rest)
	case "state":
		code, err = runState(rest)
	case "notify":
		code, err = runNotify(rest)
	case "health":
		code, err = runHealth(rest)
	default:
		fmt.Fprintf(os.Stderr, "sentinel: unknown subcommand %q\n", sub)
		printUsage()
		return 64
	}

	if err != nil {
		logSubcommandError(sub, err)
	}
	return code
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `usage: sentinel <tick|collect|analyze|state|notify|health> [flags]
       sentinel --version`)
}

// logLevelForSubcommandError re-reads LOG_LEVEL for logSubcommandError:
// run() never holds a *config.Config (each subcommand loads its own), so a
// second, cheap config.Load() call is how this stays honoring the
// configured level rather than hardcoding one — a bare os.Getenv would
// bypass internal/config as the single loader (C1). LevelInfo is the
// fallback for the one case that second call can itself fail: reporting
// that config.Load() failed, which is exactly what logSubcommandError
// exists to do, so the fallback still gets that message out.
func logLevelForSubcommandError() slog.Level {
	if cfg, err := config.Load(); err == nil {
		return logging.ParseLevel(cfg.LogLevel)
	}
	return slog.LevelInfo
}

// logSubcommandError logs to stderr via the C7 handler. Config errors are
// reported with the offending variable name only (C7: never the value).
func logSubcommandError(sub string, err error) {
	logger := slog.New(logging.New(os.Stderr, logLevelForSubcommandError())).With("component", sub)
	var cerr *config.Error
	if errors.As(err, &cerr) {
		logger.Error("configuration error", "var", cerr.Var)
		return
	}
	logger.Error(err.Error())
}

// exitCodeForConfigErr maps a config.Load() failure to exit 78 (C2/C3).
func exitCodeForConfigErr(err error) (int, error) {
	var cerr *config.Error
	if errors.As(err, &cerr) {
		return 78, err
	}
	return 1, err
}

// --- tick ---

func runTick(args []string) (int, error) {
	fs := flag.NewFlagSet("tick", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	loop := fs.Bool("loop", false, "run continuously until SIGTERM/SIGINT")
	once := fs.Bool("once", false, "run a single tick and exit")
	stateDir := fs.String("state-dir", "", "override $STATE_DIR")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "sentinel tick: unexpected positional argument")
		return 64, nil
	}
	if *loop && *once {
		fmt.Fprintln(os.Stderr, "sentinel tick: --loop and --once are mutually exclusive")
		return 64, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return exitCodeForConfigErr(err)
	}
	if *stateDir != "" {
		cfg.StateDir = *stateDir // flag wins over $STATE_DIR (C2)
	}

	store, err := state.New(cfg)
	if err != nil {
		return 69, fmt.Errorf("tick: %s: %w", cfg.StateDir, err)
	}
	deps := runtime.DefaultDeps(cfg, store)

	if *loop {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return runtime.Loop(ctx, cfg, deps)
	}

	// R2: "Startup sequence (--loop, and once before the single tick in
	// --once)" — preflight, the read-only lint, and agy-home seeding run
	// here too, not only inside Loop().
	if code, err := runtime.StartupPreflight(context.Background(), cfg); err != nil {
		return code, fmt.Errorf("tick: %w", err)
	}

	// R3.1's tick-seq counter is scoped to the sentinel-tick command, not
	// to --loop — seq 0 tells Tick to allocate the next one from
	// $STATE_DIR/tick-seq itself, same file --loop advances.
	res := runtime.Tick(context.Background(), cfg, 0, deps)
	if res.Report != nil {
		b, merr := json.Marshal(res.Report)
		if merr != nil {
			return 1, fmt.Errorf("tick: marshal stdout document: %w", merr)
		}
		if _, werr := fmt.Fprintln(os.Stdout, string(b)); werr != nil {
			return 1, fmt.Errorf("tick: write stdout: %w", werr)
		}
	}
	return res.ExitCode, res.Err
}

// --- collect --- (cmd/sentinel/collect.go)

// --- analyze --- (cmd/sentinel/analyze.go)

// --- state --- (cmd/sentinel/state.go)

// --- notify ---

func runNotify(args []string) (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return exitCodeForConfigErr(err)
	}
	return notify.Run(context.Background(), cfg, args, os.Stdin, os.Stdout)
}

// --- health ---

func runHealth(args []string) (int, error) {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "sentinel health: unexpected positional argument")
		return 64, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return exitCodeForConfigErr(err)
	}

	// C2: health with a stale or missing heartbeat -> exit 1 (pinned,
	// compose only needs non-zero).
	code, err := runtime.Health(cfg)
	if err != nil {
		return code, fmt.Errorf("health: %w", err)
	}
	return code, nil
}
