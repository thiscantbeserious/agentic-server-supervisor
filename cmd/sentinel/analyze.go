package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
)

const analyzeUsage = "usage: sentinel analyze          # facts.json on stdin -> report.json on stdout (debug)"

// runAnalyze implements the debug-only `sentinel analyze` entry point
// (contracts/analyze.md §1, §4): facts.json on stdin, report.json on
// stdout. The production path is the in-process analyze.Run seam called
// from `tick` (T6), not this command.
func runAnalyze(args []string) (int, error) {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch err := fs.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprintln(os.Stderr, analyzeUsage)
		return 0, nil
	case err != nil:
		fmt.Fprintln(os.Stderr, analyzeUsage)
		return 64, nil
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "sentinel analyze: unexpected positional argument")
		fmt.Fprintln(os.Stderr, analyzeUsage)
		return 64, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return exitCodeForConfigErr(err)
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 1, fmt.Errorf("analyze: read stdin: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return 65, errors.New("analyze: empty stdin, expected a facts document")
	}

	var f facts.Facts
	if err := json.Unmarshal(raw, &f); err != nil {
		return 65, fmt.Errorf("analyze: stdin is not a valid facts document: %w", err)
	}

	rep, runErr := analyze.Run(context.Background(), analyze.Options{
		Cfg: cfg, Facts: &f, Seq: f.Meta.TickSeq,
	}, analyze.DefaultDeps(cfg))

	b, merr := json.Marshal(rep)
	if merr != nil {
		return 1, fmt.Errorf("analyze: marshal report: %w", merr)
	}
	b = append(b, '\n')
	if _, werr := os.Stdout.Write(b); werr != nil {
		return 1, fmt.Errorf("analyze: stdout write: %w", werr)
	}

	if runErr != nil {
		return 3, runErr
	}
	return 0, nil
}
