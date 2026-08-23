// Package journal execs journalctl, normalizes journal-export JSON records
// into facts.Entry (C5), and merges + de-duplicates the results of
// HOST_JOURNAL_DIR and HOST_JOURNAL_VOLATILE_DIR. Raw journal-export field
// names never leave this package (contracts/collect.md §3).
package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/facts"
)

// ErrNoJournal is returned when neither configured journal directory exists.
var ErrNoJournal = errors.New("journal directory not found")

// Query describes one journalctl invocation, run once per existing dir in
// Dirs and merged.
type Query struct {
	Dirs  []string // existing directories only (see Dirs())
	Since string   // e.g. "600s", Run passes it to journalctl as "--since -<Since>"
	Args  []string // e.g. []string{"-k", "-p", "err"}

	// ExcludeTransport drops decoded records whose _TRANSPORT field matches
	// any of these values before normalization. Needed by the services
	// section (contracts/collect.md §3 row 7: "drop records with
	// _TRANSPORT == kernel"); the transport field itself never leaves this
	// package (C5), so the filter has to run here rather than in collect.
	ExcludeTransport []string

	// MaxRecords bounds the number of KEPT records (after
	// ExcludeTransport) held per dir to a sliding window that never
	// exceeds it (contracts/collect.md §3 "Record cap"): once the window
	// is full, decoding a further record evicts one kept record and
	// counts it in the returned dropped total, so the survivors are
	// always the newest, and the loss is always accounted for
	// (journalctl -n would make it uncountable: journald just never sends
	// the surplus). 0 means unlimited.
	MaxRecords int

	// RawAlertMaxPriority exempts entries with priority <= this value
	// from eviction (D2) for as long as any unprotected entry remains in
	// the window, eviction picks the oldest unprotected entry first.
	// Once the window holds nothing but protected entries, the ceiling
	// still holds: the oldest protected entry is evicted instead and
	// counted like any other drop. An unbounded heap during a genuine
	// emerg/crit storm is the exact OOM the cap exists to prevent, and
	// losing the oldest of MaxRecords critical lines is better than
	// losing the whole container, and the storm itself, to the OOM
	// killer. D2 means "evicted last" here, not "never".
	RawAlertMaxPriority int
}

// DirError is one directory's journalctl failure. Run tolerates any
// number of these as long as at least one directory succeeded, the
// persistent and volatile journals are two views of the same log, and a
// permission problem on one must not discard records already collected
// from the other (contracts/collect.md §3).
type DirError struct {
	Dir string
	Err error
}

func (e *DirError) Error() string { return e.Dir + ": " + e.Err.Error() }
func (e *DirError) Unwrap() error { return e.Err }

// Dirs returns the subset of paths that are journal directories: they
// exist, and they contain at least one .journal file, which systemd stores
// one level down under a machine-id directory.
//
// Existence alone is not enough, and the difference is not theoretical. A
// host with persistent journal storage has no /run/log/journal, but the
// compose file bind-mounts it anyway and Docker CREATES the source as an
// empty directory when it is missing. That empty directory then looks like
// a journal to a plain stat, journalctl is run against it, and it answers
// "No journal files were found." with a non-zero status. The section
// survives, since another directory succeeded, but every tick on every
// such host carries a collector error that describes nothing wrong.
//
// A directory with no journal files in it is not a journal that failed to
// be read. It is not a journal.
func Dirs(paths ...string) []string {
	var out []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			continue
		}
		if !hasJournalFiles(p) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// hasJournalFiles reports whether dir holds any .journal file, directly or
// one level down. Both layouts are checked rather than assuming the
// machine-id subdirectory: journalctl accepts either, and a directory
// handed over by --directory is not required to be laid out the way
// /var/log/journal is.
func hasJournalFiles(dir string) bool {
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.journal")); len(matches) > 0 {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "*.journal"))
	return len(matches) > 0
}

// Run execs journalctl once per dir, decodes the JSON stream, normalizes,
// merges, sorts by ts and de-duplicates on (ts, message).
//
// dropped counts records beyond Query.MaxRecords across all dirs, the
// caller adds it to the section's dropped_entries and sets truncated:
// true when dropped > 0. warnings carries one *DirError per directory
// that failed while at least one other directory succeeded (§3 "one
// directory failing does not discard the other"), the caller records
// each as a collector_errors[] entry without failing the section. err is
// non-nil only when EVERY directory failed (or neither exists), in which
// case entries/dropped/warnings are all zero and the section must fail.
func Run(ctx context.Context, q Query) (entries []facts.Entry, dropped int, warnings []*DirError, err error) {
	dirs := Dirs(q.Dirs...)
	if len(dirs) == 0 {
		return nil, 0, nil, fmt.Errorf("%w: %s", ErrNoJournal, strings.Join(q.Dirs, ", "))
	}

	exclude := make(map[string]bool, len(q.ExcludeTransport))
	for _, t := range q.ExcludeTransport {
		exclude[t] = true
	}

	var all []facts.Entry
	total := 0
	var warn []*DirError
	ok := false
	for _, dir := range dirs {
		got, d, rerr := runOne(ctx, dir, q.Since, q.Args, exclude, q.MaxRecords, q.RawAlertMaxPriority)
		if rerr != nil {
			warn = append(warn, &DirError{Dir: dir, Err: rerr})
			continue
		}
		ok = true
		all = append(all, got...)
		total += d
	}
	if !ok {
		// Every directory failed: this is a section failure, not a
		// tolerated partial one. Surface the first directory's error
		// (deterministic, Dirs() preserves HOST_JOURNAL_DIR before
		// HOST_JOURNAL_VOLATILE_DIR) as the query's own error.
		return nil, 0, nil, warn[0]
	}
	return mergeDedup(all), total, warn, nil
}

// runOne execs journalctl for one dir. A non-io.EOF decode error means
// the stream was corrupt or truncated: the remainder is unaccounted for,
// so the query fails rather than silently returning a short, truncated-
// looking-clean slice (contracts/collect.md §3 "A mid-stream decode error
// is never silent"). Every exit from the decode loop drains stdout to
// EOF before cmd.Wait(): journalctl blocks writing into a full 64 KB pipe
// buffer while the parent waits for it to exit, so abandoning the pipe
// early deadlocks the section until SECTION_TIMEOUT kills it.
func runOne(ctx context.Context, dir, since string, args []string, exclude map[string]bool, maxRecords, maxPriority int) ([]facts.Entry, int, error) {
	cmdArgs := []string{"-D", dir}
	if since != "" {
		cmdArgs = append(cmdArgs, "--since", "-"+since)
	}
	cmdArgs = append(cmdArgs, "-o", "json", "--no-pager")
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.CommandContext(ctx, "journalctl", cmdArgs...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, err
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}

	var entries []facts.Entry
	dropped := 0
	// allProtected is set once a scan finds nothing evictable (every held
	// entry has priority <= maxPriority) and cleared the moment an
	// unprotected record is appended. It turns the common "window is one
	// long emerg storm" case from an O(n) rescan per record into an O(1)
	// check, without it, that path is quadratic (contracts/collect.md
	// §3 "Record cap").
	allProtected := false
	dec := json.NewDecoder(stdout)
decodeLoop:
	for {
		var rec rawRecord
		switch decErr := dec.Decode(&rec); {
		case decErr == io.EOF:
			break decodeLoop
		case decErr != nil:
			if ctx.Err() == context.DeadlineExceeded {
				// The pipe was torn down because the section timeout
				// killed journalctl mid-stream, that looks like a
				// decode error but is a timeout, not corruption.
				cmd.Wait() //nolint:errcheck // reap the child
				return nil, 0, context.DeadlineExceeded
			}
			io.Copy(io.Discard, stdout) //nolint:errcheck // best-effort drain before Wait()
			cmd.Wait()                  //nolint:errcheck // reap the child; the decode error is authoritative here
			return nil, 0, fmt.Errorf("decode: %w", decErr)
		}
		if exclude[rec.Transport] {
			continue // excluded records never count against the cap
		}
		e, ok := normalize(rec)
		if !ok {
			continue
		}
		entries = append(entries, e)
		if e.Priority > maxPriority {
			allProtected = false
		}
		if maxRecords > 0 && len(entries) > maxRecords {
			// Sliding window with a hard ceiling (§3 "Record cap"): the
			// window never exceeds maxRecords. Evict the oldest
			// unprotected (priority > RAW_ALERT_MAX_PRIORITY) kept entry
			// first, same as §5. D2 means "evicted last" here, not
			// "never", once the window holds nothing but protected
			// entries (reachable only once maxRecords consecutive
			// emerg/crit/alert records have arrived), the oldest
			// protected entry is evicted instead and counted the same as
			// any other drop: an unbounded heap during collection is the
			// exact OOM the cap exists to prevent, and losing the oldest
			// of maxRecords critical lines is strictly better than
			// losing the whole container (and the storm itself) to the
			// OOM killer.
			evictIdx := -1
			if !allProtected {
				for i, x := range entries {
					if x.Priority > maxPriority {
						evictIdx = i
						break
					}
				}
				if evictIdx == -1 {
					allProtected = true
				}
			}
			if evictIdx == -1 {
				// ponytail: evictIdx is always 0 here (oldest, protected
				// or not), so this could drop the head without the
				// memmove append(entries[:i], entries[i+1:]...) does,
				// O(n×cap) all-protected worst case reaches
				// SECTION_TIMEOUT around 600k records/query. Left as is:
				// well outside anything bam produces, and a reslice-based
				// head-drop needs its own backing-array-retention test.
				evictIdx = 0 // hard ceiling: evict the oldest, protected or not
			}
			entries = append(entries[:evictIdx], entries[evictIdx+1:]...)
			dropped++
		}
	}
	io.Copy(io.Discard, stdout) //nolint:errcheck // no-op after a clean EOF; defensive per §3

	if err := cmd.Wait(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, 0, context.DeadlineExceeded
		}
		return nil, 0, &ExecError{Bin: "journalctl", Err: err, Stderr: truncate(stderr.String(), 200)}
	}
	if entries == nil {
		entries = []facts.Entry{}
	}
	return entries, dropped, nil
}

// ExecError wraps a journalctl exec failure with the binary name and a
// truncated stderr capture (contracts/collect.md §7: "journalctl writes to
// stderr → captured into the error reason (first 200 bytes)"). Unwrap
// exposes the underlying *exec.ExitError / *exec.Error so errors.As/Is in
// collect's error mapping keeps working unchanged.
type ExecError struct {
	Bin    string
	Err    error
	Stderr string
}

func (e *ExecError) Error() string {
	if e.Stderr == "" {
		return e.Err.Error()
	}
	return e.Err.Error() + ": " + e.Stderr
}

func (e *ExecError) Unwrap() error { return e.Err }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// rawRecord is the subset of journal-export JSON fields normalize needs.
// These field names never leave this package.
type rawRecord struct {
	Timestamp  string          `json:"__REALTIME_TIMESTAMP"`
	Priority   json.RawMessage `json:"PRIORITY"`
	Identifier string          `json:"SYSLOG_IDENTIFIER"`
	Unit       string          `json:"_SYSTEMD_UNIT"`
	Message    json.RawMessage `json:"MESSAGE"`
	Transport  string          `json:"_TRANSPORT"`
}

// normalize converts one raw record into facts.Entry per collect.md §3's
// normalization table. ok is false when the record must be dropped
// (unparseable __REALTIME_TIMESTAMP).
func normalize(r rawRecord) (facts.Entry, bool) {
	ts, ok := parseTimestamp(r.Timestamp)
	if !ok {
		return facts.Entry{}, false
	}
	ident := r.Identifier
	if ident == "" {
		ident = "-"
	}
	var unit *string
	if r.Unit != "" {
		u := r.Unit
		unit = &u
	}
	return facts.Entry{
		TS:         ts,
		Priority:   parsePriority(r.Priority),
		Identifier: ident,
		Unit:       unit,
		Message:    parseMessage(r.Message),
	}, true
}

func parseTimestamp(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return "", false
	}
	return time.UnixMicro(n).UTC().Format(time.RFC3339), true
}

// parsePriority accepts PRIORITY as a JSON string or number; anything
// absent or unparseable defaults to 6 (info) per §3.
func parsePriority(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 6
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 7 {
			return 6
		}
		return n
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil && n >= 0 && n <= 7 {
		return n
	}
	return 6
}

// parseMessage accepts MESSAGE as a JSON string or a JSON array of byte
// values (journalctl's encoding for non-UTF8 message bytes).
func parseMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var b []byte
	if err := json.Unmarshal(raw, &b); err == nil {
		return string(b)
	}
	return ""
}

// mergeDedup sorts ascending by ts (stable, so ties keep original,
// per-dir, dir-order, order) and drops (ts, message) duplicates, keeping
// the first occurrence.
func mergeDedup(entries []facts.Entry) []facts.Entry {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].TS < entries[j].TS })
	seen := make(map[string]struct{}, len(entries))
	out := make([]facts.Entry, 0, len(entries))
	for _, e := range entries {
		key := e.TS + "\x00" + e.Message
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, e)
	}
	return out
}
