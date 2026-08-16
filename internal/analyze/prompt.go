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

// The prompt/ directory holds every fixed prompt byte: the role document,
// the prompt skeleton, and the deep-dive response schema. No instruction,
// boundary paragraph or fence originates in a Go string literal, which is
// what keeps the prompt auditable as text. The variable payloads are of
// course marshalled from Go — the facts and finding JSON, the history
// projection's field names, and the validator's own message quoted in the
// retry correction. (The one model-facing file outside this directory is
// report.schema.json, which lives in internal/report because go:embed
// cannot cross packages.)
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
	SentinelMD      string
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
		SentinelMD: roleMD,
		Nonce:      nonce,
		HistoryN:   cfg.HistoryN,
		History:    strings.Join(historyLines, "\n"),
		FactsJSON:  string(factsJSON),
	}
	var b strings.Builder
	if err := promptTmpl.ExecuteTemplate(&b, "triage", data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// buildTriagePrompt is §6 step 3's prompt-budget enforcement:
// agy silently returns an empty response past a measured ~30KB prompt
// (contracts/analyze.md §6 step 3 — 35KB reproduced SUCCESS with an empty
// response and zero tokens), an order of magnitude under FACTS_MAX_BYTES
// (262144). The facts document itself is never touched — only the prompt
// rendered from a REDUCED COPY is, via collect.Truncate (reused, D2: never
// invents a second truncation algorithm) — so the raw-alert path and
// everything else that reads collect's own output is unaffected.
//
// Because the "shell" (sentinel.md + boundary + HISTORY + TASK) has a
// fixed size independent of the facts payload, one render is enough to
// compute it exactly (shellLen = fullLen - factsJSONLen) — no guessing,
// no iteration: reduce once against the exact remaining budget and
// re-render.
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

// deepCopyFacts returns an independent copy of f: facts.Section[T] fields
// are pointers, so a shallow struct copy would still alias the original's
// slices — collect.Truncate mutates in place, and the ORIGINAL facts must
// stay exactly what collect emitted (the raw-alert path and the §5
// fallback both read Options.Facts directly).
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
		SentinelMD:  roleMD,
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

// buildDeepDivePrompt is §6 step 10's PROMPT_MAX_BYTES
// enforcement, applying the exact same exact-arithmetic technique as
// buildTriagePrompt, to the deep collect document instead of
// the tick facts. This is critical, not cosmetic (main's own live-gate
// finding, round 6): a deep collect can reach FACTS_MAX_BYTES (262144) —
// on Linux a single argv string past MAX_ARG_STRLEN (128 KiB) fails
// execve with E2BIG, and anything past ~30KB is agy's silent empty-answer
// cliff (§6 step 3). Unbudgeted, deep dive fails systematically for every
// realistic deep collect — a 24h ZED window, exactly the case A9 exists
// to analyze.
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
