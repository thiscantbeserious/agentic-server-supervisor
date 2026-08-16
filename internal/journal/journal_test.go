package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeJSONLRecords(t *testing.T, dir, name string, records []map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range records {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	writeFixture(t, dir, name, buf.String())
}

func withStubPath(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(wd, "testdata", "bin")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// C16: TestJournalNormalization — PRIORITY as string and number, MESSAGE
// as string and byte array, missing SYSLOG_IDENTIFIER -> "-", missing
// _SYSTEMD_UNIT -> null, unparseable __REALTIME_TIMESTAMP -> dropped;
// both journal dirs merged, (ts,message) duplicates collapsed, ordering
// ascending.
func TestJournalNormalization(t *testing.T) {
	withStubPath(t)
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	// dir1: kernel-filtered records (-k -p err key)
	writeFixture(t, dir1, "kernel.jsonl", `
{"__REALTIME_TIMESTAMP":"1755250304000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"kernel","_SYSTEMD_UNIT":"","MESSAGE":"string message"}
{"__REALTIME_TIMESTAMP":"1755250305000000","PRIORITY":3,"MESSAGE":[72,105]}
{"__REALTIME_TIMESTAMP":"not-a-number","PRIORITY":3,"MESSAGE":"dropped"}
{"__REALTIME_TIMESTAMP":"1755250303000000","MESSAGE":"no priority defaults to 6"}
`)
	// dir2: duplicate of the first dir1 record (same ts+message) plus one unique
	writeFixture(t, dir2, "kernel.jsonl", `
{"__REALTIME_TIMESTAMP":"1755250304000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"kernel","MESSAGE":"string message"}
{"__REALTIME_TIMESTAMP":"1755250306000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"kernel","_SYSTEMD_UNIT":"foo.service","MESSAGE":"from dir2"}
`)

	entries, _, _, err := Run(context.Background(), Query{
		Dirs:  []string{dir1, dir2},
		Since: "600s",
		Args:  []string{"-k", "-p", "err"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// dropped: the unparseable-timestamp record; deduped: the dir1/dir2 duplicate.
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(entries), entries)
	}

	// ascending order by ts
	for i := 1; i < len(entries); i++ {
		if entries[i-1].TS > entries[i].TS {
			t.Fatalf("entries not ascending: %+v", entries)
		}
	}

	var stringMsg, byteMsg, noPriMsg, dir2Msg *struct {
		TS, Ident, Msg string
		Pri            int
		Unit           *string
	}
	for i := range entries {
		e := entries[i]
		switch e.Message {
		case "string message":
			stringMsg = &struct {
				TS, Ident, Msg string
				Pri            int
				Unit           *string
			}{e.TS, e.Identifier, e.Message, e.Priority, e.Unit}
		case "Hi":
			byteMsg = &struct {
				TS, Ident, Msg string
				Pri            int
				Unit           *string
			}{e.TS, e.Identifier, e.Message, e.Priority, e.Unit}
		case "no priority defaults to 6":
			noPriMsg = &struct {
				TS, Ident, Msg string
				Pri            int
				Unit           *string
			}{e.TS, e.Identifier, e.Message, e.Priority, e.Unit}
		case "from dir2":
			dir2Msg = &struct {
				TS, Ident, Msg string
				Pri            int
				Unit           *string
			}{e.TS, e.Identifier, e.Message, e.Priority, e.Unit}
		}
	}

	if stringMsg == nil {
		t.Fatal("string-form MESSAGE record missing (dedup should keep exactly one)")
	}
	if stringMsg.Ident != "kernel" || stringMsg.Pri != 3 {
		t.Errorf("string message record = %+v", stringMsg)
	}
	if stringMsg.Unit != nil {
		t.Errorf("_SYSTEMD_UNIT empty string should normalize to nil unit, got %v", *stringMsg.Unit)
	}

	if byteMsg == nil {
		t.Fatal("byte-array MESSAGE record missing or not decoded to \"Hi\"")
	}
	if byteMsg.Ident != "-" {
		t.Errorf("missing SYSLOG_IDENTIFIER should normalize to \"-\", got %q", byteMsg.Ident)
	}
	if byteMsg.Pri != 3 {
		t.Errorf("numeric PRIORITY not decoded: got %d want 3", byteMsg.Pri)
	}

	if noPriMsg == nil {
		t.Fatal("no-priority record missing")
	}
	if noPriMsg.Pri != 6 {
		t.Errorf("absent PRIORITY should default to 6, got %d", noPriMsg.Pri)
	}

	if dir2Msg == nil {
		t.Fatal("dir2-only record missing — both journal dirs must be queried and merged")
	}
	if dir2Msg.Unit == nil || *dir2Msg.Unit != "foo.service" {
		t.Errorf("dir2 record unit = %v, want foo.service", dir2Msg.Unit)
	}

	for _, e := range entries {
		if e.Message == "dropped" {
			t.Fatal("record with unparseable __REALTIME_TIMESTAMP must be dropped")
		}
	}
}

func TestDirsFiltersToExistingDirectories(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Dirs(dir, file, filepath.Join(dir, "missing"), "")
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("Dirs() = %v, want [%s]", got, dir)
	}
}

func TestRunNoJournalDirs(t *testing.T) {
	withStubPath(t)
	_, _, _, err := Run(context.Background(), Query{Dirs: []string{"/does/not/exist/a", "/does/not/exist/b"}})
	if !errors.Is(err, ErrNoJournal) {
		t.Fatalf("Run() err = %v, want ErrNoJournal", err)
	}
}

func TestRunExcludeTransport(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	writeFixture(t, dir, "services.jsonl", `
{"__REALTIME_TIMESTAMP":"1755250304000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"smbd","_TRANSPORT":"syslog","MESSAGE":"Failed to start Samba"}
{"__REALTIME_TIMESTAMP":"1755250305000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"kernel","_TRANSPORT":"kernel","MESSAGE":"kernel line"}
`)
	entries, _, _, err := Run(context.Background(), Query{
		Dirs:             []string{dir},
		Args:             []string{"-p", "err"},
		ExcludeTransport: []string{"kernel"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Message != "Failed to start Samba" {
		t.Fatalf("ExcludeTransport did not drop the kernel-transport record: %+v", entries)
	}
}

func TestRunCommandNotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir) // no journalctl on PATH
	journalDir := t.TempDir()
	_, _, _, err := Run(context.Background(), Query{Dirs: []string{journalDir}})
	if err == nil {
		t.Fatal("Run() expected an error when journalctl is missing")
	}
}

func TestRunTimeout(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	writeFixture(t, dir, ".sleep", "5")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, _, err := Run(ctx, Query{Dirs: []string{dir}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() err = %v, want context.DeadlineExceeded", err)
	}
}

func TestRunExitErrorCarriesStderr(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	writeFixture(t, dir, ".stderr", "permission denied by test stub\n")
	writeFixture(t, dir, ".exit", "1")
	_, _, _, err := Run(context.Background(), Query{Dirs: []string{dir}})
	if err == nil {
		t.Fatal("Run() expected an error")
	}
	var ee *ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("Run() err = %v (%T), want *ExecError", err, err)
	}
	if ee.Stderr == "" {
		t.Error("ExecError.Stderr should carry the captured stderr text")
	}
}

// Record cap (collect.md §3): decoding always runs to the end of the
// stream (the sliding window evicts, it never breaks early), and — the
// actual point of the test — a query producing far more records than fit
// in one pipe buffer must not hang: if the drain-to-EOF path is broken,
// this test times out instead of failing cleanly.
func TestRunRecordCapDrainsPastPipeBuffer(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	// 1000 records of ~200 bytes each (~200 KB total) comfortably exceeds a
	// 64 KB pipe buffer on every platform this runs on, so a broken
	// drain-to-EOF path reproduces the deadlock this test guards against
	// rather than accidentally fitting in the kernel buffer.
	var records []map[string]any
	base := int64(1755250000000000)
	for i := 0; i < 1000; i++ {
		records = append(records, map[string]any{
			"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base+int64(i)*1000000),
			"PRIORITY":             "6", "SYSLOG_IDENTIFIER": "kernel",
			"MESSAGE": fmt.Sprintf("padding record number %04d with enough bytes to matter for the pipe buffer test filler filler filler filler filler filler", i),
		})
	}
	writeJSONLRecords(t, dir, "kernel.jsonl", records)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entries, dropped, _, err := Run(ctx, Query{
		Dirs: []string{dir}, Args: []string{"-k", "-p", "err"}, MaxRecords: 50,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(entries) != 50 {
		t.Errorf("len(entries) = %d, want 50 (MaxRecords)", len(entries))
	}
	if dropped != 950 {
		t.Errorf("dropped = %d, want 950 (1000 records - 50 cap)", dropped)
	}
}

// The point of the sliding window is WHICH entries survive, not how many
// — len(entries) == 50 && dropped == 950 would pass identically whether
// the oldest or the newest 50 were kept, which is exactly how an inverted
// rule got through a full review round. The newest record here is an
// emerg (priority 0); it must be the one still present.
func TestRunRecordCapKeepsNewest(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	base := int64(1755250000000000)
	var records []map[string]any
	for i := 0; i < 25; i++ {
		pri := 6
		if i == 24 {
			// The newest record is deliberately unprotected (D2's
			// never-evicted rule has its own test below) so this test
			// isolates "newest wins" on its own.
			pri = 4
		}
		records = append(records, map[string]any{
			"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base+int64(i)*1000000),
			"PRIORITY":             fmt.Sprintf("%d", pri), "SYSLOG_IDENTIFIER": "kernel",
			"MESSAGE": fmt.Sprintf("line %02d", i),
		})
	}
	writeJSONLRecords(t, dir, "kernel.jsonl", records)

	entries, dropped, _, err := Run(context.Background(), Query{
		Dirs: []string{dir}, Args: []string{"-k", "-p", "err"}, MaxRecords: 10,
		RawAlertMaxPriority: 2, // below the test records' priorities: nothing here is protected
	})
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 15 {
		t.Fatalf("dropped = %d, want 15 (25 records - 10-entry window)", dropped)
	}
	if len(entries) != 10 {
		t.Fatalf("len(entries) = %d, want 10", len(entries))
	}
	if got := entries[len(entries)-1].Message; got != "line 24" {
		t.Errorf("newest survivor = %q, want %q — the window must keep the newest, not the oldest", got, "line 24")
	}
	if got := entries[0].Message; got != "line 15" {
		t.Errorf("oldest survivor = %q, want %q", got, "line 15")
	}
	for _, e := range entries {
		if e.Message == "line 00" {
			t.Error("the oldest record survived the cap — window is keeping the oldest, not the newest")
		}
	}
}

// D2: entries with priority <= RawAlertMaxPriority are never evicted,
// exactly as §5's byte-budget truncation exempts them — even when the
// window is otherwise full and would normally evict them for being the
// oldest kept record.
// D2 here means "evicted last", not "never" (contract amendment): as
// long as any unprotected entry remains in the window, the protected one
// must survive.
func TestRunRecordCapEvictsProtectedLastNotNever(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	base := int64(1755250000000000)
	var records []map[string]any
	// oldest record is the one true emerg (protected); everything after
	// it is ordinary noise that would otherwise push it out of a
	// 10-entry window.
	records = append(records, map[string]any{
		"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base),
		"PRIORITY":             "0", "SYSLOG_IDENTIFIER": "kernel", "MESSAGE": "PROTECTED oldest emerg",
	})
	for i := 1; i <= 20; i++ {
		records = append(records, map[string]any{
			"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base+int64(i)*1000000),
			"PRIORITY":             "6", "SYSLOG_IDENTIFIER": "kernel",
			"MESSAGE": fmt.Sprintf("noise %02d", i),
		})
	}
	writeJSONLRecords(t, dir, "kernel.jsonl", records)

	entries, dropped, _, err := Run(context.Background(), Query{
		Dirs: []string{dir}, Args: []string{"-k", "-p", "err"}, MaxRecords: 10,
		RawAlertMaxPriority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Message == "PROTECTED oldest emerg" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the protected emerg entry was evicted while unprotected noise was still available: %+v", entries)
	}
	if dropped == 0 {
		t.Error("dropped should be > 0 — plenty of unprotected noise should have been evicted")
	}
}

// The window has a hard ceiling: even an all-protected emerg/crit storm
// must not exceed MaxRecords, or the record cap provides no memory bound
// at all during the exact scenario it exists to prevent an OOM in.
// Evicted protected entries still count as drops (so
// len(entries)+dropped == count and the schema's truncatedImpliesDropped
// holds), and the newest protected entries are the survivors.
func TestRunRecordCapHasHardCeilingEvenWhenAllProtected(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	base := int64(1755250000000000)
	const total = 30
	var records []map[string]any
	for i := 0; i < total; i++ {
		records = append(records, map[string]any{
			"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base+int64(i)*1000000),
			"PRIORITY":             "1", "SYSLOG_IDENTIFIER": "kernel",
			"MESSAGE": fmt.Sprintf("crit %02d", i),
		})
	}
	writeJSONLRecords(t, dir, "kernel.jsonl", records)

	entries, dropped, _, err := Run(context.Background(), Query{
		Dirs: []string{dir}, Args: []string{"-k", "-p", "err"}, MaxRecords: 10,
		RawAlertMaxPriority: 2, // every planted record (priority 1) is protected
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > 10 {
		t.Fatalf("len(entries) = %d, want <= 10 (the window's hard ceiling) even for an all-protected stream", len(entries))
	}
	if len(entries)+dropped != total {
		t.Errorf("len(entries)+dropped = %d, want %d (the collected-before-truncation invariant)", len(entries)+dropped, total)
	}
	if dropped == 0 {
		t.Fatal("dropped should be > 0 — an all-protected stream past the cap must still evict")
	}
	if got := entries[len(entries)-1].Message; got != fmt.Sprintf("crit %02d", total-1) {
		t.Errorf("newest survivor = %q, want the newest planted record", got)
	}
	if got := entries[0].Message; got != fmt.Sprintf("crit %02d", total-len(entries)) {
		t.Errorf("oldest survivor = %q, want exactly the tail of the newest %d", got, len(entries))
	}
}

// The O(1) all-protected fast path (contracts/collect.md §3): an
// all-emerg storm past the cap must stay linear in the number of
// records, not quadratic. Asserted against a generous wall-clock bound
// so this catches a return to O(n^2) without being flaky on a loaded
// machine — 90k records took ~3s quadratic per the reproduction that
// found this, and well under 1s linear.
func TestRunRecordCapAllProtectedStaysLinear(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	base := int64(1755250000000000)
	const total = 40000
	var buf bytes.Buffer
	for i := 0; i < total; i++ {
		b, err := json.Marshal(map[string]any{
			"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base+int64(i)*1000),
			"PRIORITY":             "1", "SYSLOG_IDENTIFIER": "kernel",
			"MESSAGE": fmt.Sprintf("crit %d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	writeFixture(t, dir, "kernel.jsonl", buf.String())

	const window = 10000
	start := time.Now()
	entries, dropped, _, err := Run(context.Background(), Query{
		Dirs: []string{dir}, Args: []string{"-k", "-p", "err"}, MaxRecords: window,
		RawAlertMaxPriority: 2,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != window {
		t.Errorf("len(entries) = %d, want %d", len(entries), window)
	}
	if dropped != total-window {
		t.Errorf("dropped = %d, want %d", dropped, total-window)
	}
	// A quadratic rescan-per-record over an all-protected stream takes
	// several seconds even at this size (the O(n) reproduction that
	// found this measured ~3s at 90k records); linear takes well under a
	// second normally. 20s leaves generous headroom for a loaded machine
	// or -race's instrumentation overhead while still catching a return
	// to O(n^2), which would blow well past it either way.
	if elapsed > 20*time.Second {
		t.Errorf("Run took %v for an all-protected %d-record stream — looks quadratic, want clearly linear", elapsed, total)
	}
}

// §3: only KEPT records (after ExcludeTransport) count against the cap —
// a kernel-transport storm must not consume the budget meant for the
// services this query actually cares about.
func TestRunRecordCapCountsOnlyKeptRecords(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	base := int64(1755250000000000)
	var records []map[string]any
	for i := 0; i < 20; i++ {
		records = append(records, map[string]any{
			"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base+int64(i)*1000000),
			"PRIORITY":             "3", "SYSLOG_IDENTIFIER": "kernel", "_TRANSPORT": "kernel",
			"MESSAGE": fmt.Sprintf("kernel noise %02d", i),
		})
	}
	for i := 0; i < 5; i++ {
		records = append(records, map[string]any{
			"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base+int64(20+i)*1000000),
			"PRIORITY":             "3", "SYSLOG_IDENTIFIER": "smbd", "_TRANSPORT": "syslog",
			"MESSAGE": fmt.Sprintf("Failed to start service %02d", i),
		})
	}
	writeJSONLRecords(t, dir, "services.jsonl", records)

	entries, dropped, _, err := Run(context.Background(), Query{
		Dirs: []string{dir}, Args: []string{"-p", "err"}, ExcludeTransport: []string{"kernel"},
		MaxRecords: 5, RawAlertMaxPriority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5 — the excluded kernel noise must not consume the cap: %+v", len(entries), entries)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 — nothing kept ever exceeded the cap", dropped)
	}
	for _, e := range entries {
		if e.Identifier == "kernel" {
			t.Errorf("excluded kernel-transport record leaked through: %+v", e)
		}
	}
}

// §3 "A mid-stream decode error is never silent": a corrupt trailing
// record must fail the query, not return a short, clean-looking slice.
func TestRunDecodeErrorFailsTheQuery(t *testing.T) {
	withStubPath(t)
	dir := t.TempDir()
	writeFixture(t, dir, "kernel.jsonl",
		`{"__REALTIME_TIMESTAMP":"1755250304000000","PRIORITY":"0","SYSLOG_IDENTIFIER":"kernel","MESSAGE":"emerg line"}
{this is not valid json at all
`)
	_, _, _, err := Run(context.Background(), Query{Dirs: []string{dir}, Args: []string{"-k", "-p", "err"}})
	if err == nil {
		t.Fatal("Run() expected an error for a corrupt mid-stream record")
	}
	if errors.Is(err, io.EOF) {
		t.Error("a decode error must not be indistinguishable from a clean EOF")
	}
}

// §3 "One directory failing does not discard the other": records already
// collected from a succeeding directory must survive a sibling
// directory's failure, reported as a warning naming that directory.
func TestRunPartialDirectoryFailureKeepsOtherDirsEntries(t *testing.T) {
	withStubPath(t)
	goodDir := t.TempDir()
	badDir := t.TempDir()
	writeFixture(t, goodDir, "kernel.jsonl",
		`{"__REALTIME_TIMESTAMP":"1755250304000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"kernel","MESSAGE":"from the good dir"}
`)
	writeFixture(t, badDir, ".exit", "1")
	writeFixture(t, badDir, ".stderr", "permission denied\n")

	entries, _, warnings, err := Run(context.Background(), Query{
		Dirs: []string{goodDir, badDir}, Args: []string{"-k", "-p", "err"},
	})
	if err != nil {
		t.Fatalf("Run() must not fail when at least one directory succeeded: %v", err)
	}
	if len(entries) != 1 || entries[0].Message != "from the good dir" {
		t.Fatalf("entries from the succeeding directory were discarded: %+v", entries)
	}
	if len(warnings) != 1 || warnings[0].Dir != badDir {
		t.Fatalf("warnings = %+v, want exactly one entry naming %s", warnings, badDir)
	}
}

func TestRunAllDirectoriesFail(t *testing.T) {
	withStubPath(t)
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeFixture(t, dir1, ".exit", "1")
	writeFixture(t, dir2, ".exit", "1")
	_, _, _, err := Run(context.Background(), Query{Dirs: []string{dir1, dir2}})
	if err == nil {
		t.Fatal("Run() expected an error when every directory failed")
	}
}
