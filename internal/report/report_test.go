package report

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// findingWith builds a valid WATCH finding and sets one extra field, so a
// fixture can isolate a single offending property.
func findingWith(field string, value any) map[string]any {
	f := map[string]any{
		"severity":    "watch",
		"component":   "zfs",
		"evidence":    "eid=1841 class=checksum pool='hotstore' cksum_errors=1",
		"explanation": "Single checksum error on one mirror member.",
	}
	f[field] = value
	return f
}

func minimalOK() map[string]any {
	return map[string]any{
		"status":   "OK",
		"headline": "All quiet",
		"body":     "Nothing to report this tick.",
		"findings": []any{},
		"resolved": []any{},
	}
}

// acceptCases and rejectCases are the shared fixture tables driving
// TestValidate_Accepts/TestValidate_Rejects AND the C9 cross-check
// TestValidateAgreesWithSchema below — a fixture added here is
// automatically checked against both Validate and report.schema.json.
var acceptCases = []struct {
	name string
	doc  map[string]any
}{
	{"minimal OK report", minimalOK()},
	{
		name: "WATCH report with deep-dive fields (ARCHITECTURE §2.7 ZFS CKSUM benchmark)",
		doc: map[string]any{
			"status":   "WATCH",
			"headline": "One checksum error on seagate-zvtazeam-crypt, mirror partner clean",
			"body":     "During the running scrub of pool hotstore, ZFS corrected exactly one checksum error.",
			"findings": []any{
				map[string]any{
					"severity":       "watch",
					"component":      "zfs",
					"evidence":       "eid=1841 class=checksum pool='hotstore' vdev=seagate-zvtazeam-crypt cksum_errors=1",
					"explanation":    "ZFS detected and corrected a single checksum mismatch.",
					"analysis":       "Transient, not a trend: one event, counter at 1.",
					"recommendation": "Wait for the scrub to finish.",
					"key":            "3f9c1a7e40b2d558",
					"first_seen":     1786864210,
					"occurrences":    1,
				},
			},
			"resolved": []any{},
			"meta":     map[string]any{"hostname": "bam", "tick_seq": 412},
		},
	},
}

func TestValidate_Accepts(t *testing.T) {
	for _, tc := range acceptCases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.doc)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			if _, err := Validate(raw); err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}

// schemaDivergesFromValidate names reject-case fixtures where Validate and
// report.schema.json legitimately disagree, so nobody "fixes" the gap in
// the wrong direction later. Currently: JSON Schema cannot express "status
// equals highest finding severity" (C5) — that rule is prose-only in the
// schema's description field — so a document with status=ALERT/WATCH and
// no findings of that severity is schema-valid but Validate-invalid.
var schemaDivergesFromValidate = map[string]bool{
	"status inconsistent with highest severity (alert finding but status WATCH)": true,
	"status OK with a watch finding present":                                     true,
	"status OK with an alert finding":                                            true,
}

var rejectCases = []struct {
	name   string
	mutate func(map[string]any)
}{
	{"invalid status enum", func(d map[string]any) { d["status"] = "CRITICAL" }},
	// contracts/analyze.md §10 case 12 lists nine validator negatives; these are
	// the ones the table was missing, plus the Validate bounds that had no
	// coverage at all (explanation, analysis, recommendation, first_seen).
	{"evidence exceeds 1000 runes", func(d map[string]any) {
		d["status"] = "WATCH"
		d["findings"] = []any{findingWith("evidence", repeatRune('e', 1001))}
	}},
	{"status OK with an alert finding", func(d map[string]any) {
		f := findingWith("severity", "alert")
		d["status"] = "OK"
		d["findings"] = []any{f}
	}},
	{"finding empty explanation", func(d map[string]any) {
		d["status"] = "WATCH"
		d["findings"] = []any{findingWith("explanation", "")}
	}},
	{"explanation exceeds 800 runes", func(d map[string]any) {
		d["status"] = "WATCH"
		d["findings"] = []any{findingWith("explanation", repeatRune('x', 801))}
	}},
	{"analysis exceeds 1200 runes", func(d map[string]any) {
		d["status"] = "WATCH"
		d["findings"] = []any{findingWith("analysis", repeatRune('a', 1201))}
	}},
	{"recommendation exceeds 800 runes", func(d map[string]any) {
		d["status"] = "WATCH"
		d["findings"] = []any{findingWith("recommendation", repeatRune('r', 801))}
	}},
	{"negative first_seen", func(d map[string]any) {
		d["status"] = "WATCH"
		d["findings"] = []any{findingWith("first_seen", -1)}
	}},
	// occurrences is omitempty, so an explicit 0 is indistinguishable from an
	// absent field after unmarshal — the schema's minimum:1 must still hold.
	{"explicit occurrences 0", func(d map[string]any) {
		d["status"] = "WATCH"
		d["findings"] = []any{findingWith("occurrences", 0)}
	}},
	{"negative occurrences", func(d map[string]any) {
		d["status"] = "WATCH"
		d["findings"] = []any{findingWith("occurrences", -3)}
	}},
	{"empty headline", func(d map[string]any) { d["headline"] = "" }},
	{"headline exceeds 80 runes", func(d map[string]any) { d["headline"] = repeatRune('h', 81) }},
	{"empty body", func(d map[string]any) { d["body"] = "" }},
	{"body exceeds 2000 runes", func(d map[string]any) { d["body"] = repeatRune('b', 2001) }},
	{"findings exceeds maxItems 20", func(d map[string]any) {
		findings := make([]any, 21)
		for i := range findings {
			findings[i] = map[string]any{
				"severity": "info", "component": "meta",
				"evidence": "x", "explanation": "x",
			}
		}
		d["findings"] = findings
		d["status"] = "OK" // info-only -> OK per mapping
	}},
	{"resolved exceeds maxItems 20", func(d map[string]any) {
		resolved := make([]any, 21)
		for i := range resolved {
			resolved[i] = "x"
		}
		d["resolved"] = resolved
	}},
	{"finding invalid severity", func(d map[string]any) {
		d["findings"] = []any{map[string]any{
			"severity": "critical", "component": "kernel",
			"evidence": "x", "explanation": "x",
		}}
	}},
	{"finding invalid component", func(d map[string]any) {
		d["findings"] = []any{map[string]any{
			"severity": "watch", "component": "cpu",
			"evidence": "x", "explanation": "x",
		}}
		d["status"] = "WATCH"
	}},
	{"finding empty evidence", func(d map[string]any) {
		d["findings"] = []any{map[string]any{
			"severity": "watch", "component": "kernel",
			"evidence": "", "explanation": "x",
		}}
		d["status"] = "WATCH"
	}},
	{"finding key not matching pattern", func(d map[string]any) {
		d["findings"] = []any{map[string]any{
			"severity": "watch", "component": "kernel",
			"evidence": "x", "explanation": "x", "key": "not-a-valid-key",
		}}
		d["status"] = "WATCH"
	}},
	{"status inconsistent with highest severity (alert finding but status WATCH)", func(d map[string]any) {
		d["findings"] = []any{map[string]any{
			"severity": "alert", "component": "kernel",
			"evidence": "x", "explanation": "x",
		}}
		d["status"] = "WATCH"
	}},
	{"status OK with a watch finding present", func(d map[string]any) {
		d["findings"] = []any{map[string]any{
			"severity": "watch", "component": "kernel",
			"evidence": "x", "explanation": "x",
		}}
		d["status"] = "OK"
	}},
	{"missing required field status", func(d map[string]any) { delete(d, "status") }},
	{"missing required field findings", func(d map[string]any) { delete(d, "findings") }},
	{"null findings", func(d map[string]any) { d["findings"] = nil }},
	{"missing required field resolved", func(d map[string]any) { delete(d, "resolved") }},
	{"null resolved", func(d map[string]any) { d["resolved"] = nil }},
	{"negative meta.tick_seq", func(d map[string]any) {
		d["meta"] = map[string]any{"tick_seq": -5}
	}},
}

func TestValidate_Rejects(t *testing.T) {
	for _, tc := range rejectCases {
		t.Run(tc.name, func(t *testing.T) {
			d := minimalOK()
			tc.mutate(d)
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			if _, err := Validate(raw); err == nil {
				t.Fatalf("Validate() expected an error, got nil")
			}
		})
	}
}

func TestValidate_RejectsMalformedJSON(t *testing.T) {
	if _, err := Validate([]byte("{not json")); err == nil {
		t.Fatal("Validate() expected an error for malformed JSON")
	}
}

func repeatRune(r rune, n int) string {
	out := make([]rune, n)
	for i := range out {
		out[i] = r
	}
	return string(out)
}

// TestValidateAgreesWithSchema is the C9 cross-package assertion: it drives
// EVERY fixture in acceptCases and rejectCases (the same tables backing
// TestValidate_Accepts/TestValidate_Rejects) through both report.Validate
// and report.schema.json, so a fixture added to catch one validator's bug
// automatically exercises the other. jsonschema/v6 is test-only (C5, D7) —
// never imported by runtime code. The one known, named, legitimate
// divergence is schemaDivergesFromValidate above (status/severity
// consistency is not expressible in JSON Schema).
func TestValidateAgreesWithSchema(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	schema, err := jsonschema.UnmarshalJSON(bytes.NewReader(SchemaJSON))
	if err != nil {
		t.Fatalf("unmarshal embedded schema: %v", err)
	}
	if err := compiler.AddResource("report.schema.json", schema); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	sch, err := compiler.Compile("report.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	for _, tc := range acceptCases {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.doc)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			if _, err := Validate(raw); err != nil {
				t.Errorf("Validate rejected a fixture the schema should accept: %v", err)
			}
			var inst any
			if err := json.Unmarshal(raw, &inst); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			if err := sch.Validate(inst); err != nil {
				t.Errorf("schema rejected a fixture Validate accepts: %v", err)
			}
		})
	}

	for _, tc := range rejectCases {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			d := minimalOK()
			tc.mutate(d)
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			if _, err := Validate(raw); err == nil {
				t.Errorf("Validate accepted a fixture that should be rejected")
			}
			var inst any
			if err := json.Unmarshal(raw, &inst); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			schemaErr := sch.Validate(inst)
			if schemaDivergesFromValidate[tc.name] {
				if schemaErr != nil {
					t.Errorf("%q is documented as a known Validate/schema divergence (schema should still ACCEPT it), but the schema now rejects it too — update schemaDivergesFromValidate", tc.name)
				}
				return
			}
			if schemaErr == nil {
				t.Errorf("schema accepted a fixture Validate rejects (and it is not in schemaDivergesFromValidate): %q", tc.name)
			}
		})
	}
}
