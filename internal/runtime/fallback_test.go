package runtime

import (
	"testing"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/dedup"
)

// R3.5: the collector fallback's dedup key is the fixed
// dedup.Key("meta", "collector unavailable"), deliberately decoupled from
// the finding's evidence (the real captured error/stderr text, which
// varies tick to tick), so that state re-notifies on the ALERT window
// instead of authoring a new key, and a resolve for the one before it,
// every single tick. Regression guard for report.schema.json's evidence
// description, which now says the evidence field itself stays real
// (captured, not invented) even for this LLM-free path, unlike the
// analyzer-unavailable fallback's evidence, which is a fixed synthetic
// phrase.
func TestCollectorUnavailable_KeyStableAcrossErrorTextEvidenceStaysReal(t *testing.T) {
	cfg := testConfig(t, tick0)

	a := CollectorUnavailable(cfg, 1, "journalctl: permission denied")
	b := CollectorUnavailable(cfg, 2, "journalctl: exit status 1, no such directory")

	if a.Findings[0].Key != b.Findings[0].Key {
		t.Fatalf("key changed with the captured error text: %q vs %q", a.Findings[0].Key, b.Findings[0].Key)
	}
	want := dedup.Key("meta", "collector unavailable")
	if a.Findings[0].Key != want {
		t.Fatalf("key = %q, want the fixed dedup.Key(%q, %q) = %q", a.Findings[0].Key, "meta", "collector unavailable", want)
	}
	if a.Findings[0].Evidence == b.Findings[0].Evidence {
		t.Fatal("evidence identical across two different captured errors; want it to carry the real, differing error text, not a synthetic constant")
	}
	if a.Findings[0].Evidence != "journalctl: permission denied" {
		t.Errorf("evidence = %q, want the real captured error text verbatim", a.Findings[0].Evidence)
	}
}
