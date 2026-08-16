package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
)

// --- fixture tree helpers ---

type tree struct {
	journalDir, journalVolDir string
	hostProc, hostRoot        string
	hostRasdaemon             string
	stateDir                  string
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sampleArcstats = `name type data
5 1 0
size 4 8523441152
c 4 8589934592
c_max 4 17179869184
c_min 4 1073741824
hits 4 918273645
misses 4 3421887
l2_size 4 0
l2_hits 4 0
l2_misses 4 0
`

const procNetHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func newTree(t *testing.T) tree {
	t.Helper()
	tr := tree{
		journalDir:    t.TempDir(),
		journalVolDir: t.TempDir(),
		hostProc:      t.TempDir(),
		hostRoot:      t.TempDir(),
		hostRasdaemon: t.TempDir(),
		stateDir:      t.TempDir(),
	}
	mustWrite(t, filepath.Join(tr.hostProc, "meminfo"), "MemTotal:       32784332 kB\nMemAvailable:   19233104 kB\nMemFree:         2113044 kB\nSwapTotal:       8388604 kB\nSwapFree:        8388604 kB\nDirty:              1284 kB\n")
	mustWrite(t, filepath.Join(tr.hostProc, "loadavg"), "0.72 0.61 0.55 2/431 12345\n")
	mustWrite(t, filepath.Join(tr.hostProc, "uptime"), "4127883.12 98765.43\n")
	mustWrite(t, filepath.Join(tr.hostProc, "mounts"), fmt.Sprintf("rootdev / ext4 rw 0 0\nproc /proc proc rw 0 0\n"))
	mustWrite(t, filepath.Join(tr.hostProc, "net", "tcp"), procNetHeader)
	mustWrite(t, filepath.Join(tr.hostProc, "net", "tcp6"), procNetHeader)
	mustWrite(t, filepath.Join(tr.hostProc, "net", "udp"), procNetHeader)
	mustWrite(t, filepath.Join(tr.hostProc, "net", "udp6"), procNetHeader)
	mustWrite(t, filepath.Join(tr.hostProc, "spl", "kstat", "zfs", "arcstats"), sampleArcstats)
	if err := os.MkdirAll(filepath.Join(tr.hostProc, "spl", "kstat", "zfs", "hotstore"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Real Linux kstat named-table format: 2 header lines, then
	// "<name> <type> <value>" rows — readPoolKstat/parseKstatFile parse
	// this the same way readArcStats parses arcstats.
	mustWrite(t, filepath.Join(tr.hostProc, "spl", "kstat", "zfs", "hotstore", "state"),
		"7 1 0x01 1 80 12345678 1234567890\nname                            type data\nstate                           4    0\n")
	mustWrite(t, filepath.Join(tr.hostProc, "spl", "kstat", "zfs", "hotstore", "io"),
		"7 1 0x01 1 80 12345678 1234567890\nname                            type data\nreads                           4    88213\nwrites                          4    12094\n")
	return tr
}

func newConfig(t *testing.T, tr tree) *config.Config {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(wd, "testdata", "bin")
	t.Setenv("PATH", stub+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STATE_DIR", tr.stateDir)
	t.Setenv("HOST_JOURNAL_DIR", tr.journalDir)
	t.Setenv("HOST_JOURNAL_VOLATILE_DIR", tr.journalVolDir)
	t.Setenv("HOST_PROC", tr.hostProc)
	t.Setenv("HOST_ROOT", tr.hostRoot)
	t.Setenv("HOST_RASDAEMON", tr.hostRasdaemon)
	t.Setenv("SENTINEL_HOSTNAME", "test-host")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// pathWithoutSensors drops any PATH entry that has a "sensors" executable
// in it, so TestSectionFailureIsolated stays hermetic even on a host that
// happens to have lm-sensors installed.
func pathWithoutSensors(path string) string {
	var kept []string
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "sensors")); err == nil {
			continue
		}
		kept = append(kept, dir)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

func journalRecord(tsMicro int64, priority int, ident, message string) map[string]any {
	return map[string]any{
		"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", tsMicro),
		"PRIORITY":             fmt.Sprintf("%d", priority),
		"SYSLOG_IDENTIFIER":    ident,
		"MESSAGE":              message,
	}
}

func writeJSONL(t *testing.T, path string, records []map[string]any) {
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
	mustWrite(t, path, buf.String())
}

// compileSchema loads the embedded facts.schema.json exactly as
// facts_test.go does — collect can't reuse that unexported helper across
// packages, so this is collect's own copy of the same two lines.
func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	schema, err := jsonschema.UnmarshalJSON(bytes.NewReader(facts.SchemaJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("facts.schema.json", schema); err != nil {
		t.Fatal(err)
	}
	sch, err := compiler.Compile("facts.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

func validateAgainstSchema(t *testing.T, b []byte) {
	t.Helper()
	sch := compileSchema(t)
	var inst any
	if err := json.Unmarshal(b, &inst); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("output does not validate against facts.schema.json: %v\n%s", err, b)
	}
}

// --- C1/C2: Run produces a valid, schema-conformant object ---

func TestRunProducesObject(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	f, err := Run(context.Background(), Options{Cfg: cfg, Seq: 7})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("output is not a JSON object: %v", err)
	}
}

func TestOutputValidatesAgainstSchema(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	f, err := Run(context.Background(), Options{Cfg: cfg, Seq: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	validateAgainstSchema(t, b)
}

// --- C3/D9: tick mode has all eight sections + meta, deep absent; deep
// mode has deep + meta only ---

func TestTickModeSectionSet(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kernel == nil || f.Ras == nil || f.Smart == nil || f.Sensors == nil ||
		f.ZFS == nil || f.Resources == nil || f.Services == nil || f.Network == nil {
		t.Fatalf("tick mode must carry all eight sections: %+v", f)
	}
	if f.Deep != nil {
		t.Fatalf("tick mode must not carry deep: %+v", f.Deep)
	}
	if f.Meta.Mode != "tick" {
		t.Errorf("meta.mode = %q, want tick", f.Meta.Mode)
	}

	fd, err := Run(context.Background(), Options{Cfg: cfg, DeepComponent: "kernel"})
	if err != nil {
		t.Fatal(err)
	}
	if fd.Deep == nil {
		t.Fatal("deep mode must carry deep")
	}
	if fd.Kernel != nil || fd.Ras != nil || fd.Smart != nil || fd.Sensors != nil ||
		fd.ZFS != nil || fd.Resources != nil || fd.Services != nil || fd.Network != nil {
		t.Fatalf("deep mode must not carry any tick section: %+v", fd)
	}
	if fd.Meta.Mode != "deep" || fd.Meta.DeepComponent == nil || *fd.Meta.DeepComponent != "kernel" {
		t.Errorf("meta = %+v, want mode=deep deep_component=kernel", fd.Meta)
	}
}

// --- C4: schema rejects malformed facts ---

func TestSchemaRejectsMalformedFacts(t *testing.T) {
	sch := compileSchema(t)
	base := func() map[string]any {
		return map[string]any{
			"meta": map[string]any{
				"schema_version": "1", "hostname": "h", "timestamp": "2026-01-01T00:00:00Z",
				"tick_seq": 1, "mode": "tick", "deep_component": nil, "window": "10m",
				"duration_ms": 1, "truncated": false, "collector_errors": []any{},
			},
		}
	}
	cases := []struct {
		name string
		doc  map[string]any
	}{
		{"priority as string", func() map[string]any {
			d := base()
			d["kernel"] = map[string]any{
				"count": 1, "truncated": false, "dropped_entries": 0,
				"entries": []any{map[string]any{"ts": "2026-01-01T00:00:00Z", "priority": "3", "identifier": "-", "unit": nil, "message": "m"}},
			}
			return d
		}()},
		{"size_kb as string", func() map[string]any {
			d := base()
			d["resources"] = map[string]any{
				"truncated": false, "dropped_entries": 0,
				"filesystems": []any{map[string]any{"mount": "/", "source": "x", "size_kb": "1", "used_kb": 1, "avail_kb": 1, "use_percent": 1}},
				"memory_kb":   map[string]any{}, "load": map[string]any{"load1": 0.1, "load5": 0.1, "load15": 0.1, "running": 1, "total_procs": 1},
				"uptime_seconds": 1,
			}
			return d
		}()},
		{"section carries both error and data", func() map[string]any {
			d := base()
			d["sensors"] = map[string]any{"error": "boom", "truncated": false, "dropped_entries": 0, "chips": map[string]any{}}
			return d
		}()},
		{"truncated true with dropped_entries 0", func() map[string]any {
			d := base()
			d["kernel"] = map[string]any{"count": 0, "truncated": true, "dropped_entries": 0, "entries": []any{}}
			return d
		}()},
		{"collector_errors as strings", func() map[string]any {
			d := base()
			d["meta"].(map[string]any)["collector_errors"] = []any{"oops"}
			return d
		}()},
		{"unit missing", func() map[string]any {
			d := base()
			d["kernel"] = map[string]any{
				"count": 1, "truncated": false, "dropped_entries": 0,
				"entries": []any{map[string]any{"ts": "2026-01-01T00:00:00Z", "priority": 3, "identifier": "-", "message": "m"}},
			}
			return d
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := sch.Validate(tc.doc); err == nil {
				t.Fatalf("schema accepted malformed document (%s): %+v", tc.name, tc.doc)
			}
		})
	}
}

// --- C6: a missing section binary fails only its own section ---

func TestSectionFailureIsolated(t *testing.T) {
	basePath := os.Getenv("PATH") // real /bin etc., for the stub's own cat/sleep
	tr := newTree(t)
	cfg := newConfig(t, tr)

	writeJSONL(t, filepath.Join(tr.journalDir, "kernel.jsonl"), []map[string]any{
		journalRecord(1755250304000000, 2, "kernel", "SENTINEL-TEST critical line"),
	})

	// PATH now contains journalctl but not sensors: a dedicated dir with
	// only the journalctl stub symlinked in, ahead of the real PATH so
	// exec.LookPath("sensors") finds nothing (dev/CI hosts don't ship
	// lm-sensors) while the stub script's own cat/sleep still resolve.
	wd, _ := os.Getwd()
	noSensorsDir := t.TempDir()
	if err := os.Symlink(filepath.Join(wd, "testdata", "bin", "journalctl"), filepath.Join(noSensorsDir, "journalctl")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", noSensorsDir+string(os.PathListSeparator)+pathWithoutSensors(basePath))

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("Run must not fail when one binary is missing: %v", err)
	}
	if f.Sensors == nil || f.Sensors.Err == "" {
		t.Fatalf("Sensors section should carry an error, got %+v", f.Sensors)
	}
	found := false
	for _, ce := range f.Meta.CollectorErrors {
		if ce.Section == "sensors" && ce.ExitCode == 127 {
			found = true
		}
	}
	if !found {
		t.Errorf("meta.collector_errors should have a sensors/127 entry: %+v", f.Meta.CollectorErrors)
	}
	if f.Kernel == nil || f.Kernel.Err != "" {
		t.Fatalf("Kernel section must still carry data: %+v", f.Kernel)
	}
	if len(f.Kernel.Data.Entries) != 1 {
		t.Errorf("Kernel entries = %+v, want 1", f.Kernel.Data.Entries)
	}
}

// --- C7: per-section timeout, not cumulative ---

func TestSectionTimeout(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	cfg.SectionTimeout = 1 * time.Second
	mustWrite(t, filepath.Join(tr.stateDir, ".unused"), "") // keep stateDir non-empty is irrelevant; just ensures dir exists

	t.Setenv("SENSORS_SLEEP", "30")

	start := time.Now()
	f, err := Run(context.Background(), Options{Cfg: cfg})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.Sensors == nil || f.Sensors.Err == "" {
		t.Fatalf("Sensors should have timed out: %+v", f.Sensors)
	}
	if !bytes.Contains([]byte(f.Sensors.Err), []byte("timeout")) {
		t.Errorf("Sensors.Err = %q, want it to mention timeout", f.Sensors.Err)
	}
	var code int
	for _, ce := range f.Meta.CollectorErrors {
		if ce.Section == "sensors" {
			code = ce.ExitCode
		}
	}
	if code != 124 {
		t.Errorf("sensors exit_code = %d, want 124", code)
	}
	if elapsed >= 15*time.Second {
		t.Errorf("Run took %v — timeout is not per-section", elapsed)
	}
}

// --- C8/C9/C10: truncation ---

func TestTruncationBudget(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)

	var records []map[string]any
	base := int64(1755250000000000)
	for i := 0; i < 300; i++ {
		records = append(records, journalRecord(base+int64(i)*1000000, 6,
			"kernel", fmt.Sprintf("noncritical padding line number %04d filler filler filler", i)))
	}
	writeJSONL(t, filepath.Join(tr.journalDir, "kernel.jsonl"), records)
	cfg.FactsMaxBytes = 8192

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > cfg.FactsMaxBytes {
		t.Errorf("output len %d exceeds FACTS_MAX_BYTES %d", len(b), cfg.FactsMaxBytes)
	}
	if !f.Meta.Truncated {
		t.Error("meta.truncated should be true")
	}
	anyTruncated := false
	if f.Kernel != nil && f.Kernel.Err == "" && f.Kernel.Data.Truncated && f.Kernel.Data.DroppedEntries > 0 {
		anyTruncated = true
		if f.Kernel.Data.Count != len(f.Kernel.Data.Entries)+f.Kernel.Data.DroppedEntries {
			t.Errorf("count invariant broken: count=%d entries=%d dropped=%d",
				f.Kernel.Data.Count, len(f.Kernel.Data.Entries), f.Kernel.Data.DroppedEntries)
		}
	}
	if !anyTruncated {
		t.Error("expected at least one truncated section with dropped_entries > 0")
	}
	validateAgainstSchema(t, b)
}

func TestTruncationKeepsOldest(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)

	var records []map[string]any
	base := int64(1755250000000000)
	for i := 0; i < 100; i++ {
		records = append(records, journalRecord(base+int64(i)*1000000, 6,
			"kernel", fmt.Sprintf("padding line %04d filler filler filler filler filler", i)))
	}
	writeJSONL(t, filepath.Join(tr.journalDir, "kernel.jsonl"), records)

	untruncated, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	firstTS := untruncated.Kernel.Data.Entries[0].TS

	cfg.FactsMaxBytes = 6000
	truncated, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(truncated.Kernel.Data.Entries) == 0 {
		t.Fatal("expected at least one surviving entry")
	}
	if truncated.Kernel.Data.Entries[0].TS != firstTS {
		t.Errorf("oldest entry ts changed: got %s want %s", truncated.Kernel.Data.Entries[0].TS, firstTS)
	}
}

// D2, normal-loop case: with <= RAW_ALERT_MAX_LINES protected entries
// planted, the normal §5 step-2 loop has plenty of unprotected filler to
// drop first — every protected entry must survive, exactly, not just
// "more than zero".
func TestTruncationProtectsCriticalNormalLoop(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)

	const wantProtected = 10 // <= default RAW_ALERT_MAX_LINES (20)
	var records []map[string]any
	base := int64(1755250000000000)
	for i := 0; i < 300; i++ {
		pri, msg := 6, fmt.Sprintf("noncritical filler line %04d filler filler filler filler", i)
		if i%30 == 0 {
			pri, msg = cfg.RawAlertMaxPriority, fmt.Sprintf("PROTECTED critical line %04d", i)
		}
		records = append(records, journalRecord(base+int64(i)*1000000, pri, "kernel", msg))
	}
	writeJSONL(t, filepath.Join(tr.journalDir, "kernel.jsonl"), records)

	cfg.FactsMaxBytes = 8192 // enough filler exists to reach budget without touching protected entries
	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kernel == nil || f.Kernel.Err != "" {
		t.Fatalf("kernel section failed: %+v", f.Kernel)
	}
	if !f.Kernel.Data.Truncated {
		t.Fatal("test setup did not actually truncate — assertions below would be vacuous")
	}
	survivors := 0
	for _, e := range f.Kernel.Data.Entries {
		if e.Priority <= cfg.RawAlertMaxPriority {
			survivors++
		}
	}
	if survivors != wantProtected {
		t.Errorf("protected entries surviving = %d, want %d (every one of them)", survivors, wantProtected)
	}
	for _, ce := range f.Meta.CollectorErrors {
		if ce.Section == "*" {
			t.Error("hard truncation fired — test setup should have stayed in the normal loop")
		}
	}
}

// D2, hard-truncation case: with more than RAW_ALERT_MAX_LINES protected
// entries and nothing droppable, §5 step 3 fires: kernel.entries caps to
// exactly the RAW_ALERT_MAX_LINES newest protected entries.
func TestTruncationProtectsCriticalHardTruncation(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)

	const total = 35 // > default RAW_ALERT_MAX_LINES (20)
	var records []map[string]any
	base := int64(1755250000000000)
	for i := 0; i < total; i++ {
		records = append(records, journalRecord(base+int64(i)*1000000, cfg.RawAlertMaxPriority,
			fmt.Sprintf("kernel-%03d", i), fmt.Sprintf("PROTECTED critical line %04d filler filler filler filler", i)))
	}
	writeJSONL(t, filepath.Join(tr.journalDir, "kernel.jsonl"), records)

	cfg.FactsMaxBytes = 512 // every entry is protected: unreachable without hard truncation
	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kernel == nil || f.Kernel.Err != "" {
		t.Fatalf("kernel section failed: %+v", f.Kernel)
	}
	hardTruncated := false
	for _, ce := range f.Meta.CollectorErrors {
		if ce.Section == "*" {
			hardTruncated = true
		}
	}
	if !hardTruncated {
		t.Fatal("test setup did not reach the hard-truncation fixed point")
	}
	if len(f.Kernel.Data.Entries) != cfg.RawAlertMaxLines {
		t.Fatalf("kernel.entries survivors = %d, want exactly RAW_ALERT_MAX_LINES = %d", len(f.Kernel.Data.Entries), cfg.RawAlertMaxLines)
	}
	// entries were collected in ascending ts order, so the newest
	// RAW_ALERT_MAX_LINES are the LAST cfg.RawAlertMaxLines of the planted
	// records — assert the survivors are exactly that tail, in order.
	wantFirstIdent := fmt.Sprintf("kernel-%03d", total-cfg.RawAlertMaxLines)
	wantLastIdent := fmt.Sprintf("kernel-%03d", total-1)
	if f.Kernel.Data.Entries[0].Identifier != wantFirstIdent {
		t.Errorf("oldest survivor = %q, want %q (the newest-%d cut, not an arbitrary subset)",
			f.Kernel.Data.Entries[0].Identifier, wantFirstIdent, cfg.RawAlertMaxLines)
	}
	if last := f.Kernel.Data.Entries[len(f.Kernel.Data.Entries)-1].Identifier; last != wantLastIdent {
		t.Errorf("newest survivor = %q, want %q", last, wantLastIdent)
	}
}

// --- C11: ZFS CKSUM fixture (ARCHITECTURE §2.7) ---

func TestZFSCksumFixture(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	writeJSONL(t, filepath.Join(tr.journalDir, "zed.jsonl"), []map[string]any{
		journalRecord(1755250323000000, 4, "zed", "eid=1841 class=checksum pool='hotstore' vdev=seagate-zvtazeam-crypt cksum_errors=1"),
	})

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.ZFS == nil || f.ZFS.Err != "" {
		t.Fatalf("zfs section failed: %+v", f.ZFS)
	}
	if len(f.ZFS.Data.Events) != 1 || f.ZFS.Data.Events[0].Message != "eid=1841 class=checksum pool='hotstore' vdev=seagate-zvtazeam-crypt cksum_errors=1" {
		t.Errorf("zfs events = %+v", f.ZFS.Data.Events)
	}
	found := false
	for _, p := range f.ZFS.Data.Pools {
		if p == "hotstore" {
			found = true
		}
	}
	if !found {
		t.Errorf("pools = %v, want hotstore", f.ZFS.Data.Pools)
	}
	if f.ZFS.Data.Arc["size"] != 8523441152 {
		t.Errorf("arc.size = %d, want 8523441152", f.ZFS.Data.Arc["size"])
	}
}

// --- C12: network baseline lifecycle ---

func TestNetworkBaseline(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	mustWrite(t, filepath.Join(tr.hostProc, "net", "tcp"), procNetHeader+
		"   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0 0 10 0 0 0 0\n")

	f1, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f1.Network == nil || f1.Network.Err != "" {
		t.Fatalf("network section failed: %+v", f1.Network)
	}
	if !f1.Network.Data.BaselineInitialized {
		t.Error("first run: baseline_initialized should be true (freshly created)")
	}
	if len(f1.Network.Data.NewListeners) != 0 {
		t.Errorf("first run: new_listeners should be empty, got %+v", f1.Network.Data.NewListeners)
	}
	if _, err := os.Stat(filepath.Join(tr.stateDir, "baseline-ports")); err != nil {
		t.Fatalf("baseline-ports not created: %v", err)
	}

	// second run, same listener set: baseline now exists.
	f2, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f2.Network.Data.BaselineInitialized {
		t.Error("second run: baseline_initialized should be false (baseline already existed)")
	}

	// third run: add a new listener.
	mustWrite(t, filepath.Join(tr.hostProc, "net", "tcp"), procNetHeader+
		"   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1 0 0 10 0 0 0 0\n"+
		"   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12346 1 0 0 10 0 0 0 0\n")
	f3, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(f3.Network.Data.NewListeners) != 1 || f3.Network.Data.NewListeners[0].Port != 8080 {
		t.Errorf("new_listeners = %+v, want port 8080", f3.Network.Data.NewListeners)
	}

	// fourth run: remove the original listener.
	mustWrite(t, filepath.Join(tr.hostProc, "net", "tcp"), procNetHeader+
		"   1: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12346 1 0 0 10 0 0 0 0\n")
	f4, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	closedFound := false
	for _, l := range f4.Network.Data.ClosedListeners {
		if l.Port == 22 {
			closedFound = true
		}
	}
	if !closedFound {
		t.Errorf("closed_listeners = %+v, want port 22 present", f4.Network.Data.ClosedListeners)
	}

	// read-only STATE_DIR: never fatal.
	roTree := newTree(t)
	roCfg := newConfig(t, roTree)
	if err := os.Chmod(roTree.stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(roTree.stateDir, 0o700)
	fr, err := Run(context.Background(), Options{Cfg: roCfg})
	if err != nil {
		t.Fatalf("Run must not fail on a read-only STATE_DIR: %v", err)
	}
	if fr.Network == nil || fr.Network.Err != "" {
		t.Fatalf("network section must stay healthy on a read-only STATE_DIR: %+v", fr.Network)
	}
	foundNetErr := false
	for _, ce := range fr.Meta.CollectorErrors {
		if ce.Section == "network" {
			foundNetErr = true
		}
	}
	if !foundNetErr {
		t.Error("expected one collector_errors[] entry for the failed baseline write")
	}
	// collect.md §2/§7: baseline not created ⇒ baseline_initialized stays
	// false — a write failure must not report a baseline that was never
	// actually written.
	if fr.Network.Data.BaselineInitialized {
		t.Error("baseline_initialized must be false when the baseline write failed")
	}
}

// --- C13: tick_seq is an argument, never a file ---

func TestTickSeqIsAnArgument(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)

	f, err := Run(context.Background(), Options{Cfg: cfg, Seq: 412})
	if err != nil {
		t.Fatal(err)
	}
	if f.Meta.TickSeq != 412 {
		t.Errorf("meta.tick_seq = %d, want 412", f.Meta.TickSeq)
	}
	fd, err := Run(context.Background(), Options{Cfg: cfg, Seq: 413, DeepComponent: "kernel"})
	if err != nil {
		t.Fatal(err)
	}
	if fd.Meta.TickSeq != 413 {
		t.Errorf("deep meta.tick_seq = %d, want 413", fd.Meta.TickSeq)
	}
	if _, err := os.Stat(filepath.Join(tr.stateDir, "tick-seq")); err == nil {
		t.Fatal("collect must never create $STATE_DIR/tick-seq (D8)")
	}
}

// --- C14: deep mode ---

func TestDeepMode(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	writeJSONL(t, filepath.Join(tr.journalDir, "zed.jsonl"), []map[string]any{
		journalRecord(1755250323000000, 4, "zed", "eid=1841 class=checksum pool='hotstore' cksum_errors=1"),
	})

	histTS := int64(1755250202)
	histName := fmt.Sprintf("%010d-000412.json", histTS)
	mustWrite(t, filepath.Join(tr.stateDir, "history", histName),
		`{"status":"WATCH","headline":"Single checksum error on hotstore","findings":[{"severity":"warning","component":"zfs","evidence":"e","explanation":"x"}]}`)

	f, err := Run(context.Background(), Options{Cfg: cfg, DeepComponent: "zfs"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Meta.Mode != "deep" || f.Meta.DeepComponent == nil || *f.Meta.DeepComponent != "zfs" {
		t.Fatalf("meta = %+v", f.Meta)
	}
	if f.Deep == nil || f.Deep.Err != "" {
		t.Fatalf("deep section failed: %+v", f.Deep)
	}
	if f.Deep.Data.Component != "zfs" {
		t.Errorf("deep.component = %q, want zfs", f.Deep.Data.Component)
	}
	if len(f.Deep.Data.ZedEvents) != 1 {
		t.Errorf("zed_events = %+v", f.Deep.Data.ZedEvents)
	}
	if f.Deep.Data.SmartEntries == nil || f.Deep.Data.KernelEntries == nil {
		t.Errorf("smart_entries/kernel_entries must be arrays, got %+v / %+v", f.Deep.Data.SmartEntries, f.Deep.Data.KernelEntries)
	}
	if len(f.Deep.Data.History) != 1 {
		t.Fatalf("history = %+v, want 1 entry", f.Deep.Data.History)
	}
	wantTS := time.Unix(histTS, 0).UTC().Format(time.RFC3339)
	if f.Deep.Data.History[0].TS != wantTS {
		t.Errorf("history[0].ts = %q, want %q (from filename)", f.Deep.Data.History[0].TS, wantTS)
	}
	// §3b: pool_kstat is a flat merged map of every file's kstat rows
	// under the pool dir, not filename->wholefile-content.
	hotstore, ok := f.Deep.Data.PoolKstat["hotstore"]
	if !ok {
		t.Fatalf("pool_kstat missing hotstore: %+v", f.Deep.Data.PoolKstat)
	}
	if hotstore["state"] != int64(0) || hotstore["reads"] != int64(88213) || hotstore["writes"] != int64(12094) {
		t.Errorf("pool_kstat[hotstore] = %+v, want state=0 reads=88213 writes=12094", hotstore)
	}

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	validateAgainstSchema(t, b)

	// runs under DEEP_TIMEOUT, not SECTION_TIMEOUT
	cfg.SectionTimeout = 1 * time.Second
	cfg.DeepTimeout = 10 * time.Second
	t.Setenv("SENSORS_SLEEP", "0") // unrelated to deep, but keep other tests hermetic
	fd2, err := Run(context.Background(), Options{Cfg: cfg, DeepComponent: "kernel"})
	if err != nil {
		t.Fatal(err)
	}
	if fd2.Deep == nil || fd2.Deep.Err != "" {
		t.Fatalf("deep kernel failed under DEEP_TIMEOUT: %+v", fd2.Deep)
	}
}

// --- C17: read-only proof ---

func TestReadOnlyProof(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)

	type snap struct {
		size  int64
		mtime time.Time
	}
	before := map[string]snap{}
	roots := []string{tr.journalDir, tr.journalVolDir, tr.hostProc, tr.hostRoot, tr.hostRasdaemon}
	for _, root := range roots {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			before[path] = snap{info.Size(), info.ModTime()}
			return nil
		})
	}

	if _, err := Run(context.Background(), Options{Cfg: cfg}); err != nil {
		t.Fatal(err)
	}

	for _, root := range roots {
		filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			b, ok := before[path]
			if !ok {
				t.Errorf("new file created under a read-only mount: %s", path)
				return nil
			}
			if b.size != info.Size() || !b.mtime.Equal(info.ModTime()) {
				t.Errorf("file modified under a read-only mount: %s", path)
			}
			return nil
		})
	}

	// only $STATE_DIR/baseline-ports should have been written.
	entries, err := os.ReadDir(tr.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "baseline-ports" {
		t.Errorf("$STATE_DIR contents = %v, want only baseline-ports", entries)
	}
}

// --- C18: services section ---

func TestServicesSection(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	writeJSONL(t, filepath.Join(tr.journalDir, "services.jsonl"), []map[string]any{
		{
			"__REALTIME_TIMESTAMP": "1755250390000000", "PRIORITY": "3",
			"SYSLOG_IDENTIFIER": "smbd", "_SYSTEMD_UNIT": "smbd.service", "_TRANSPORT": "syslog",
			"MESSAGE": "Failed to start Samba SMB Daemon.",
		},
		{
			"__REALTIME_TIMESTAMP": "1755250392000000", "PRIORITY": "3",
			"SYSLOG_IDENTIFIER": "smbd", "_SYSTEMD_UNIT": "smbd.service", "_TRANSPORT": "syslog",
			"MESSAGE": "smbd: connection reset by peer",
		},
		{
			"__REALTIME_TIMESTAMP": "1755250395000000", "PRIORITY": "3",
			"SYSLOG_IDENTIFIER": "kernel", "_TRANSPORT": "kernel",
			"MESSAGE": "kernel error, must not appear in services (covered by §1)",
		},
	})

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Services == nil || f.Services.Err != "" {
		t.Fatalf("services section failed: %+v", f.Services)
	}
	if len(f.Services.Data.FailedUnits) != 1 || f.Services.Data.FailedUnits[0] != "smbd.service" {
		t.Errorf("failed_units = %v, want [smbd.service]", f.Services.Data.FailedUnits)
	}
	for _, e := range f.Services.Data.Entries {
		if e.Identifier == "kernel" {
			t.Errorf("kernel-transport record leaked into services: %+v", e)
		}
	}

	// per-section SERVICES_MAX_BYTES budget
	var many []map[string]any
	base := int64(1755250000000000)
	for i := 0; i < 200; i++ {
		many = append(many, map[string]any{
			"__REALTIME_TIMESTAMP": fmt.Sprintf("%d", base+int64(i)*1000000),
			"PRIORITY":             "3", "SYSLOG_IDENTIFIER": "svc", "_TRANSPORT": "syslog",
			"MESSAGE": fmt.Sprintf("some noncritical service error line number %04d filler filler", i),
		})
	}
	writeJSONL(t, filepath.Join(tr.journalDir, "services.jsonl"), many)
	cfg.ServicesMaxBytes = 512
	f2, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f2.Services == nil || f2.Services.Err != "" {
		t.Fatalf("services section failed: %+v", f2.Services)
	}
	if !f2.Services.Data.Truncated || f2.Services.Data.DroppedEntries == 0 {
		t.Errorf("services should be truncated under a tiny SERVICES_MAX_BYTES: %+v", f2.Services.Data)
	}
	if f2.Services.Data.Count != len(f2.Services.Data.Entries)+f2.Services.Data.DroppedEntries {
		t.Errorf("count invariant broken: %+v", f2.Services.Data)
	}
}

// --- C19: all sections fail ---

func TestAllSectionsFail(t *testing.T) {
	tr := tree{
		journalDir:    filepath.Join(t.TempDir(), "missing-journal"),
		journalVolDir: filepath.Join(t.TempDir(), "missing-journal-vol"),
		hostProc:      filepath.Join(t.TempDir(), "missing-proc"),
		hostRoot:      filepath.Join(t.TempDir(), "missing-root"),
		hostRasdaemon: filepath.Join(t.TempDir(), "missing-rasdaemon"),
		stateDir:      t.TempDir(),
	}
	noBinDir := t.TempDir() // neither journalctl nor sensors present
	t.Setenv("PATH", noBinDir)
	t.Setenv("STATE_DIR", tr.stateDir)
	t.Setenv("HOST_JOURNAL_DIR", tr.journalDir)
	t.Setenv("HOST_JOURNAL_VOLATILE_DIR", tr.journalVolDir)
	t.Setenv("HOST_PROC", tr.hostProc)
	t.Setenv("HOST_ROOT", tr.hostRoot)
	t.Setenv("HOST_RASDAEMON", tr.hostRasdaemon)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatalf("Run must still succeed: %v", err)
	}
	if f.Kernel == nil || f.Kernel.Err == "" {
		t.Errorf("kernel should have failed, got %+v", f.Kernel)
	}
	if f.Smart == nil || f.Smart.Err == "" {
		t.Errorf("smart should have failed, got %+v", f.Smart)
	}
	if f.Ras == nil || f.Ras.Err == "" {
		t.Error("ras should have failed")
	}
	if f.Sensors == nil || f.Sensors.Err == "" {
		t.Error("sensors should have failed")
	}
	if f.ZFS == nil || f.ZFS.Err == "" {
		t.Error("zfs should have failed")
	}
	if f.Resources == nil || f.Resources.Err == "" {
		t.Error("resources should have failed")
	}
	if f.Services == nil || f.Services.Err == "" {
		t.Error("services should have failed")
	}
	if f.Network == nil || f.Network.Err == "" {
		t.Error("network should have failed")
	}
	if len(f.Meta.CollectorErrors) < 8 {
		t.Errorf("collector_errors = %+v, want at least 8", f.Meta.CollectorErrors)
	}

	// §4: "A failed section always has a matching meta.collector_errors[]
	// entry with the identical reason."
	byName := map[string]string{}
	for _, ce := range f.Meta.CollectorErrors {
		byName[ce.Section] = ce.Reason
	}
	assertSameReason := func(section, sectionErr string) {
		t.Helper()
		if sectionErr == "" {
			return
		}
		got, ok := byName[section]
		if !ok {
			t.Errorf("%s: no matching collector_errors[] entry", section)
			return
		}
		if got != sectionErr {
			t.Errorf("%s: section.Err = %q, collector_errors[].reason = %q — must be identical", section, sectionErr, got)
		}
	}
	assertSameReason("kernel", f.Kernel.Err)
	assertSameReason("smart", f.Smart.Err)
	assertSameReason("ras", f.Ras.Err)
	assertSameReason("sensors", f.Sensors.Err)
	assertSameReason("zfs", f.ZFS.Err)
	assertSameReason("resources", f.Resources.Err)
	assertSameReason("services", f.Services.Err)
	assertSameReason("network", f.Network.Err)

	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	validateAgainstSchema(t, b)
}

// --- injected kernel error (container-only) ---

// C5: with a real journald present (SENTINEL_CONTAINER=1 — set by the T7
// container test target, never locally), an injected kern.err line must
// show up in kernel.entries within one collect.Run. Loudly skipped
// otherwise per C9 ("must t.Skip loudly, never pass silently") — this
// must actually run under SENTINEL_CONTAINER=1, not stay a permanent stub.
func TestInjectedKernelError(t *testing.T) {
	if os.Getenv("SENTINEL_CONTAINER") != "1" {
		t.Skip("SENTINEL_CONTAINER=1 not set — requires a real journald")
	}
	marker := fmt.Sprintf("SENTINEL-TEST-%d", os.Getpid())
	if err := exec.Command("logger", "-p", "kern.err", marker).Run(); err != nil {
		t.Fatalf("logger: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kernel == nil || f.Kernel.Err != "" {
		t.Fatalf("kernel section failed: %+v", f.Kernel)
	}
	for _, e := range f.Kernel.Data.Entries {
		if strings.Contains(e.Message, marker) {
			return
		}
	}
	t.Errorf("marker %q not found in kernel.entries: %+v", marker, f.Kernel.Data.Entries)
}

// --- agy fix round 1 ---

// collect.md §5 step 3: hitting the hard-truncation fixed point must set
// meta.truncated: true — this is the exact state where the analyzer most
// needs to know the picture is incomplete. Exercised directly against
// Truncate() with no sections present at all, so there is nothing for
// hardTruncate to reduce and no section-level Truncated flag can end up
// true by coincidence — the only way this passes is if hitting the fixed
// point itself sets meta.truncated.
func TestHardTruncationSetsMetaTruncated(t *testing.T) {
	cfg := &config.Config{FactsMaxBytes: 10, RawAlertMaxPriority: 2, RawAlertMaxLines: 20}
	f := &facts.Facts{Meta: facts.Meta{CollectorErrors: []facts.CollectorError{}}}

	Truncate(f, cfg)

	hardTruncationHappened := false
	for _, ce := range f.Meta.CollectorErrors {
		if ce.Section == "*" {
			hardTruncationHappened = true
		}
	}
	if !hardTruncationHappened {
		t.Fatal("test setup did not reach the hard-truncation fixed point")
	}
	if !f.Meta.Truncated {
		t.Error("meta.truncated must be true when hard truncation fired (§5 step 3), even with no section left to flag")
	}
}

// collect.md §7: journalctl stderr is captured into the error reason
// (first 200 bytes) — it's exactly where a permission or group_add
// problem on the target host would announce itself.
func TestExitErrorReasonIncludesStderr(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	// Both dirs must fail: since one directory failing now tolerates the
	// other (§3 "one directory failing does not discard the other"), the
	// kernel section only fails outright when neither dir succeeds.
	for _, dir := range []string{tr.journalDir, tr.journalVolDir} {
		mustWrite(t, filepath.Join(dir, ".stderr"), "Failed to open journal: Permission denied\n")
		mustWrite(t, filepath.Join(dir, ".exit"), "1")
	}

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kernel == nil || f.Kernel.Err == "" {
		t.Fatalf("kernel section should have failed: %+v", f.Kernel)
	}
	if !strings.Contains(f.Kernel.Err, "Permission denied") {
		t.Errorf("Kernel.Err = %q, want it to include the captured journalctl stderr", f.Kernel.Err)
	}
	found := false
	for _, ce := range f.Meta.CollectorErrors {
		if ce.Section == "kernel" {
			found = true
			if !strings.Contains(ce.Reason, "Permission denied") {
				t.Errorf("collector_errors[kernel].reason = %q, want it to include the captured stderr", ce.Reason)
			}
		}
	}
	if !found {
		t.Fatal("no collector_errors entry for kernel")
	}
}

// collect.md §4: meta.window examples are "10m" / "24h" — Duration.String()
// would emit "10m0s" / "24h0m0s".
func TestMetaWindowFormat(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Meta.Window != "10m" {
		t.Errorf("tick meta.window = %q, want %q", f.Meta.Window, "10m")
	}

	fd, err := Run(context.Background(), Options{Cfg: cfg, DeepComponent: "kernel"})
	if err != nil {
		t.Fatal(err)
	}
	if fd.Meta.Window != "24h" {
		t.Errorf("deep meta.window = %q, want %q", fd.Meta.Window, "24h")
	}
}

// --- second agy round: absent optional sources, remote-fs skip, record cap ---

// collect.md §3 "Absent optional sources": $HOST_RASDAEMON not existing
// (rasdaemon not installed) must not fail the ras section — store: [],
// journal-backed entries[] still collected.
func TestRasAbsentRasdaemonIsNotAFailure(t *testing.T) {
	tr := newTree(t)
	if err := os.RemoveAll(tr.hostRasdaemon); err != nil {
		t.Fatal(err)
	}
	cfg := newConfig(t, tr)
	writeJSONL(t, filepath.Join(tr.journalDir, "ras.jsonl"), []map[string]any{
		journalRecord(1755250304000000, 4, "rasdaemon", "some RAS event"),
	})

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Ras == nil || f.Ras.Err != "" {
		t.Fatalf("ras section must stay healthy when $HOST_RASDAEMON is absent: %+v", f.Ras)
	}
	if f.Ras.Data.Store == nil || len(f.Ras.Data.Store) != 0 {
		t.Errorf("store = %+v, want empty (never nil)", f.Ras.Data.Store)
	}
	if len(f.Ras.Data.Entries) != 1 {
		t.Errorf("ras journal entries should still be collected: %+v", f.Ras.Data.Entries)
	}
}

// collect.md §3 "Absent optional sources": no ZFS kstat tree (module not
// loaded) must not fail the zfs section — arc: {}, pools: [], zed
// events[] still collected.
func TestZFSAbsentKstatTreeIsNotAFailure(t *testing.T) {
	tr := newTree(t)
	if err := os.RemoveAll(filepath.Join(tr.hostProc, "spl")); err != nil {
		t.Fatal(err)
	}
	cfg := newConfig(t, tr)
	writeJSONL(t, filepath.Join(tr.journalDir, "zed.jsonl"), []map[string]any{
		journalRecord(1755250304000000, 4, "zed", "eid=1 class=checksum pool='hotstore'"),
	})

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.ZFS == nil || f.ZFS.Err != "" {
		t.Fatalf("zfs section must stay healthy with no ZFS kstat tree: %+v", f.ZFS)
	}
	if len(f.ZFS.Data.Arc) != 0 {
		t.Errorf("arc = %+v, want empty", f.ZFS.Data.Arc)
	}
	if f.ZFS.Data.Pools == nil || len(f.ZFS.Data.Pools) != 0 {
		t.Errorf("pools = %+v, want empty (never nil)", f.ZFS.Data.Pools)
	}
	if len(f.ZFS.Data.Events) != 1 {
		t.Errorf("zed events should still be collected: %+v", f.ZFS.Data.Events)
	}
}

// collect.md §3 "Absent optional sources": no tcp6/udp6 (IPv6 disabled)
// must not fail the network section — the IPv4 files still produce it.
func TestNetworkAbsentIPv6FilesIsNotAFailure(t *testing.T) {
	tr := newTree(t)
	if err := os.Remove(filepath.Join(tr.hostProc, "net", "tcp6")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(tr.hostProc, "net", "udp6")); err != nil {
		t.Fatal(err)
	}
	cfg := newConfig(t, tr)

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Network == nil || f.Network.Err != "" {
		t.Fatalf("network section must stay healthy with no tcp6/udp6: %+v", f.Network)
	}
}

// collect.md §3 row 6 note: remote filesystem types must be skipped, the
// same as the pseudo-fs set — this is what keeps a hung NFS/CIFS mount
// from putting syscall.Statfs into uninterruptible D-state.
func TestResourcesSkipsRemoteFilesystems(t *testing.T) {
	tr := newTree(t)
	mustWrite(t, filepath.Join(tr.hostProc, "mounts"),
		"rootdev / ext4 rw 0 0\nproc /proc proc rw 0 0\nnasbox:/export /mnt/nas nfs4 rw 0 0\n")
	// Create /mnt/nas under hostRoot for real so syscall.Statfs would
	// SUCCEED if the fstype skip were broken — a missing path would be
	// skipped either way (Statfs fails, §3 row 6's "failure ⇒ skipped"
	// rule), which would make this test pass without the remote-fs set
	// doing anything.
	if err := os.MkdirAll(filepath.Join(tr.hostRoot, "mnt", "nas"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := newConfig(t, tr)

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Resources == nil || f.Resources.Err != "" {
		t.Fatalf("resources section failed: %+v", f.Resources)
	}
	for _, fs := range f.Resources.Data.Filesystems {
		if fs.Mount == "/mnt/nas" {
			t.Errorf("nfs4 mount reached the output — remote-fs skip set is not applied: %+v", fs)
		}
	}
}

// New C3 variable JOURNAL_MAX_RECORDS reaches journal.Query.MaxRecords —
// exercised end to end through collect.Run rather than just journal.Run,
// since that's the actual wiring collect.md asks for.
func TestJournalMaxRecordsWiredThroughCollect(t *testing.T) {
	tr := newTree(t)
	var records []map[string]any
	base := int64(1755250000000000)
	for i := 0; i < 40; i++ {
		records = append(records, journalRecord(base+int64(i)*1000000, 3, "kernel", fmt.Sprintf("line %04d", i)))
	}
	writeJSONL(t, filepath.Join(tr.journalDir, "kernel.jsonl"), records)

	t.Setenv("JOURNAL_MAX_RECORDS", "10")
	cfg := newConfig(t, tr)
	if cfg.JournalMaxRecords != 10 {
		t.Fatalf("cfg.JournalMaxRecords = %d, want 10", cfg.JournalMaxRecords)
	}

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kernel == nil || f.Kernel.Err != "" {
		t.Fatalf("kernel section failed: %+v", f.Kernel)
	}
	if len(f.Kernel.Data.Entries) != 10 {
		t.Errorf("kernel.entries = %d, want 10 (JOURNAL_MAX_RECORDS)", len(f.Kernel.Data.Entries))
	}
	if f.Kernel.Data.DroppedEntries != 30 {
		t.Errorf("kernel.dropped_entries = %d, want 30", f.Kernel.Data.DroppedEntries)
	}
	if !f.Kernel.Data.Truncated {
		t.Error("kernel.truncated should be true")
	}
	if f.Kernel.Data.Count != 40 {
		t.Errorf("kernel.count = %d, want 40 (collected before any truncation)", f.Kernel.Data.Count)
	}
}

// collect.md §3 "One directory failing does not discard the other",
// wired end to end through collect.Run: a permission problem on the
// volatile journal must not discard kernel entries already collected
// from the persistent one, and must surface as a collector_errors[] row
// naming the failed directory rather than a section failure.
func TestKernelPartialDirectoryFailureStaysHealthy(t *testing.T) {
	tr := newTree(t)
	cfg := newConfig(t, tr)
	writeJSONL(t, filepath.Join(tr.journalDir, "kernel.jsonl"), []map[string]any{
		journalRecord(1755250304000000, 3, "kernel", "from the persistent journal"),
	})
	mustWrite(t, filepath.Join(tr.journalVolDir, ".exit"), "1")
	mustWrite(t, filepath.Join(tr.journalVolDir, ".stderr"), "permission denied on volatile journal\n")

	f, err := Run(context.Background(), Options{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kernel == nil || f.Kernel.Err != "" {
		t.Fatalf("kernel section must stay healthy when only one directory failed: %+v", f.Kernel)
	}
	if len(f.Kernel.Data.Entries) != 1 || f.Kernel.Data.Entries[0].Message != "from the persistent journal" {
		t.Fatalf("entries from the succeeding directory were discarded: %+v", f.Kernel.Data.Entries)
	}
	found := false
	for _, ce := range f.Meta.CollectorErrors {
		if ce.Section == "kernel" && strings.Contains(ce.Reason, tr.journalVolDir) {
			found = true
			if !strings.Contains(ce.Reason, "permission denied") {
				t.Errorf("collector_errors reason = %q, want it to include the captured stderr", ce.Reason)
			}
		}
	}
	if !found {
		t.Error("expected a collector_errors[] entry naming the failed volatile journal directory")
	}
}
