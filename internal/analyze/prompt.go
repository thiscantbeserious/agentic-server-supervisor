// prompt.go: prompt construction. Embeds the prompt/ directory, renders the
// templates, generates the fence nonce, and keeps every prompt under the
// size at which agy silently stops answering.
//
// The binding spec is contracts/analyze.md.
package analyze

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"strings"
	"text/template"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/collect"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
)

// The prompt/ directory holds everything this package says TO the model:
// the role document, the prompt skeleton, and the deep-dive response
// schema. No instruction, boundary paragraph or fence originates in a Go
// string literal, which is what keeps the prompt auditable as text.
//
// The payloads are a different matter and some of their bytes are
// Go-authored: the history projection's field names, the validator message
// the correction quotes, and text this package itself wrote into an
// earlier tick's report — a withheld-recommendation note, a fallback
// placeholder — which returns here as history evidence. Auditing what the
// model is told means reading this directory; auditing everything it sees
// means following the payloads too.
//
// (The one model-facing file outside this directory is report.schema.json,
// which lives in internal/report because go:embed cannot cross packages.)
//
// text/template, not html/template: HTML escaping would corrupt the
// embedded JSON payloads. Both calls share one header define so the fence
// and boundary structure exists in exactly one place and cannot silently
// diverge between the triage and deep-dive prompts.
//
//go:embed prompt/role.md
var roleMDRaw string

// roleMD is the embedded role document with its trailing newline trimmed:
// the template supplies the blank-line separator that follows it, and the
// file's own newline would double it. The prompt is compared byte-for-byte
// in tests, so this matters.
var roleMD = strings.TrimRight(roleMDRaw, "\n")

//go:embed prompt/prompt.tmpl
var promptFS embed.FS

var promptTmpl = template.Must(template.ParseFS(promptFS, "prompt/prompt.tmpl"))

// promptData feeds both prompt templates; fields unused by one call stay
// zero. DeepDive selects the deep-dive boundary paragraph, which names all
// three of that prompt's fenced payloads.
type promptData struct {
	RoleMD          string
	DeepDive        bool
	Nonce           string
	HistoryN        int
	History         string
	FactsJSON       string
	FindingJSON     string
	DeepJSON        string
	Component       string
	ValidationError string
}

// newNonce returns 16 hex chars from crypto/rand — the per-run fence
// token. The fences are only a boundary if injected log text cannot
// predict them; a fresh random nonce per run is what makes a forged
// "end of fence" line inert.
func newNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// buildCorrection renders the retry suffix with the concrete validation
// error from the failed attempt (truncated; the text comes from our own
// validator and contains no log content, so it is safe to embed).
func buildCorrection(validationErr string) (string, error) {
	var b strings.Builder
	data := promptData{ValidationError: truncRunes(validationErr, 300)}
	if err := promptTmpl.ExecuteTemplate(&b, "correction", data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// renderTriagePrompt renders the triage prompt: role document, security
// boundary, history window, this tick's facts, task.
func renderTriagePrompt(cfg *config.Config, f *facts.Facts, historyLines []string, nonce string) (string, error) {
	factsJSON, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	data := promptData{
		RoleMD:    roleMD,
		Nonce:     nonce,
		HistoryN:  cfg.HistoryN,
		History:   strings.Join(historyLines, "\n"),
		FactsJSON: string(factsJSON),
	}
	var b strings.Builder
	if err := promptTmpl.ExecuteTemplate(&b, "triage", data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// buildTriagePrompt renders the triage prompt and, if it exceeds the size
// budget, re-renders it from a reduced copy of the facts. agy silently
// returns an empty answer past a measured ~30 KB prompt, an order of
// magnitude below the facts size cap, so an unbudgeted prompt fails in the
// worst way: successfully, with nothing. The non-facts shell has a fixed
// size, so one render yields the exact remaining budget — no iteration.
// The reduction uses the collector's own truncation on a copy; the
// original facts are never touched, because the fallback and raw-alert
// paths read them and must see exactly what the collector emitted.
func buildTriagePrompt(cfg *config.Config, f *facts.Facts, historyLines []string, nonce string) (string, error) {
	prompt, err := renderTriagePrompt(cfg, f, historyLines, nonce)
	if err != nil {
		return "", err
	}
	if len(prompt) <= cfg.PromptMaxBytes {
		return prompt, nil
	}

	factsJSON, err := json.Marshal(f)
	if err != nil {
		return "", err
	}
	shellLen := len(prompt) - len(factsJSON)
	budget := cfg.PromptMaxBytes - shellLen
	if budget < 1 {
		budget = 1 // pathological: the shell alone exceeds the budget; Truncate still degrades gracefully
	}

	reduced, err := deepCopyFacts(f)
	if err != nil {
		// Reduction failed — ship the oversized prompt rather than fail the
		// whole tick; agy will most likely return agy_empty and the
		// fallback path (which reads the UNREDUCED facts) still surfaces
		// every protected kernel line regardless.
		return prompt, nil
	}
	budgetCfg := *cfg
	budgetCfg.FactsMaxBytes = budget
	collect.Truncate(reduced, &budgetCfg)

	reducedPrompt, err := renderTriagePrompt(cfg, reduced, historyLines, nonce)
	if err != nil {
		return prompt, nil
	}
	return reducedPrompt, nil
}

// deepCopyFacts returns a fully independent copy. The facts sections are
// pointers, so a struct copy would still alias the slices the budget
// reduction mutates.
func deepCopyFacts(f *facts.Facts) (*facts.Facts, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	var cp facts.Facts
	if err := json.Unmarshal(b, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// renderDeepDivePrompt renders the deep-dive prompt: role document,
// security boundary, history window, the one candidate finding, the deep
// collection, task.
func renderDeepDivePrompt(cfg *config.Config, findingJSON, deepJSON string, historyLines []string, nonce, component string) (string, error) {
	data := promptData{
		RoleMD:      roleMD,
		DeepDive:    true,
		Nonce:       nonce,
		HistoryN:    cfg.HistoryN,
		History:     strings.Join(historyLines, "\n"),
		FindingJSON: findingJSON,
		DeepJSON:    deepJSON,
		Component:   component,
	}
	var b strings.Builder
	if err := promptTmpl.ExecuteTemplate(&b, "deepdive", data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// buildDeepDivePrompt applies the same budget technique to the deep-dive
// prompt. Not cosmetic: a deep collection can reach the full facts size
// cap, a single argv string that large fails exec outright on Linux, and
// anything past ~30 KB hits agy's silent-empty cliff — unbudgeted, the
// deep dive would fail systematically for exactly the large collections it
// exists to analyze.
func buildDeepDivePrompt(cfg *config.Config, findingJSON string, deepFacts *facts.Facts, historyLines []string, nonce, component string) (string, error) {
	deepJSON, err := json.Marshal(deepFacts)
	if err != nil {
		return "", err
	}
	prompt, err := renderDeepDivePrompt(cfg, findingJSON, string(deepJSON), historyLines, nonce, component)
	if err != nil {
		return "", err
	}
	if len(prompt) <= cfg.PromptMaxBytes {
		return prompt, nil
	}

	shellLen := len(prompt) - len(deepJSON)
	budget := cfg.PromptMaxBytes - shellLen
	if budget < 1 {
		budget = 1
	}

	reduced, err := deepCopyFacts(deepFacts)
	if err != nil {
		return prompt, nil // ship the oversized prompt rather than fail the whole deep-dive
	}
	budgetCfg := *cfg
	budgetCfg.FactsMaxBytes = budget
	collect.Truncate(reduced, &budgetCfg)

	reducedDeepJSON, err := json.Marshal(reduced)
	if err != nil {
		return prompt, nil
	}
	reducedPrompt, err := renderDeepDivePrompt(cfg, findingJSON, string(reducedDeepJSON), historyLines, nonce, component)
	if err != nil {
		return prompt, nil
	}
	return reducedPrompt, nil
}
