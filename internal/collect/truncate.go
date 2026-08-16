package collect

import (
	"encoding/json"
	"math"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
)

// candidate is one truncatable array inside facts.Facts, addressed by its
// dot-path (contracts/collect.md §5).
type candidate struct {
	path       string
	length     func() int
	droppable  func() int
	marshal    func() ([]byte, error)
	dropN      func(n int) int // drop up to n from the end, respecting protection; returns actual count dropped
	hardReduce func() int      // reduce to the protected subset (empty for non-priority candidates); returns count dropped
}

// entryCandidate builds a candidate over a []facts.Entry field, where
// entries with priority <= maxPri are protected from the normal
// truncation loop (D2).
func entryCandidate(path string, entries *[]facts.Entry, dropped *int, truncated *bool, maxPri int) candidate {
	protected := func(e facts.Entry) bool { return e.Priority <= maxPri }
	return candidate{
		path:      path,
		length:    func() int { return len(*entries) },
		droppable: func() int { return countUnprotected(*entries, protected) },
		marshal:   func() ([]byte, error) { return json.Marshal(*entries) },
		dropN: func(n int) int {
			d := dropFromEnd(entries, n, protected)
			if d > 0 {
				*dropped += d
				*truncated = true
			}
			return d
		},
		hardReduce: func() int {
			d := reduceToProtected(entries, protected)
			if d > 0 {
				*dropped += d
				*truncated = true
			}
			return d
		},
	}
}

// genericCandidate builds a candidate over a slice with no priority
// notion (network.listeners, resources.filesystems, deep.history) — every
// element is droppable.
func genericCandidate[T any](path string, s *[]T, dropped *int, truncated *bool) candidate {
	noneProtected := func(T) bool { return false }
	return candidate{
		path:      path,
		length:    func() int { return len(*s) },
		droppable: func() int { return len(*s) },
		marshal:   func() ([]byte, error) { return json.Marshal(*s) },
		dropN: func(n int) int {
			d := dropFromEnd(s, n, noneProtected)
			if d > 0 {
				*dropped += d
				*truncated = true
			}
			return d
		},
		hardReduce: func() int {
			d := reduceToProtected(s, noneProtected) // reduces to empty
			if d > 0 {
				*dropped += d
				*truncated = true
			}
			return d
		},
	}
}

func countUnprotected[T any](s []T, protected func(T) bool) int {
	n := 0
	for _, v := range s {
		if !protected(v) {
			n++
		}
	}
	return n
}

// dropFromEnd removes up to n elements scanning from the end, skipping
// protected elements, and returns the number actually dropped. Relative
// order of the surviving elements is preserved.
func dropFromEnd[T any](s *[]T, n int, protected func(T) bool) int {
	arr := *s
	if n <= 0 || len(arr) == 0 {
		return 0
	}
	drop := make([]bool, len(arr))
	remaining, dropped := n, 0
	for i := len(arr) - 1; i >= 0 && remaining > 0; i-- {
		if protected(arr[i]) {
			continue
		}
		drop[i] = true
		remaining--
		dropped++
	}
	if dropped == 0 {
		return 0
	}
	out := make([]T, 0, len(arr)-dropped)
	for i, v := range arr {
		if !drop[i] {
			out = append(out, v)
		}
	}
	*s = out
	return dropped
}

// reduceToProtected keeps only protected elements, returning the count
// dropped.
func reduceToProtected[T any](s *[]T, protected func(T) bool) int {
	arr := *s
	out := make([]T, 0, len(arr))
	for _, v := range arr {
		if protected(v) {
			out = append(out, v)
		}
	}
	dropped := len(arr) - len(out)
	*s = out
	return dropped
}

// buildCandidates returns the §5 fixed table, dot-path order, restricted
// to sections that are actually present and healthy.
func buildCandidates(f *facts.Facts, maxPri int) []candidate {
	var cands []candidate
	if f.Kernel != nil && f.Kernel.Err == "" {
		cands = append(cands, entryCandidate("kernel.entries", &f.Kernel.Data.Entries, &f.Kernel.Data.DroppedEntries, &f.Kernel.Data.Truncated, maxPri))
	}
	if f.Ras != nil && f.Ras.Err == "" {
		cands = append(cands, entryCandidate("ras.entries", &f.Ras.Data.Entries, &f.Ras.Data.DroppedEntries, &f.Ras.Data.Truncated, maxPri))
	}
	if f.Smart != nil && f.Smart.Err == "" {
		cands = append(cands, entryCandidate("smart.entries", &f.Smart.Data.Entries, &f.Smart.Data.DroppedEntries, &f.Smart.Data.Truncated, maxPri))
	}
	if f.ZFS != nil && f.ZFS.Err == "" {
		cands = append(cands, entryCandidate("zfs.events", &f.ZFS.Data.Events, &f.ZFS.Data.DroppedEntries, &f.ZFS.Data.Truncated, maxPri))
	}
	if f.Services != nil && f.Services.Err == "" {
		cands = append(cands, entryCandidate("services.entries", &f.Services.Data.Entries, &f.Services.Data.DroppedEntries, &f.Services.Data.Truncated, maxPri))
	}
	if f.Network != nil && f.Network.Err == "" {
		cands = append(cands, genericCandidate("network.listeners", &f.Network.Data.Listeners, &f.Network.Data.DroppedEntries, &f.Network.Data.Truncated))
	}
	if f.Resources != nil && f.Resources.Err == "" {
		cands = append(cands, genericCandidate("resources.filesystems", &f.Resources.Data.Filesystems, &f.Resources.Data.DroppedEntries, &f.Resources.Data.Truncated))
	}
	if f.Deep != nil && f.Deep.Err == "" {
		d := &f.Deep.Data
		if d.Entries != nil {
			cands = append(cands, entryCandidate("deep.entries", &d.Entries, &d.DroppedEntries, &d.Truncated, maxPri))
		}
		if d.ZedEvents != nil {
			cands = append(cands, entryCandidate("deep.zed_events", &d.ZedEvents, &d.DroppedEntries, &d.Truncated, maxPri))
		}
		if d.SmartEntries != nil {
			cands = append(cands, entryCandidate("deep.smart_entries", &d.SmartEntries, &d.DroppedEntries, &d.Truncated, maxPri))
		}
		if d.KernelEntries != nil {
			cands = append(cands, entryCandidate("deep.kernel_entries", &d.KernelEntries, &d.DroppedEntries, &d.Truncated, maxPri))
		}
		if d.History != nil {
			cands = append(cands, genericCandidate("deep.history", &d.History, &d.DroppedEntries, &d.Truncated))
		}
	}
	return cands
}

func selectLargest(cands []candidate) candidate {
	best := cands[0]
	bestSize := marshalSize(best)
	for _, c := range cands[1:] {
		sz := marshalSize(c)
		if sz > bestSize || (sz == bestSize && c.path < best.path) {
			best, bestSize = c, sz
		}
	}
	return best
}

func marshalSize(c candidate) int {
	b, _ := c.marshal()
	return len(b)
}

// Truncate applies the §5 algorithm in place: while the marshaled
// document exceeds FACTS_MAX_BYTES, drop from the largest droppable
// candidate; at the fixed point (no droppable candidate left), hard-
// truncate to protected subsets and stop.
func Truncate(f *facts.Facts, cfg *config.Config) {
	maxPri := cfg.RawAlertMaxPriority
	hardTruncated := false
	for {
		b, _ := json.Marshal(f)
		if len(b) <= cfg.FactsMaxBytes {
			break
		}
		cands := buildCandidates(f, maxPri)
		var droppable []candidate
		for _, c := range cands {
			if c.droppable() > 0 {
				droppable = append(droppable, c)
			}
		}
		if len(droppable) == 0 {
			hardTruncate(f, cfg)
			hardTruncated = true
			break
		}
		pick := selectLargest(droppable)
		n := int(math.Ceil(float64(pick.length()) * 0.25))
		pick.dropN(n)
	}
	// Hitting the step-3 fixed point always means the document is
	// incomplete (§5 step 3), even in the edge case where there is
	// nothing left for hardTruncate to reduce and so no individual
	// section's own Truncated flag ends up set — anySectionTruncated
	// alone would silently report false there.
	f.Meta.Truncated = hardTruncated || anySectionTruncated(f)
}

// truncateEntries implements the same algorithm restricted to a single
// candidate — the per-section SERVICES_MAX_BYTES budget (§3 row 7, §5
// last line).
func truncateEntries(entries *[]facts.Entry, dropped *int, truncated *bool, maxBytes, maxPri int) {
	protected := func(e facts.Entry) bool { return e.Priority <= maxPri }
	for {
		b, _ := json.Marshal(*entries)
		if len(b) <= maxBytes {
			return
		}
		if countUnprotected(*entries, protected) == 0 {
			return // fixed point: nothing left to drop
		}
		n := int(math.Ceil(float64(len(*entries)) * 0.25))
		d := dropFromEnd(entries, n, protected)
		if d > 0 {
			*dropped += d
			*truncated = true
		}
	}
}

// hardTruncate is §5 step 3: reduce every candidate to its protected
// subset, additionally cap kernel.entries to the RAW_ALERT_MAX_LINES
// newest protected entries, and record the fixed-point collector error.
func hardTruncate(f *facts.Facts, cfg *config.Config) {
	maxPri := cfg.RawAlertMaxPriority
	for _, c := range buildCandidates(f, maxPri) {
		c.hardReduce()
	}
	if f.Kernel != nil && f.Kernel.Err == "" {
		capNewest(&f.Kernel.Data.Entries, &f.Kernel.Data.DroppedEntries, &f.Kernel.Data.Truncated, cfg.RawAlertMaxLines)
	}
	f.Meta.CollectorErrors = append(f.Meta.CollectorErrors, facts.CollectorError{
		Section: "*", Reason: "hard truncation, budget exhausted", ExitCode: 0,
	})
}

// capNewest keeps only the last maxLines elements (entries are kept
// ascending by ts, so the tail is the newest).
func capNewest(entries *[]facts.Entry, dropped *int, truncated *bool, maxLines int) {
	arr := *entries
	if len(arr) <= maxLines {
		return
	}
	drop := len(arr) - maxLines
	*entries = arr[drop:]
	*dropped += drop
	*truncated = true
}

func anySectionTruncated(f *facts.Facts) bool {
	healthy := func(err string, t bool) bool { return err == "" && t }
	if f.Kernel != nil && healthy(f.Kernel.Err, f.Kernel.Data.Truncated) {
		return true
	}
	if f.Ras != nil && healthy(f.Ras.Err, f.Ras.Data.Truncated) {
		return true
	}
	if f.Smart != nil && healthy(f.Smart.Err, f.Smart.Data.Truncated) {
		return true
	}
	if f.Sensors != nil && healthy(f.Sensors.Err, f.Sensors.Data.Truncated) {
		return true
	}
	if f.ZFS != nil && healthy(f.ZFS.Err, f.ZFS.Data.Truncated) {
		return true
	}
	if f.Resources != nil && healthy(f.Resources.Err, f.Resources.Data.Truncated) {
		return true
	}
	if f.Services != nil && healthy(f.Services.Err, f.Services.Data.Truncated) {
		return true
	}
	if f.Network != nil && healthy(f.Network.Err, f.Network.Data.Truncated) {
		return true
	}
	if f.Deep != nil && healthy(f.Deep.Err, f.Deep.Data.Truncated) {
		return true
	}
	return false
}
