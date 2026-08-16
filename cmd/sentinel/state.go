package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	st "github.com/thiscantbeserious/agentic-server-supervisor/internal/state"
)

var stateSubcommands = map[string]bool{
	"process": true, "history": true, "outbox-add": true, "outbox-take": true, "outbox-ack": true,
}

// stateMaxPositional is how many positional args (past the sub-subcommand
// itself) each state subcommand accepts (S.1: only history and outbox-ack
// take one). Subcommands absent here default to 0 via the zero value.
var stateMaxPositional = map[string]int{
	"history":    1,
	"outbox-ack": 1,
}

const stateUsage = "usage: sentinel state process|history [n]|outbox-add|outbox-take|outbox-ack <id>"

// runState implements `sentinel state` (contracts/state.md §S.1, §S.6).
// stdout carries exactly one compact JSON document (or, for outbox-ack,
// nothing); everything else goes to stderr (C7). Any state failure maps to
// exit 5, per §S.6, except the input-shape errors that are exit 65 and the
// dir-probe failure in state.New that is exit 69.
func runState(args []string) (int, error) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "sentinel state: missing sub-subcommand")
		fmt.Fprintln(os.Stderr, stateUsage)
		return 64, nil
	}
	sub := args[0]
	if !stateSubcommands[sub] {
		fmt.Fprintf(os.Stderr, "sentinel state: unknown sub-subcommand %q\n", sub)
		fmt.Fprintln(os.Stderr, stateUsage)
		return 64, nil
	}
	if sub == "outbox-ack" && len(args) < 2 {
		fmt.Fprintln(os.Stderr, "sentinel state outbox-ack: missing <id>")
		return 64, nil
	}
	// C2/S.1: "history [n] and outbox-ack <id> are the only subcommands
	// taking a positional argument" — process/outbox-add/outbox-take take
	// none, and a stray one past what's allowed is a usage error (64), not
	// something to silently ignore.
	if maxPositional := stateMaxPositional[sub]; len(args)-1 > maxPositional {
		fmt.Fprintf(os.Stderr, "sentinel state %s: unexpected argument %q\n", sub, args[len(args)-1])
		fmt.Fprintln(os.Stderr, stateUsage)
		return 64, nil
	}

	cfg, err := config.Load()
	if err != nil {
		return exitCodeForConfigErr(err)
	}

	store, err := st.New(cfg)
	if err != nil {
		return 69, fmt.Errorf("state: %w", err)
	}

	switch sub {
	case "process":
		return stateProcess(store)
	case "history":
		n := 5
		if len(args) > 1 {
			v, err := strconv.Atoi(args[1])
			if err != nil || v < 0 {
				fmt.Fprintln(os.Stderr, "sentinel state history: n must be a non-negative integer")
				return 64, nil
			}
			n = v
		}
		return stateHistory(store, n)
	case "outbox-add":
		return stateOutboxAdd(store)
	case "outbox-take":
		return stateOutboxTake(store)
	case "outbox-ack":
		return stateOutboxAck(store, args[1])
	}
	return 64, nil // unreachable: sub already validated against stateSubcommands
}

func stateProcess(store *st.Store) (int, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 1, fmt.Errorf("state process: read stdin: %w", err)
	}
	d, err := store.Process(raw)
	if err != nil {
		if err == st.ErrBadInput {
			return 65, err
		}
		return 5, fmt.Errorf("state process: %w", err)
	}
	return writeJSONLine(d)
}

func stateHistory(store *st.Store, n int) (int, error) {
	h, err := store.History(n)
	if err != nil {
		return 5, fmt.Errorf("state history: %w", err)
	}
	return writeJSONLine(h)
}

func stateOutboxAdd(store *st.Store) (int, error) {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return 1, fmt.Errorf("state outbox-add: read stdin: %w", err)
	}
	id, err := store.OutboxAdd(raw)
	if err != nil {
		if err == st.ErrBadInput {
			return 65, err
		}
		return 5, fmt.Errorf("state outbox-add: %w", err)
	}
	fmt.Println(id)
	return 0, nil
}

func stateOutboxTake(store *st.Store) (int, error) {
	items, err := store.OutboxTake()
	if err != nil {
		return 5, fmt.Errorf("state outbox-take: %w", err)
	}
	return writeJSONLine(items)
}

func stateOutboxAck(store *st.Store, id string) (int, error) {
	if err := store.OutboxAck(id); err != nil {
		return 5, fmt.Errorf("state outbox-ack: %w", err)
	}
	return 0, nil
}

func writeJSONLine(v any) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 1, fmt.Errorf("state: marshal: %w", err)
	}
	b = append(b, '\n')
	if _, err := os.Stdout.Write(b); err != nil {
		return 1, fmt.Errorf("state: stdout write: %w", err)
	}
	return 0, nil
}
