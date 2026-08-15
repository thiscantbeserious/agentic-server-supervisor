package facts

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestSectionMarshalHealthy(t *testing.T) {
	s := Section[KernelData]{Data: KernelData{Count: 1, Truncated: false, DroppedEntries: 0, Entries: []Entry{}}}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, hasErr := got["error"]; hasErr {
		t.Fatalf("healthy section must not marshal an \"error\" key, got %s", raw)
	}
	if got["count"] != float64(1) {
		t.Fatalf("expected count=1 in %s", raw)
	}
}

func TestSectionMarshalError(t *testing.T) {
	s := Section[KernelData]{Err: "command not found: journalctl"}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"error":"command not found: journalctl"}`
	if string(raw) != want {
		t.Fatalf("Marshal() = %s, want %s", raw, want)
	}
}

func TestSectionUnmarshalError(t *testing.T) {
	var s Section[KernelData]
	if err := json.Unmarshal([]byte(`{"error":"boom"}`), &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.Err != "boom" {
		t.Fatalf("Err = %q, want %q", s.Err, "boom")
	}
}

// N10/main decision: "error present ⇒ the section is failed, data
// ignored" — a document that carries both "error" and data keys (which
// facts.schema.json itself rejects as malformed, see
// TestFactsSchema_RejectsMalformed) must decode deterministically as a
// failed section, not silently as a healthy one with a lost collector
// error.
func TestSectionUnmarshalErrorWinsOverStrayData(t *testing.T) {
	var s Section[KernelData]
	raw := []byte(`{"error":"journalctl timed out","count":99,"truncated":true,"dropped_entries":1,"entries":[]}`)
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.Err != "journalctl timed out" {
		t.Fatalf("Err = %q, want %q", s.Err, "journalctl timed out")
	}
	if s.Data.Count != 0 {
		t.Fatalf("Data = %+v, want zero value (data must be ignored when error is present)", s.Data)
	}
}

func TestSectionUnmarshalHealthy(t *testing.T) {
	var s Section[KernelData]
	raw := []byte(`{"count":2,"truncated":true,"dropped_entries":3,"entries":[]}`)
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if s.Err != "" {
		t.Fatalf("Err = %q, want empty", s.Err)
	}
	if s.Data.Count != 2 || !s.Data.Truncated || s.Data.DroppedEntries != 3 {
		t.Fatalf("Data = %+v, unexpected", s.Data)
	}
}

func unitPtr(u string) *string { return &u }

func realisticTickFacts() *Facts {
	return &Facts{
		Meta: Meta{
			SchemaVersion: SchemaVersion,
			Hostname:      "bam",
			Timestamp:     "2026-08-15T09:35:02Z",
			TickSeq:       412,
			Mode:          "tick",
			DeepComponent: nil,
			Window:        "10min",
			DurationMs:    1873,
			Truncated:     true,
			CollectorErrors: []CollectorError{
				{Section: "sensors", Reason: "command not found: sensors", ExitCode: 127},
			},
		},
		Kernel: &Section[KernelData]{Data: KernelData{
			Count: 1, Truncated: false, DroppedEntries: 0,
			Entries: []Entry{
				{TS: "2026-08-15T09:31:44Z", Priority: 3, Identifier: "kernel", Unit: nil,
					Message: "ata3.00: exception Emask 0x0 SAct 0x0 SErr 0x0 action 0x6 frozen"},
			},
		}},
		Ras: &Section[RasData]{Data: RasData{
			Count: 0, Truncated: false, DroppedEntries: 0, Entries: []Entry{},
			Store: []StoreFile{{Name: "ras-mc_event.db", Size: 40960, Mtime: 1786864210}},
		}},
		Smart: &Section[SmartData]{Data: SmartData{
			Count: 1, Truncated: false, DroppedEntries: 0,
			Entries: []Entry{
				{TS: "2026-08-15T09:30:01Z", Priority: 5, Identifier: "smartd", Unit: unitPtr("smartd.service"),
					Message: "Device: /dev/sdb [SAT], SMART Usage Attribute: 194 Temperature_Celsius changed from 118 to 117"},
			},
		}},
		Sensors: &Section[SensorsData]{Err: "command not found: sensors"},
		ZFS: &Section[ZFSData]{Data: ZFSData{
			Count: 2, Truncated: false, DroppedEntries: 0,
			Events: []Entry{
				{TS: "2026-08-15T09:12:03Z", Priority: 4, Identifier: "zed", Unit: unitPtr("zfs-zed.service"),
					Message: "eid=1841 class=checksum pool='hotstore' vdev=seagate-zvtazeam-crypt cksum_errors=1"},
				{TS: "2026-08-15T09:12:04Z", Priority: 6, Identifier: "zed", Unit: unitPtr("zfs-zed.service"),
					Message: "eid=1842 class=scrub_start pool='hotstore'"},
			},
			Arc: map[string]int64{
				"size": 8523441152, "c": 8589934592, "c_max": 17179869184, "c_min": 1073741824,
				"hits": 918273645, "misses": 3421887, "l2_size": 0, "l2_hits": 0, "l2_misses": 0,
			},
			Pools: []string{"cache", "hotstore"},
		}},
		Resources: &Section[ResourcesData]{Data: ResourcesData{
			Truncated: false, DroppedEntries: 0,
			Filesystems: []Filesystem{
				{Mount: "/", Source: "/dev/mapper/bam-root", SizeKB: 61234560, UsedKB: 24118272, AvailKB: 33988608, UsePercent: 42},
			},
			MemoryKB:      map[string]int64{"MemTotal": 32784332, "MemAvailable": 19233104},
			Load:          Load{Load1: 0.72, Load5: 0.61, Load15: 0.55, Running: 2, TotalProcs: 431},
			UptimeSeconds: 4127883,
		}},
		Services: &Section[ServicesData]{Data: ServicesData{
			Count: 47, Truncated: true, DroppedEntries: 44,
			Entries: []Entry{
				{TS: "2026-08-15T09:33:10Z", Priority: 3, Identifier: "smbd", Unit: unitPtr("smbd.service"),
					Message: "Failed to start Samba SMB Daemon."},
			},
			FailedUnits: []string{"smbd.service"},
		}},
		Network: &Section[NetworkData]{Data: NetworkData{
			Truncated: false, DroppedEntries: 0, BaselineInitialized: false,
			Listeners: []Listener{
				{Proto: "tcp", Addr: "00000000", Port: 22},
				{Proto: "udp", Addr: "00000000", Port: 137},
			},
			NewListeners:    []Listener{{Proto: "tcp", Addr: "0100007F", Port: 8000}},
			ClosedListeners: []Listener{},
		}},
	}
}

func TestFactsMarshalRoundTrip(t *testing.T) {
	f := realisticTickFacts()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Facts
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Sensors == nil || got.Sensors.Err != "command not found: sensors" {
		t.Fatalf("Sensors section did not round-trip as an error section: %+v", got.Sensors)
	}
	if got.Kernel == nil || got.Kernel.Data.Count != 1 {
		t.Fatalf("Kernel section did not round-trip: %+v", got.Kernel)
	}
	if got.Meta.TickSeq != 412 || got.Meta.Mode != "tick" {
		t.Fatalf("Meta did not round-trip: %+v", got.Meta)
	}
}

func compileFactsSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	schema, err := jsonschema.UnmarshalJSON(bytes.NewReader(SchemaJSON))
	if err != nil {
		t.Fatalf("unmarshal embedded schema: %v", err)
	}
	if err := compiler.AddResource("facts.schema.json", schema); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	sch, err := compiler.Compile("facts.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return sch
}

func TestFactsSchema_AcceptsRealisticTickDocument(t *testing.T) {
	sch := compileFactsSchema(t)
	raw, err := json.Marshal(realisticTickFacts())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Errorf("facts.schema.json rejected a realistic tick document: %v", err)
	}
}

func TestFactsSchema_RejectsMalformed(t *testing.T) {
	sch := compileFactsSchema(t)
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "kernel section missing dropped_entries",
			raw:  `{"meta":{"schema_version":"1","hostname":"bam","timestamp":"2026-08-15T09:35:02Z","tick_seq":1,"mode":"tick","deep_component":null,"window":"10min","duration_ms":1,"truncated":false,"collector_errors":[]},"kernel":{"count":0,"truncated":false,"entries":[]}}`,
		},
		{
			name: "collector_errors item is a string, not an object",
			raw:  `{"meta":{"schema_version":"1","hostname":"bam","timestamp":"2026-08-15T09:35:02Z","tick_seq":1,"mode":"tick","deep_component":null,"window":"10min","duration_ms":1,"truncated":false,"collector_errors":["boom"]}}`,
		},
		{
			name: "meta missing required field mode",
			raw:  `{"meta":{"schema_version":"1","hostname":"bam","timestamp":"2026-08-15T09:35:02Z","tick_seq":1,"deep_component":null,"window":"10min","duration_ms":1,"truncated":false,"collector_errors":[]}}`,
		},
		{
			name: "unknown top-level property",
			raw:  `{"meta":{"schema_version":"1","hostname":"bam","timestamp":"2026-08-15T09:35:02Z","tick_seq":1,"mode":"tick","deep_component":null,"window":"10min","duration_ms":1,"truncated":false,"collector_errors":[]},"bogus":true}`,
		},
		{
			// contracts/collect.md invariant: "truncated: true implies
			// dropped_entries > 0". A section claiming truncation with a
			// zero drop count is contradictory and must be rejected.
			name: "kernel section truncated=true with dropped_entries=0",
			raw:  `{"meta":{"schema_version":"1","hostname":"bam","timestamp":"2026-08-15T09:35:02Z","tick_seq":1,"mode":"tick","deep_component":null,"window":"10min","duration_ms":1,"truncated":true,"collector_errors":[]},"kernel":{"count":0,"truncated":true,"dropped_entries":0,"entries":[]}}`,
		},
		{
			name: "services section truncated=true with dropped_entries=0",
			raw:  `{"meta":{"schema_version":"1","hostname":"bam","timestamp":"2026-08-15T09:35:02Z","tick_seq":1,"mode":"tick","deep_component":null,"window":"10min","duration_ms":1,"truncated":true,"collector_errors":[]},"services":{"count":0,"truncated":true,"dropped_entries":0,"entries":[],"failed_units":[]}}`,
		},
		{
			name: "sensors section truncated=true with dropped_entries=0",
			raw:  `{"meta":{"schema_version":"1","hostname":"bam","timestamp":"2026-08-15T09:35:02Z","tick_seq":1,"mode":"tick","deep_component":null,"window":"10min","duration_ms":1,"truncated":true,"collector_errors":[]},"sensors":{"truncated":true,"dropped_entries":0,"chips":{}}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var inst any
			if err := json.Unmarshal([]byte(tc.raw), &inst); err != nil {
				t.Fatalf("fixture is not valid JSON: %v", err)
			}
			if err := sch.Validate(inst); err == nil {
				t.Errorf("facts.schema.json accepted a malformed document")
			}
		})
	}
}
