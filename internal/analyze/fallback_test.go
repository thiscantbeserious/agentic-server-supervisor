package analyze

import (
	"testing"

	"github.com/thiscantbeserious/ai-ops-nanny/internal/dedup"
	"github.com/thiscantbeserious/ai-ops-nanny/internal/facts"
)

func factsWithKernelLine(seq int64, msg string) *facts.Facts {
	f := baseFacts(seq)
	f.Kernel = &facts.Section[facts.KernelData]{Data: facts.KernelData{
		Entries: []facts.Entry{
			{TS: "2026-08-15T09:00:00Z", Priority: 2, Identifier: "kernel", Message: msg},
		},
	}}
	return f
}

// The fallback dedups on the failure, not on the kernel text it carries:
// keying on the raw lines makes every changed line a new alert plus a
// resolve for the one before it, once per tick, for as long as the outage
// lasts.
//
// The two fixture messages differ only in the DEVICE name ("disk" vs
// "fan"), a non-numeric token. dedup.EvidenceCore (C6) masks bare numeric
// tokens to "#", so a pair differing only by a digit ("disk 1" vs "disk 2")
// collapses to the same key under the OLD raw-evidence implementation too,
// making that fixture pass for the wrong reason and giving zero regression
// protection for the fix this test exists to pin.
func TestFallback_KeyIsStableAcrossKernelContent(t *testing.T) {
	cfg := newTestConfig(t)
	a := Fallback(cfg, 1, "agy_failed", factsWithKernelLine(1, "disk is failing"))
	b := Fallback(cfg, 2, "agy_failed", factsWithKernelLine(2, "fan is failing"))

	if a.Findings[0].Key != b.Findings[0].Key {
		t.Fatalf("key changed with kernel content: %q vs %q", a.Findings[0].Key, b.Findings[0].Key)
	}
	if a.Meta == nil || !a.Meta.Degraded {
		t.Fatal("fallback report is not flagged degraded")
	}
}

// The analyzer's own outage key must never collide with the collector
// fallback's key: a collision would merge two distinct incidents (analyzer
// down vs. collector down) into one active-alert record, and resolving
// either would wrongly all-clear both.
func TestFallback_KeyDiffersFromCollectorUnavailable(t *testing.T) {
	cfg := newTestConfig(t)
	a := Fallback(cfg, 1, "agy_failed", factsWithKernelLine(1, "disk is failing"))
	collectorKey := dedup.Key("meta", "collector unavailable")

	if a.Findings[0].Key == "" {
		t.Fatal("fallback finding has no key")
	}
	if a.Findings[0].Key == collectorKey {
		t.Fatalf("analyzer fallback key collides with the collector-unavailable key: %q", a.Findings[0].Key)
	}
}
