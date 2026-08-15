// Command sentinel is the whole supervisor binary: subcommand dispatch and
// the one exit-code map (C2). This is the ONLY file in the codebase that
// calls os.Exit; every runX function below returns (int, error) instead.
//
// collect/analyze/state/notify/tick/health are wired here as clearly
// marked not-yet-implemented stubs — they parse their flags correctly
// (usage errors still exit 64) but do not call into runtime logic, because
// the packages that provide it (internal/collect, internal/analyze,
// internal/state, internal/notify, internal/runtime) are out of scope for
// this TODO (T2).
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	// Embeds the IANA time zone database so config.Load's TZ validation
	// (time.LoadLocation) never depends on system zoneinfo being present —
	// the debian-slim runtime image is CGO_ENABLED=0 and may not ship it.
	_ "time/tzdata"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/logging"
)

// version is set at build time in later TODOs (T7 CI); "dev" is the
// unreleased-binary default.
var version = "dev"

// errNotImplemented marks a subcommand whose backing package does not
// exist yet in this TODO.
var errNotImplemented = errors.New("not yet implemented")

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

// logSubcommandError logs to stderr via the C7 handler. Config errors are
// reported with the offending variable name only (C7: never the value).
func logSubcommandError(sub string, err error) {
	logger := slog.New(logging.New(os.Stderr, slog.LevelInfo)).With("component", sub)
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

	fmt.Fprintf(os.Stderr, "sentinel tick: not yet implemented (internal/runtime, T6) state_dir=%s\n", cfg.StateDir)
	return 1, errNotImplemented
}

// --- collect ---

func runCollect(args []string) (int, error) {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	deep := fs.String("deep", "", "zfs|smart|kernel|ras")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "sentinel collect: unexpected positional argument")
		return 64, nil
	}
	switch *deep {
	case "", "zfs", "smart", "kernel", "ras":
	default:
		fmt.Fprintf(os.Stderr, "sentinel collect: --deep must be one of zfs, smart, kernel, ras, got %q\n", *deep)
		return 64, nil
	}

	if _, err := config.Load(); err != nil {
		return exitCodeForConfigErr(err)
	}

	fmt.Fprintln(os.Stderr, "sentinel collect: not yet implemented (internal/collect, T3)")
	return 1, errNotImplemented
}

// --- analyze ---

func runAnalyze(args []string) (int, error) {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "sentinel analyze: unexpected positional argument")
		return 64, nil
	}

	if _, err := config.Load(); err != nil {
		return exitCodeForConfigErr(err)
	}

	fmt.Fprintln(os.Stderr, "sentinel analyze: not yet implemented (internal/analyze, T4)")
	return 1, errNotImplemented
}

// --- state ---

var stateSubcommands = map[string]bool{
	"process": true, "history": true, "outbox-add": true, "outbox-take": true, "outbox-ack": true,
}

func runState(args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "sentinel state: missing sub-subcommand (process|history [n]|outbox-add|outbox-take|outbox-ack <id>)")
		return 64, nil
	}
	sub := args[0]
	if !stateSubcommands[sub] {
		fmt.Fprintf(os.Stderr, "sentinel state: unknown sub-subcommand %q\n", sub)
		return 64, nil
	}
	if sub == "outbox-ack" && len(args) < 2 {
		fmt.Fprintln(os.Stderr, "sentinel state outbox-ack: missing <id>")
		return 64, nil
	}

	if _, err := config.Load(); err != nil {
		return exitCodeForConfigErr(err)
	}

	fmt.Fprintln(os.Stderr, "sentinel state: not yet implemented (internal/state, T5)")
	return 1, errNotImplemented
}

// --- notify ---

func runNotify(args []string) (int, error) {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Bool("dry-run", false, "print the payload instead of sending it")
	fs.Bool("seed-config", false, "write the apprise config volume and exit")
	if err := fs.Parse(args); err != nil {
		return 64, err
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "sentinel notify: at most one positional argument (file)")
		return 64, nil
	}

	if _, err := config.Load(); err != nil {
		return exitCodeForConfigErr(err)
	}

	fmt.Fprintln(os.Stderr, "sentinel notify: not yet implemented (internal/notify, T6)")
	return 1, errNotImplemented
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

	if _, err := config.Load(); err != nil {
		return exitCodeForConfigErr(err)
	}

	fmt.Fprintln(os.Stderr, "sentinel health: not yet implemented (internal/state heartbeat, T5)")
	return 1, errNotImplemented
}
