package collect

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/config"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/facts"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/report"
)

// collectDeep is collect.Run's only deep entry point (§3b). All deep
// journal queries run over DEEP_WINDOW at every priority.
func collectDeep(ctx context.Context, cfg *config.Config, meta *facts.Meta, component string) (facts.DeepData, error) {
	switch component {
	case "zfs":
		return collectDeepZFS(ctx, cfg, meta)
	case "smart":
		entries, dropped, err := deepJournal(ctx, cfg, meta, "-t", "smartd")
		if err != nil {
			return facts.DeepData{}, err
		}
		return facts.DeepData{Entries: entries, Truncated: dropped > 0, DroppedEntries: dropped}, nil
	case "kernel":
		entries, dropped, err := deepJournal(ctx, cfg, meta, "-k")
		if err != nil {
			return facts.DeepData{}, err
		}
		return facts.DeepData{Entries: entries, Truncated: dropped > 0, DroppedEntries: dropped}, nil
	case "ras":
		entries, dropped, err := deepJournal(ctx, cfg, meta, "-t", "rasdaemon")
		if err != nil {
			return facts.DeepData{}, err
		}
		// $HOST_RASDAEMON absent is an optional-source case (§3), same as
		// the tick-mode ras section.
		store, err := readRasStore(cfg.HostRasdaemon)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return facts.DeepData{}, err
			}
			store = []facts.StoreFile{}
		}
		return facts.DeepData{Entries: entries, Store: store, Truncated: dropped > 0, DroppedEntries: dropped}, nil
	default:
		return facts.DeepData{}, fmt.Errorf("unknown deep component %q", component)
	}
}

func deepJournal(ctx context.Context, cfg *config.Config, meta *facts.Meta, args ...string) ([]facts.Entry, int, error) {
	return runJournalQuery(ctx, meta, "deep", journalQuery(cfg,
		[]string{cfg.HostJournalDir, cfg.HostJournalVolatileDir}, sinceArg(cfg.DeepWindow), args, nil))
}

func collectDeepZFS(ctx context.Context, cfg *config.Config, meta *facts.Meta) (facts.DeepData, error) {
	zed, zedDropped, err := deepJournal(ctx, cfg, meta, "-t", "zed")
	if err != nil {
		return facts.DeepData{}, err
	}
	smart, smartDropped, err := deepJournal(ctx, cfg, meta, "-t", "smartd")
	if err != nil {
		return facts.DeepData{}, err
	}
	kernel, kernelDropped, err := deepJournal(ctx, cfg, meta, "-k")
	if err != nil {
		return facts.DeepData{}, err
	}
	// No ZFS module is an optional-source case (§3): arc/pools/pool_kstat
	// degrade to empty, the zed events[] above are still collected.
	arc, err := readArcStats(cfg.HostProc)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return facts.DeepData{}, err
		}
		arc = map[string]int64{}
	}
	pools, err := readZFSPools(cfg.HostProc)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return facts.DeepData{}, err
		}
		pools = []string{}
	}
	poolKstat := map[string]map[string]any{}
	for _, p := range pools {
		if k, err := readPoolKstat(cfg.HostProc, p); err == nil {
			poolKstat[p] = k
		}
	}
	history, err := readHistory(cfg.StateDir)
	if err != nil {
		return facts.DeepData{}, err
	}
	dropped := zedDropped + smartDropped + kernelDropped
	return facts.DeepData{
		ZedEvents: zed, SmartEntries: smart, KernelEntries: kernel,
		PoolKstat: poolKstat, Arc: arc, History: history,
		Truncated: dropped > 0, DroppedEntries: dropped,
	}, nil
}

// readPoolKstat merges every readable file directly under
// $HOST_PROC/spl/kstat/zfs/<pool>/ into one flat map, each file parsed as
// a kstat key/value table (§3b), the same "name type data" shape (2
// header lines, then "name type value" rows) readArcStats already parses;
// a pool's `io`/`txgs` kstat files are themselves multi-row tables, not a
// single scalar, so the file's content is parsed, not merely captured.
func readPoolKstat(hostProc, pool string) (map[string]any, error) {
	dir := filepath.Join(hostProc, "spl", "kstat", "zfs", pool)
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		kv, err := parseKstatFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		for k, v := range kv {
			out[k] = v
		}
	}
	return out, nil
}

// parseKstatFile parses one Linux kstat named-table file: 2 header lines,
// then "<name> <type> <value...>" rows. Value is int64 where it parses as
// a single field, else the remaining fields joined back to a string.
func parseKstatFile(path string) (map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]any{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		if line <= 2 {
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		key := fields[0]
		if len(fields) == 3 {
			if n, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
				out[key] = n
				continue
			}
		}
		out[key] = strings.Join(fields[2:], " ")
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// historyFileRe matches the <unix-seconds,10>-<tick_seq,6>.json layout
// (D3); files that don't match fall back to their mtime.
var historyFileRe = regexp.MustCompile(`^(\d{10})-\d{6}\.json$`)

func readHistory(stateDir string) ([]facts.HistoryRef, error) {
	dir := filepath.Join(stateDir, "history")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []facts.HistoryRef{}, nil
		}
		return nil, err
	}

	var out []facts.HistoryRef
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r report.Report
		if err := json.Unmarshal(b, &r); err != nil {
			continue
		}
		if !hasZFSFinding(r) {
			continue
		}
		ts := tsFromHistoryName(e.Name())
		if ts == "" {
			if info, err := e.Info(); err == nil {
				ts = info.ModTime().UTC().Format(time.RFC3339)
			}
		}
		out = append(out, facts.HistoryRef{TS: ts, Status: r.Status, Headline: r.Headline})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS < out[j].TS })
	if out == nil {
		out = []facts.HistoryRef{}
	}
	return out, nil
}

func hasZFSFinding(r report.Report) bool {
	for _, f := range r.Findings {
		if f.Component == "zfs" {
			return true
		}
	}
	return false
}

func tsFromHistoryName(name string) string {
	m := historyFileRe.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	sec, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}
