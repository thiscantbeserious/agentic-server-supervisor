// Package facts defines the collect output wire types (C5) and the embedded
// facts.schema.json. Imported by collect, analyze, runtime, state, nobody
// redefines these types.
package facts

import (
	_ "embed"
	"encoding/json"
)

const SchemaVersion = "1"

//go:embed facts.schema.json
var SchemaJSON []byte // test-only consumer (D7)

// Section carries either a healthy payload or an error. Marshals to
// {"error": "..."} when Err != "", otherwise to Data. Consumers MUST probe
// Err before reading Data.
type Section[T any] struct {
	Err  string
	Data T
}

func (s Section[T]) MarshalJSON() ([]byte, error) {
	if s.Err != "" {
		return json.Marshal(struct {
			Error string `json:"error"`
		}{Error: s.Err})
	}
	return json.Marshal(s.Data)
}

// UnmarshalJSON probes for an "error" key. Its presence makes the section
// failed regardless of any other keys the document carries: error present
// ⇒ the section is failed, data ignored. A document with both "error" and
// data keys is malformed per facts.schema.json (additionalProperties:
// false on the error branch) but must still decode deterministically
// rather than lose the collector error silently.
func (s *Section[T]) UnmarshalJSON(b []byte) error {
	var probe struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	if probe.Error != "" {
		s.Err = probe.Error
		return nil
	}
	s.Err = ""
	return json.Unmarshal(b, &s.Data)
}

// Entry is the NORMALIZED journal entry (C5), the only journal shape that
// leaves internal/journal.
type Entry struct {
	TS         string  `json:"ts"`
	Priority   int     `json:"priority"`
	Identifier string  `json:"identifier"`
	Unit       *string `json:"unit"`
	Message    string  `json:"message"`
}

type CollectorError struct {
	Section  string `json:"section"`
	Reason   string `json:"reason"`
	ExitCode int    `json:"exit_code"`
}

type Meta struct {
	SchemaVersion   string           `json:"schema_version"`
	Hostname        string           `json:"hostname"`
	Timestamp       string           `json:"timestamp"`
	TickSeq         int64            `json:"tick_seq"`
	Mode            string           `json:"mode"`
	DeepComponent   *string          `json:"deep_component"`
	Window          string           `json:"window"`
	DurationMs      int64            `json:"duration_ms"`
	Truncated       bool             `json:"truncated"`
	CollectorErrors []CollectorError `json:"collector_errors"`
}

type KernelData struct {
	Count          int     `json:"count"`
	Truncated      bool    `json:"truncated"`
	DroppedEntries int     `json:"dropped_entries"`
	Entries        []Entry `json:"entries"`
}

type SmartData KernelData

type StoreFile struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

type RasData struct {
	Count          int         `json:"count"`
	Truncated      bool        `json:"truncated"`
	DroppedEntries int         `json:"dropped_entries"`
	Entries        []Entry     `json:"entries"`
	Store          []StoreFile `json:"store"`
}

type SensorsData struct {
	Truncated      bool           `json:"truncated"`
	DroppedEntries int            `json:"dropped_entries"`
	Chips          map[string]any `json:"chips"`
}

type ZFSData struct {
	Count          int              `json:"count"`
	Truncated      bool             `json:"truncated"`
	DroppedEntries int              `json:"dropped_entries"`
	Events         []Entry          `json:"events"`
	Arc            map[string]int64 `json:"arc"`
	Pools          []string         `json:"pools"`
}

type Filesystem struct {
	Mount      string `json:"mount"`
	Source     string `json:"source"`
	SizeKB     int64  `json:"size_kb"`
	UsedKB     int64  `json:"used_kb"`
	AvailKB    int64  `json:"avail_kb"`
	UsePercent int    `json:"use_percent"`
}

type Load struct {
	Load1      float64 `json:"load1"`
	Load5      float64 `json:"load5"`
	Load15     float64 `json:"load15"`
	Running    int     `json:"running"`
	TotalProcs int     `json:"total_procs"`
}

type ResourcesData struct {
	Truncated      bool             `json:"truncated"`
	DroppedEntries int              `json:"dropped_entries"`
	Filesystems    []Filesystem     `json:"filesystems"`
	MemoryKB       map[string]int64 `json:"memory_kb"`
	Load           Load             `json:"load"`
	UptimeSeconds  int64            `json:"uptime_seconds"`
}

type ServicesData struct {
	Count          int      `json:"count"`
	Truncated      bool     `json:"truncated"`
	DroppedEntries int      `json:"dropped_entries"`
	Entries        []Entry  `json:"entries"`
	FailedUnits    []string `json:"failed_units"`
}

type Listener struct {
	Proto string `json:"proto"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
}

type NetworkData struct {
	Truncated           bool       `json:"truncated"`
	DroppedEntries      int        `json:"dropped_entries"`
	BaselineInitialized bool       `json:"baseline_initialized"`
	Listeners           []Listener `json:"listeners"`
	NewListeners        []Listener `json:"new_listeners"`
	ClosedListeners     []Listener `json:"closed_listeners"`
}

type HistoryRef struct {
	TS       string `json:"ts"`
	Status   string `json:"status"`
	Headline string `json:"headline"`
}

type DeepData struct {
	Component      string                    `json:"component"`
	Truncated      bool                      `json:"truncated"`
	DroppedEntries int                       `json:"dropped_entries"`
	Entries        []Entry                   `json:"entries,omitempty"`
	ZedEvents      []Entry                   `json:"zed_events,omitempty"`
	SmartEntries   []Entry                   `json:"smart_entries,omitempty"`
	KernelEntries  []Entry                   `json:"kernel_entries,omitempty"`
	Store          []StoreFile               `json:"store,omitempty"`
	PoolKstat      map[string]map[string]any `json:"pool_kstat,omitempty"`
	Arc            map[string]int64          `json:"arc,omitempty"`
	History        []HistoryRef              `json:"history,omitempty"`
}

type Facts struct {
	Meta      Meta                    `json:"meta"`
	Kernel    *Section[KernelData]    `json:"kernel,omitempty"`
	Ras       *Section[RasData]       `json:"ras,omitempty"`
	Smart     *Section[SmartData]     `json:"smart,omitempty"`
	Sensors   *Section[SensorsData]   `json:"sensors,omitempty"`
	ZFS       *Section[ZFSData]       `json:"zfs,omitempty"`
	Resources *Section[ResourcesData] `json:"resources,omitempty"`
	Services  *Section[ServicesData]  `json:"services,omitempty"`
	Network   *Section[NetworkData]   `json:"network,omitempty"`
	Deep      *Section[DeepData]      `json:"deep,omitempty"`
}
