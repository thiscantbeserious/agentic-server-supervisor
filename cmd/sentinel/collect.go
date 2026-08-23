package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/collect"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/config"
)

const collectUsage = "usage: sentinel collect [--deep zfs|smart|kernel|ras]"

// runCollect implements `sentinel collect` (contracts/collect.md §1):
// flag parsing, collect.Run, and the error → exit code map. stdout is
// reserved for the compact JSON document (C7), everything else goes to
// stderr, and stdout is never written on a non-zero exit.
func runCollect(args []string) (int, error) {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // usage is printed by hand below, on stderr only (§1)
	deep := fs.String("deep", "", "zfs|smart|kernel|ras")

	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintln(os.Stderr, collectUsage)
		return 0, nil
	case err != nil:
		fmt.Fprintln(os.Stderr, collectUsage)
		return 64, nil
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "sentinel collect: unexpected positional argument")
		fmt.Fprintln(os.Stderr, collectUsage)
		return 64, nil
	}
	switch *deep {
	case "", "zfs", "smart", "kernel", "ras":
	default:
		fmt.Fprintf(os.Stderr, "sentinel collect: --deep must be one of zfs, smart, kernel, ras, got %q\n", *deep)
		fmt.Fprintln(os.Stderr, collectUsage)
		return 64, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return exitCodeForConfigErr(err)
	}

	f, err := collect.Run(context.Background(), collect.Options{Cfg: cfg, DeepComponent: *deep})
	if err != nil {
		return 1, fmt.Errorf("collect: %w", err)
	}

	b, err := json.Marshal(f)
	if err != nil {
		return 1, fmt.Errorf("collect: marshal: %w", err)
	}
	b = append(b, '\n')
	if _, err := os.Stdout.Write(b); err != nil {
		return 1, fmt.Errorf("collect: stdout write: %w", err)
	}
	return 0, nil
}
