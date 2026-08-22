// Package report defines the Report wire type (C5) and its hand-written
// runtime validator. report.schema.json is normative and embedded so tests
// can assert Validate agrees with it; the schema itself is never linked
// into the runtime validation path (C5, C9).
package report

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
)

//go:embed report.schema.json
var SchemaJSON []byte

type Finding struct {
	Severity       string `json:"severity"`
	Component      string `json:"component"`
	Evidence       string `json:"evidence"`
	Explanation    string `json:"explanation"`
	Analysis       string `json:"analysis,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	Key            string `json:"key,omitempty"`
	FirstSeen      int64  `json:"first_seen,omitempty"`
	Occurrences    int    `json:"occurrences,omitempty"`
}

type Meta struct {
	Hostname string `json:"hostname,omitempty"`
	TickSeq  int64  `json:"tick_seq,omitempty"`
	Raw      bool   `json:"raw,omitempty"`
	Degraded bool   `json:"degraded,omitempty"`
}

type Report struct {
	Status   string    `json:"status"`
	Headline string    `json:"headline"`
	Body     string    `json:"body"`
	Findings []Finding `json:"findings"`
	Resolved []string  `json:"resolved"`
	Meta     *Meta     `json:"meta,omitempty"`
}

var validSeverity = map[string]int{"info": 1, "watch": 2, "alert": 3}
var validComponent = map[string]bool{
	"kernel": true, "ras": true, "smart": true, "sensors": true,
	"resources": true, "services": true, "network": true, "zfs": true, "meta": true,
}
var validStatus = map[string]bool{"OK": true, "WATCH": true, "ALERT": true}

var keyPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Validate is the executable form of report.schema.json: enums, rune-length
// bounds, array caps, and the status/highest-severity consistency rule. It
// does not use DisallowUnknownFields, unknown fields are stripped by
// ordinary json.Unmarshal, not rejected.
func Validate(raw []byte) (*Report, error) {
	// Presence check first: json.Unmarshal cannot itself tell "absent" from
	// "null" from "[]", and report.schema.json requires findings/resolved
	// (C5). Probe the raw object before the nil->[] output-marshaling
	// convention (C5) would paper over a missing/null field.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("report: invalid JSON: %w", err)
	}
	for _, field := range []string{"status", "headline", "body", "findings", "resolved"} {
		v, ok := probe[field]
		if !ok || string(v) == "null" {
			return nil, fmt.Errorf("report: %s: required", field)
		}
	}
	// Same absent-vs-present problem one level down: occurrences is omitempty,
	// so an explicit 0 unmarshals identically to an omitted field, while the
	// schema sets minimum 1. Probe the raw findings for it.
	var rawFindings []map[string]json.RawMessage
	if err := json.Unmarshal(probe["findings"], &rawFindings); err == nil {
		for i, rf := range rawFindings {
			if v, ok := rf["occurrences"]; ok && string(v) == "0" {
				return nil, fmt.Errorf("report: findings[%d].occurrences: must be >= 1", i)
			}
		}
	}

	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("report: invalid JSON: %w", err)
	}

	if !validStatus[r.Status] {
		return nil, fmt.Errorf("report: status: invalid enum value %q", r.Status)
	}
	if err := runeBounds("headline", r.Headline, 1, 80); err != nil {
		return nil, err
	}
	if err := runeBounds("body", r.Body, 1, 2000); err != nil {
		return nil, err
	}
	if len(r.Findings) > 20 {
		return nil, fmt.Errorf("report: findings: %d items exceeds maxItems 20", len(r.Findings))
	}
	if len(r.Resolved) > 20 {
		return nil, fmt.Errorf("report: resolved: %d items exceeds maxItems 20", len(r.Resolved))
	}

	highest := 0
	for i, f := range r.Findings {
		rank, ok := validSeverity[f.Severity]
		if !ok {
			return nil, fmt.Errorf("report: findings[%d].severity: invalid enum value %q", i, f.Severity)
		}
		if !validComponent[f.Component] {
			return nil, fmt.Errorf("report: findings[%d].component: invalid enum value %q", i, f.Component)
		}
		if err := runeBounds(fmt.Sprintf("findings[%d].evidence", i), f.Evidence, 1, 1000); err != nil {
			return nil, err
		}
		if err := runeBounds(fmt.Sprintf("findings[%d].explanation", i), f.Explanation, 1, 800); err != nil {
			return nil, err
		}
		if err := maxRunes(fmt.Sprintf("findings[%d].analysis", i), f.Analysis, 1200); err != nil {
			return nil, err
		}
		if err := maxRunes(fmt.Sprintf("findings[%d].recommendation", i), f.Recommendation, 800); err != nil {
			return nil, err
		}
		if f.Key != "" && !keyPattern.MatchString(f.Key) {
			return nil, fmt.Errorf("report: findings[%d].key: %q does not match ^[0-9a-f]{16}$", i, f.Key)
		}
		if f.FirstSeen < 0 {
			return nil, fmt.Errorf("report: findings[%d].first_seen: must be >= 0", i)
		}
		if f.Occurrences < 0 {
			return nil, fmt.Errorf("report: findings[%d].occurrences: must be >= 1", i)
		}
		if rank > highest {
			highest = rank
		}
	}
	for i, res := range r.Resolved {
		if err := runeBounds(fmt.Sprintf("resolved[%d]", i), res, 1, 80); err != nil {
			return nil, err
		}
	}

	if r.Meta != nil && r.Meta.TickSeq < 0 {
		return nil, fmt.Errorf("report: meta.tick_seq: must be >= 0")
	}

	wantStatus := statusForRank(highest)
	if r.Status != wantStatus {
		return nil, fmt.Errorf("report: status %q inconsistent with highest finding severity, want %q", r.Status, wantStatus)
	}

	return &r, nil
}

func statusForRank(rank int) string {
	switch {
	case rank >= 3:
		return "ALERT"
	case rank == 2:
		return "WATCH"
	default:
		return "OK"
	}
}

func runeBounds(field, s string, min, max int) error {
	n := len([]rune(s))
	if n < min || n > max {
		return fmt.Errorf("report: %s: length %d runes out of bounds [%d,%d]", field, n, min, max)
	}
	return nil
}

func maxRunes(field, s string, max int) error {
	if s == "" {
		return nil
	}
	n := len([]rune(s))
	if n > max {
		return fmt.Errorf("report: %s: length %d runes exceeds maxLength %d", field, n, max)
	}
	return nil
}
