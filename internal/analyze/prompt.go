package analyze

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// sentinelMD is the embedded role/instructions document (§7.3). go:embed
// cannot escape its package directory (C1), so it lives here rather than
// under internal/report or being read from disk.
//
//go:embed sentinel.md
var sentinelMD string

// templatesFS embeds the stage-1/stage-2 prompt skeletons (§7.1, §7.2).
// text/template, not html/template: the latter HTML-escapes the payload
// and would corrupt the embedded facts/finding/deep JSON. The two stages
// share one "header" define (role doc + SECURITY BOUNDARY heading +
// HISTORY fence) so the fence/heading structure that tests 9/9b assert on
// exists in exactly one place — a change to it cannot silently diverge
// between stages the way two hand-copied string-builder blocks could.
//
//go:embed templates/*.tmpl
var templatesFS embed.FS

var promptTmpl = template.Must(template.ParseFS(templatesFS, "templates/*.tmpl"))

// promptData is the single struct passed to both stage templates; unused
// fields for a given stage are simply left zero.
type promptData struct {
	SentinelMD  string
	Stage2      bool // selects boundary_stage2 vs boundary_stage1 in header.tmpl (D8)
	Nonce       string
	HistoryN    int
	History     string
	FactsJSON   string
	FindingJSON string
	DeepJSON    string
	Component   string
}

const correctionBlock = `===== CORRECTION =====
Your previous answer was not a valid report document. Output ONE JSON object
only - no prose, no markdown fence, no explanation before or after it. It must
match the schema exactly: required keys status, headline, body, findings,
resolved; no additional keys; status must equal the highest finding severity
(alert -> ALERT, watch -> WATCH, otherwise OK). Do not emit "key", "meta",
"first_seen" or "occurrences".`

// newNonce returns 16 lowercase hex chars from 8 bytes of crypto/rand (§6
// step 1) — a fresh, unguessable fence token per Run so injected data
// cannot pre-empt the fence markers.
func newNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// historyFinding and historyProjection are the compact per-report
// projection injected into HISTORY (§6 step 2): {status, headline,
// findings:[{severity,component,key}], resolved}.
type historyFinding struct {
	Severity  string `json:"severity"`
	Component string `json:"component"`
	Key       string `json:"key"`
}

type historyProjection struct {
	Status   string           `json:"status"`
	Headline string           `json:"headline"`
	Findings []historyFinding `json:"findings"`
	Resolved []string         `json:"resolved"`
}

// historyLines returns the newest n history documents from
// ${stateDir}/history, oldest first, each rendered as one compact JSON
// line. Newest is determined by sort.Strings of the filenames (the
// <unix-seconds,10>-<tick_seq,6>.json naming written by state sorts
// chronologically as strings). Unreadable or unparseable files are skipped
// silently (§6 step 2); a missing dir yields nil.
func historyLines(stateDir string, n int) []string {
	dir := filepath.Join(stateDir, "history")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if n > 0 && len(names) > n {
		names = names[len(names)-n:]
	}

	lines := make([]string, 0, len(names))
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var r report.Report
		if err := json.Unmarshal(b, &r); err != nil {
			continue
		}
		proj := historyProjection{
			Status:   r.Status,
			Headline: r.Headline,
			Findings: []historyFinding{},
			Resolved: r.Resolved,
		}
		for _, f := range r.Findings {
			proj.Findings = append(proj.Findings, historyFinding{
				Severity: f.Severity, Component: f.Component, Key: f.Key,
			})
		}
		if proj.Resolved == nil {
			proj.Resolved = []string{}
		}
		line, err := json.Marshal(proj)
		if err != nil {
			continue
		}
		lines = append(lines, string(line))
	}
	return lines
}

// assembleStage1 builds the stage-1 prompt verbatim per §7.1. The
// SECURITY BOUNDARY paragraph itself lives in
// templates/boundary_stage1.tmpl — every substitution into prompt text,
// nonce included, goes through the one text/template engine.
func assembleStage1(cfg *config.Config, f *facts.Facts, hist []string, nonce string) (string, error) {
	factsJSON, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	data := promptData{
		SentinelMD: sentinelMD,
		Nonce:      nonce,
		HistoryN:   cfg.HistoryN,
		History:    strings.Join(hist, "\n"),
		FactsJSON:  string(factsJSON),
	}
	var b strings.Builder
	if err := promptTmpl.ExecuteTemplate(&b, "stage1", data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// assembleStage2 builds the stage-2 prompt verbatim per §7.2, selecting
// templates/boundary_stage2.tmpl (D8) via Stage2.
func assembleStage2(cfg *config.Config, findingJSON, deepJSON string, hist []string, nonce, component string) (string, error) {
	data := promptData{
		SentinelMD:  sentinelMD,
		Stage2:      true,
		Nonce:       nonce,
		HistoryN:    cfg.HistoryN,
		History:     strings.Join(hist, "\n"),
		FindingJSON: findingJSON,
		DeepJSON:    deepJSON,
		Component:   component,
	}
	var b strings.Builder
	if err := promptTmpl.ExecuteTemplate(&b, "stage2", data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// truncRunes truncates s to at most n runes, keeping the prefix.
func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
