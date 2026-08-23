// fallback.go: the ONE fallback runtime authors, the collector fallback
// (R3.5). The analyzer's own fallback is authored by internal/analyze and
// passed through unchanged (C8), runtime never builds it.
package runtime

import (
	"github.com/thiscantbeserious/ai-ops-nanny/internal/config"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/dedup"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/report"
)

// CollectorUnavailable builds R3.5's fallback report when collect.Run
// itself returns an error. The key is stable across ticks on purpose, so
// state re-notifies on the ALERT window instead of every tick.
func CollectorUnavailable(cfg *config.Config, seq int64, errText string) *report.Report {
	if errText == "" {
		errText = "unknown collector error"
	}
	key := dedup.Key("meta", "collector unavailable")
	explanation := "collect failed: " + truncRunesR(errText, 780)
	return &report.Report{
		Status:   "ALERT",
		Headline: "Collector unavailable",
		Body:     truncRunesR(errText, 2000),
		Findings: []report.Finding{{
			Severity:    "alert",
			Component:   "meta",
			Evidence:    truncRunesR(errText, 1000),
			Explanation: truncRunesR(explanation, 800),
			Key:         key,
		}},
		Resolved: []string{},
		Meta:     &report.Meta{Hostname: cfg.Hostname, TickSeq: seq},
	}
}

func truncRunesR(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
