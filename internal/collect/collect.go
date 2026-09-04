// Package collect implements `sentinel collect` (contracts/collect.md):
// eight independent, timeout-isolated sections plus meta, assembled into
// facts.Facts and truncated deterministically to FACTS_MAX_BYTES.
package collect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/journal"
)

// ErrUnparseable marks a `sensors -j` invocation whose stdout is not valid
// JSON (contracts/collect.md §3 row 4).
var ErrUnparseable = errors.New("unparseable sensors output")

// Options configures one Run.
type Options struct {
	Cfg           *config.Config
	Seq           int64  // meta.tick_seq, collect neither reads nor writes tick-seq (D8)
	DeepComponent string // "" ⇒ tick mode; otherwise zfs|smart|kernel|ras, run under DEEP_TIMEOUT
}

// nowFn is overridden in tests via Options indirectly through Cfg.Now
// (config.Config.Now, C3: zero ⇒ live clock). Collect never calls
// time.Now() directly outside this one indirection point.
func now(cfg *config.Config) time.Time {
	if cfg.Now.IsZero() {
		return time.Now()
	}
	return cfg.Now
}

// Run collects everything, truncates, and returns the document. It
// returns an error only for internal failures (marshal/stdout write is
// the caller's concern, Run itself only assembles the struct).
// TickSections is how many facts sections a tick collects, each bounded
// by SECTION_TIMEOUT and run one after another: kernel, ras, smart,
// sensors, zfs, resources, services, network.
const TickSections = 8

func Run(ctx context.Context, o Options) (*facts.Facts, error) {
	start := time.Now()
	cfg := o.Cfg

	meta := facts.Meta{
		SchemaVersion:   facts.SchemaVersion,
		Hostname:        cfg.Hostname,
		TickSeq:         o.Seq,
		CollectorErrors: []facts.CollectorError{},
	}

	f := &facts.Facts{}

	if o.DeepComponent != "" {
		meta.Mode = "deep"
		dc := o.DeepComponent
		meta.DeepComponent = &dc
		meta.Window = cfg.DeepWindowRaw
		f.Deep = runSection(ctx, &meta, "deep", cfg.DeepTimeout, func(ctx context.Context) (facts.DeepData, error) {
			return collectDeep(ctx, cfg, &meta, o.DeepComponent)
		})
		if f.Deep.Err == "" {
			f.Deep.Data.Component = o.DeepComponent
		}
	} else {
		meta.Mode = "tick"
		meta.Window = cfg.TickWindowRaw

		// TickSections counts the runSection calls below. Keep it next to
		// them, state.HealthWindow budgets SECTION_TIMEOUT per section.
		f.Kernel = runSection(ctx, &meta, "kernel", cfg.SectionTimeout, func(ctx context.Context) (facts.KernelData, error) {
			return collectKernel(ctx, cfg, &meta)
		})
		f.Ras = runSection(ctx, &meta, "ras", cfg.SectionTimeout, func(ctx context.Context) (facts.RasData, error) {
			return collectRas(ctx, cfg, &meta)
		})
		f.Smart = runSection(ctx, &meta, "smart", cfg.SectionTimeout, func(ctx context.Context) (facts.SmartData, error) {
			return collectSmart(ctx, cfg, &meta)
		})
		f.Sensors = runSection(ctx, &meta, "sensors", cfg.SectionTimeout, func(ctx context.Context) (facts.SensorsData, error) {
			return collectSensors(ctx, cfg)
		})
		f.ZFS = runSection(ctx, &meta, "zfs", cfg.SectionTimeout, func(ctx context.Context) (facts.ZFSData, error) {
			return collectZFS(ctx, cfg, &meta)
		})
		f.Resources = runSection(ctx, &meta, "resources", cfg.SectionTimeout, func(ctx context.Context) (facts.ResourcesData, error) {
			return collectResources(ctx, cfg)
		})
		f.Services = runSection(ctx, &meta, "services", cfg.SectionTimeout, func(ctx context.Context) (facts.ServicesData, error) {
			return collectServices(ctx, cfg, &meta)
		})
		f.Network = runSection(ctx, &meta, "network", cfg.SectionTimeout, func(ctx context.Context) (facts.NetworkData, error) {
			return collectNetwork(ctx, cfg, &meta)
		})
	}

	// duration_ms and timestamp must be in the document BEFORE Truncate
	// measures it against FACTS_MAX_BYTES (§5: truncation "operates on the
	// assembled *facts.Facts before it is marshaled"), assigning them
	// after Truncate lets the final output exceed the budget by their
	// ~30-40 bytes with no hard-truncation marker to explain why.
	meta.DurationMs = time.Since(start).Milliseconds()
	meta.Timestamp = now(cfg).UTC().Format(time.RFC3339)
	f.Meta = meta
	Truncate(f, cfg)

	return f, nil
}

// runSection executes fn under a per-section timeout and converts any
// error into the section-error form. It never returns an error.
func runSection[T any](ctx context.Context, m *facts.Meta, name string, timeout time.Duration, fn func(context.Context) (T, error)) *facts.Section[T] {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := fn(cctx)
	if err != nil {
		reason, code := mapError(err, name, timeout)
		m.CollectorErrors = append(m.CollectorErrors, facts.CollectorError{Section: name, Reason: reason, ExitCode: code})
		return &facts.Section[T]{Err: reason}
	}
	return &facts.Section[T]{Data: data}
}

// binForSection is used only when the underlying error type carries no
// program name (namely *exec.ExitError), the two subprocesses collect
// spawns are fixed per section.
func binForSection(name string) string {
	if name == "sensors" {
		return "sensors"
	}
	return "journalctl"
}

// mapError implements the §7 error → (reason, exit_code) table, in the
// documented order.
func mapError(err error, sectionName string, timeout time.Duration) (string, int) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("timeout after %ds", int(timeout/time.Second)), 124
	case errors.Is(err, exec.ErrNotFound):
		bin := binForSection(sectionName)
		var ee *exec.Error
		if errors.As(err, &ee) {
			bin = ee.Name
		}
		return fmt.Sprintf("command not found: %s", bin), 127
	case errors.Is(err, journal.ErrNoJournal):
		return fmt.Sprintf("%s not readable", journalNotReadablePath(err)), 66
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission):
		return fmt.Sprintf("%s not readable", pathFromErr(err)), 66
	case errors.Is(err, ErrUnparseable):
		return "unparseable sensors output", 65
	default:
		// Checked before the generic *exec.ExitError case below: a
		// journal.ExecError additionally carries the captured journalctl
		// stderr (§7: "captured into the error reason, first 200 bytes"),
		// and errors.As alone would find the *exec.ExitError it wraps
		// first, losing that text.
		var je *journal.ExecError
		if errors.As(err, &je) {
			code := 1
			var ee *exec.ExitError
			if errors.As(je.Err, &ee) {
				code = ee.ExitCode()
			}
			reason := fmt.Sprintf("%s exited %d", binForSection(sectionName), code)
			if je.Stderr != "" {
				reason += ": " + je.Stderr
			}
			return reason, code
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Sprintf("%s exited %d", binForSection(sectionName), ee.ExitCode()), ee.ExitCode()
		}
		return err.Error(), 1
	}
}

func pathFromErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Path
	}
	return "path"
}

// journalNotReadablePath extracts the dirs journal.Run reports in
// ErrNoJournal's wrapped message ("journal directory not found: dir1,
// dir2") so the reason follows §7's "<dir> not readable" form instead of
// leaking journal.ErrNoJournal's own wording.
func journalNotReadablePath(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

// sinceArg renders a time.Duration as the plain-seconds systemd.time
// syntax journalctl accepts for --since (e.g. "600s"), avoiding any
// ambiguity from re-serializing the Go duration string.
func sinceArg(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}

// --- kernel / ras / smart / zfs (journal-backed) sections ---

// journalQuery fills in the two fields every call site must pass:
// MaxRecords (the record-cap window) and RawAlertMaxPriority (D2, which
// entries the cap must never evict, contracts/collect.md §3).
func journalQuery(cfg *config.Config, dirs []string, since string, args []string, exclude []string) journal.Query {
	return journal.Query{
		Dirs: dirs, Since: since, Args: args, ExcludeTransport: exclude,
		MaxRecords: cfg.JournalMaxRecords, RawAlertMaxPriority: cfg.RawAlertMaxPriority,
	}
}

// runJournalQuery wraps journal.Run, recording a collector_errors[] row
// for each directory that failed while at least one other directory
// succeeded, a tolerated partial failure, not a section failure (§3
// "One directory failing does not discard the other").
func runJournalQuery(ctx context.Context, meta *facts.Meta, sectionName string, q journal.Query) ([]facts.Entry, int, error) {
	entries, dropped, warnings, err := journal.Run(ctx, q)
	for _, w := range warnings {
		reason, code := mapError(w.Err, sectionName, 0)
		meta.CollectorErrors = append(meta.CollectorErrors, facts.CollectorError{
			Section: sectionName, Reason: w.Dir + ": " + reason, ExitCode: code,
		})
	}
	return entries, dropped, err
}

func collectKernel(ctx context.Context, cfg *config.Config, meta *facts.Meta) (facts.KernelData, error) {
	entries, dropped, err := runJournalQuery(ctx, meta, "kernel", journalQuery(cfg,
		[]string{cfg.HostJournalDir, cfg.HostJournalVolatileDir}, sinceArg(cfg.TickWindow),
		[]string{"-k", "-p", "err"}, nil))
	if err != nil {
		return facts.KernelData{}, err
	}
	return facts.KernelData{
		Count: len(entries) + dropped, Truncated: dropped > 0, DroppedEntries: dropped,
		Entries: entries,
	}, nil
}

func collectSmart(ctx context.Context, cfg *config.Config, meta *facts.Meta) (facts.SmartData, error) {
	entries, dropped, err := runJournalQuery(ctx, meta, "smart", journalQuery(cfg,
		[]string{cfg.HostJournalDir, cfg.HostJournalVolatileDir}, sinceArg(cfg.TickWindow),
		[]string{"-t", "smartd"}, nil))
	if err != nil {
		return facts.SmartData{}, err
	}
	return facts.SmartData{
		Count: len(entries) + dropped, Truncated: dropped > 0, DroppedEntries: dropped,
		Entries: entries,
	}, nil
}

func collectRas(ctx context.Context, cfg *config.Config, meta *facts.Meta) (facts.RasData, error) {
	entries, dropped, err := runJournalQuery(ctx, meta, "ras", journalQuery(cfg,
		[]string{cfg.HostJournalDir, cfg.HostJournalVolatileDir}, sinceArg(cfg.TickWindow),
		[]string{"-t", "rasdaemon"}, nil))
	if err != nil {
		return facts.RasData{}, err
	}
	// $HOST_RASDAEMON absent is an optional-source case, not a section
	// failure (collect.md §3 "Absent optional sources"): rasdaemon may
	// simply not be installed on this host, permanently, and failing the
	// whole section would fire that error every tick forever.
	store, err := readRasStore(cfg.HostRasdaemon)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return facts.RasData{}, err
		}
		store = []facts.StoreFile{}
	}
	return facts.RasData{
		Count: len(entries) + dropped, Truncated: dropped > 0, DroppedEntries: dropped,
		Entries: entries, Store: store,
	}, nil
}

func readRasStore(dir string) ([]facts.StoreFile, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]facts.StoreFile, 0, len(dirEntries))
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, facts.StoreFile{Name: e.Name(), Size: info.Size(), Mtime: info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var arcKeys = map[string]bool{
	"size": true, "c": true, "c_max": true, "c_min": true,
	"hits": true, "misses": true, "l2_size": true, "l2_hits": true, "l2_misses": true,
}

func collectZFS(ctx context.Context, cfg *config.Config, meta *facts.Meta) (facts.ZFSData, error) {
	events, dropped, err := runJournalQuery(ctx, meta, "zfs", journalQuery(cfg,
		[]string{cfg.HostJournalDir, cfg.HostJournalVolatileDir}, sinceArg(cfg.TickWindow),
		[]string{"-t", "zed"}, nil))
	if err != nil {
		return facts.ZFSData{}, err
	}
	// No ZFS module is an optional-source case (§3 "Absent optional
	// sources"): arc/pools degrade to empty, zed events[] still collected.
	arc, err := readArcStats(cfg.HostProc)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return facts.ZFSData{}, err
		}
		arc = map[string]int64{}
	}
	pools, err := readZFSPools(cfg.HostProc)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return facts.ZFSData{}, err
		}
		pools = []string{}
	}
	return facts.ZFSData{
		Count: len(events) + dropped, Truncated: dropped > 0, DroppedEntries: dropped,
		Events: events, Arc: arc, Pools: pools,
	}, nil
}

func readArcStats(hostProc string) (map[string]int64, error) {
	f, err := os.Open(filepath.Join(hostProc, "spl", "kstat", "zfs", "arcstats"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		if line <= 2 {
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !arcKeys[fields[0]] {
			continue
		}
		if v, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			out[fields[0]] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readZFSPools(hostProc string) ([]string, error) {
	dir := filepath.Join(hostProc, "spl", "kstat", "zfs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	pools := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			pools = append(pools, e.Name())
		}
	}
	sort.Strings(pools)
	return pools, nil
}

// --- sensors ---

func collectSensors(ctx context.Context, cfg *config.Config) (facts.SensorsData, error) {
	cmd := exec.CommandContext(ctx, "sensors", "-j")
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return facts.SensorsData{}, context.DeadlineExceeded
		}
		return facts.SensorsData{}, err
	}
	var chips map[string]any
	if err := json.Unmarshal(out, &chips); err != nil {
		return facts.SensorsData{}, ErrUnparseable
	}
	if chips == nil {
		chips = map[string]any{}
	}
	return facts.SensorsData{Chips: chips}, nil
}

// --- resources ---

var pseudoFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true, "tmpfs": true,
	"cgroup": true, "cgroup2": true, "mqueue": true, "hugetlbfs": true, "debugfs": true,
	"tracefs": true, "securityfs": true, "pstore": true, "bpf": true, "configfs": true,
	"fusectl": true, "autofs": true, "nsfs": true, "ramfs": true, "binfmt_misc": true,
	"overlay": true, "squashfs": true, "efivarfs": true, "rpc_pipefs": true, "selinuxfs": true,
}

// remoteFS (collect.md §3 row 6 note): syscall.Statfs on a hung remote
// mount blocks in uninterruptible D-state. Go cannot cancel a blocking
// syscall, so SECTION_TIMEOUT does not save the collector, the goroutine
// and its OS thread are stuck until the mount responds, possibly never.
// The target host is a NAS, so this is an every-tick risk.
var remoteFS = map[string]bool{
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "smbfs": true,
	"ceph": true, "glusterfs": true, "fuse.sshfs": true, "fuse.s3fs": true,
	"fuse.rclone": true, "afs": true, "9p": true,
}

func collectResources(ctx context.Context, cfg *config.Config) (facts.ResourcesData, error) {
	fsList, err := readFilesystems(cfg.HostProc, cfg.HostRoot)
	if err != nil {
		return facts.ResourcesData{}, err
	}
	mem, err := readMeminfo(cfg.HostProc)
	if err != nil {
		return facts.ResourcesData{}, err
	}
	load, err := readLoad(cfg.HostProc)
	if err != nil {
		return facts.ResourcesData{}, err
	}
	uptime, err := readUptime(cfg.HostProc)
	if err != nil {
		return facts.ResourcesData{}, err
	}
	return facts.ResourcesData{Filesystems: fsList, MemoryKB: mem, Load: load, UptimeSeconds: uptime}, nil
}

func readFilesystems(hostProc, hostRoot string) ([]facts.Filesystem, error) {
	f, err := os.Open(filepath.Join(hostProc, "mounts"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []facts.Filesystem
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		source, mount, fstype := fields[0], fields[1], fields[2]
		if pseudoFS[fstype] || remoteFS[fstype] {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(filepath.Join(hostRoot, mount), &st); err != nil {
			continue
		}
		if st.Blocks == 0 {
			continue
		}
		bsize := int64(st.Bsize)
		sizeKB := int64(st.Blocks) * bsize / 1024
		usedKB := int64(st.Blocks-st.Bfree) * bsize / 1024
		availKB := int64(st.Bavail) * bsize / 1024
		usePercent := 0
		if denom := usedKB + availKB; denom > 0 {
			usePercent = int(math.Ceil(float64(usedKB) * 100 / float64(denom)))
		}
		out = append(out, facts.Filesystem{
			Mount: mount, Source: source,
			SizeKB: sizeKB, UsedKB: usedKB, AvailKB: availKB, UsePercent: usePercent,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	if out == nil {
		out = []facts.Filesystem{}
	}
	return out, nil
}

var meminfoKeys = map[string]bool{
	"MemTotal": true, "MemAvailable": true, "MemFree": true,
	"SwapTotal": true, "SwapFree": true, "Dirty": true,
}

func readMeminfo(hostProc string) (map[string]int64, error) {
	f, err := os.Open(filepath.Join(hostProc, "meminfo"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		if !meminfoKeys[name] {
			continue
		}
		valFields := strings.Fields(parts[1])
		if len(valFields) == 0 {
			continue
		}
		if v, err := strconv.ParseInt(valFields[0], 10, 64); err == nil {
			out[name] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readLoad(hostProc string) (facts.Load, error) {
	b, err := os.ReadFile(filepath.Join(hostProc, "loadavg"))
	if err != nil {
		return facts.Load{}, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 4 {
		return facts.Load{}, fmt.Errorf("malformed loadavg")
	}
	l1, _ := strconv.ParseFloat(fields[0], 64)
	l5, _ := strconv.ParseFloat(fields[1], 64)
	l15, _ := strconv.ParseFloat(fields[2], 64)
	running, total := 0, 0
	if rp := strings.SplitN(fields[3], "/", 2); len(rp) == 2 {
		running, _ = strconv.Atoi(rp[0])
		total, _ = strconv.Atoi(rp[1])
	}
	return facts.Load{Load1: l1, Load5: l5, Load15: l15, Running: running, TotalProcs: total}, nil
}

func readUptime(hostProc string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(hostProc, "uptime"))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) < 1 {
		return 0, fmt.Errorf("malformed uptime")
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

// --- services ---

var failedUnitRe = regexp.MustCompile(`Failed to start|entered failed state|Start request repeated`)

func collectServices(ctx context.Context, cfg *config.Config, meta *facts.Meta) (facts.ServicesData, error) {
	entries, capDropped, err := runJournalQuery(ctx, meta, "services", journalQuery(cfg,
		[]string{cfg.HostJournalDir, cfg.HostJournalVolatileDir}, sinceArg(cfg.TickWindow),
		[]string{"-p", "err"}, []string{"kernel"}))
	if err != nil {
		return facts.ServicesData{}, err
	}

	failedSet := map[string]bool{}
	for _, e := range entries {
		if !failedUnitRe.MatchString(e.Message) {
			continue
		}
		unit := e.Identifier
		if e.Unit != nil && *e.Unit != "" {
			unit = *e.Unit
		}
		if unit != "" {
			failedSet[unit] = true
		}
	}
	failedUnits := make([]string, 0, len(failedSet))
	for u := range failedSet {
		failedUnits = append(failedUnits, u)
	}
	sort.Strings(failedUnits)

	count := len(entries) + capDropped
	dropped := capDropped
	truncated := capDropped > 0
	truncateEntries(&entries, &dropped, &truncated, cfg.ServicesMaxBytes, cfg.RawAlertMaxPriority)

	return facts.ServicesData{
		Count: count, Truncated: truncated, DroppedEntries: dropped,
		Entries: entries, FailedUnits: failedUnits,
	}, nil
}

// --- network ---

func collectNetwork(ctx context.Context, cfg *config.Config, meta *facts.Meta) (facts.NetworkData, error) {
	listeners, err := readListeners(cfg.HostProc)
	if err != nil {
		return facts.NetworkData{}, err
	}

	baselinePath := filepath.Join(cfg.StateDir, "baseline-ports")
	baseline, readErr := readBaseline(baselinePath)
	switch {
	case readErr == nil:
		newL, closedL := diffListeners(baseline, listeners)
		return facts.NetworkData{
			BaselineInitialized: false,
			Listeners:           listeners, NewListeners: newL, ClosedListeners: closedL,
		}, nil
	case errors.Is(readErr, fs.ErrNotExist):
		initialized := true
		if werr := writeBaseline(cfg.StateDir, listeners); werr != nil {
			// §2/§7: the write failure is never fatal, but a baseline that
			// was never actually written must not be reported as
			// initialized, the next tick's listener diff would be
			// meaningless. Reason names the stable destination path, not
			// os.CreateTemp's random ".tmp-*" name.
			_, code := mapError(werr, "network", 0)
			meta.CollectorErrors = append(meta.CollectorErrors, facts.CollectorError{
				Section: "network", Reason: "baseline-ports not writable", ExitCode: code,
			})
			initialized = false
		}
		return facts.NetworkData{
			BaselineInitialized: initialized,
			Listeners:           listeners, NewListeners: []facts.Listener{}, ClosedListeners: []facts.Listener{},
		}, nil
	default:
		return facts.NetworkData{}, readErr
	}
}

func readListeners(hostProc string) ([]facts.Listener, error) {
	specs := []struct {
		file, proto, state string
		optional           bool
	}{
		{"tcp", "tcp", "0A", false}, {"tcp6", "tcp", "0A", true},
		{"udp", "udp", "07", false}, {"udp6", "udp", "07", true},
	}
	var out []facts.Listener
	for _, sp := range specs {
		entries, err := parseProcNet(filepath.Join(hostProc, "net", sp.file), sp.proto, sp.state)
		if err != nil {
			// §3 "Absent optional sources": tcp6/udp6 missing (IPv6
			// disabled) contributes no listeners rather than failing the
			// whole section, tcp/udp are not optional and stay fatal.
			if sp.optional && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		out = append(out, entries...)
	}
	return uniqueSortListeners(out), nil
}

func parseProcNet(path, proto, wantState string) ([]facts.Listener, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []facts.Listener
	for i, line := range strings.Split(string(b), "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != wantState {
			continue
		}
		parts := strings.SplitN(fields[1], ":", 2)
		if len(parts) != 2 {
			continue
		}
		port, err := strconv.ParseUint(parts[1], 16, 16)
		if err != nil {
			continue
		}
		out = append(out, facts.Listener{Proto: proto, Addr: parts[0], Port: int(port)})
	}
	return out, nil
}

func uniqueSortListeners(in []facts.Listener) []facts.Listener {
	sort.Slice(in, func(i, j int) bool { return listenerLess(in[i], in[j]) })
	out := in[:0:0]
	var prev facts.Listener
	first := true
	for _, l := range in {
		if !first && l == prev {
			continue
		}
		out = append(out, l)
		prev = l
		first = false
	}
	if out == nil {
		out = []facts.Listener{}
	}
	return out
}

func listenerLess(a, b facts.Listener) bool {
	if a.Proto != b.Proto {
		return a.Proto < b.Proto
	}
	if a.Port != b.Port {
		return a.Port < b.Port
	}
	return a.Addr < b.Addr
}

func readBaseline(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func writeBaseline(stateDir string, listeners []facts.Listener) error {
	seen := map[string]bool{}
	keys := make([]string, 0, len(listeners))
	for _, l := range listeners {
		k := l.Proto + "/" + strconv.Itoa(l.Port)
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var buf strings.Builder
	for _, k := range keys {
		buf.WriteString(k)
		buf.WriteString("\n")
	}
	return atomicWrite(stateDir, "baseline-ports", []byte(buf.String()), 0o644)
}

func diffListeners(baseline []string, current []facts.Listener) (newL, closedL []facts.Listener) {
	baseSet := map[string]bool{}
	for _, b := range baseline {
		baseSet[b] = true
	}
	curSet := map[string]bool{}
	for _, l := range current {
		k := l.Proto + "/" + strconv.Itoa(l.Port)
		curSet[k] = true
		if !baseSet[k] {
			newL = append(newL, l)
		}
	}
	for k := range baseSet {
		if curSet[k] {
			continue
		}
		parts := strings.SplitN(k, "/", 2)
		if len(parts) != 2 {
			continue
		}
		port, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		closedL = append(closedL, facts.Listener{Proto: parts[0], Port: port})
	}
	sort.Slice(newL, func(i, j int) bool { return listenerLess(newL[i], newL[j]) })
	sort.Slice(closedL, func(i, j int) bool { return listenerLess(closedL[i], closedL[j]) })
	if newL == nil {
		newL = []facts.Listener{}
	}
	if closedL == nil {
		closedL = []facts.Listener{}
	}
	return newL, closedL
}

// atomicWrite implements C4's write pattern: create-temp, write, sync,
// close, rename.
func atomicWrite(dir, name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, name)); err != nil {
		return err
	}
	ok = true
	return nil
}
