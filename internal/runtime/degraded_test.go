package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thiscantbeserious/agentic-server-supervisor/internal/analyze"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/collect"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/config"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/facts"
	"github.com/thiscantbeserious/agentic-server-supervisor/internal/report"
)

// degradedDeps wires a collector that always succeeds and an analyzer that
// always falls back, the shape of a live analyzer outage.
func degradedDeps(t *testing.T, cfg *config.Config, d Deps) Deps {
	t.Helper()
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return factsClean(), nil }
	d.AnalyzeRun = func(ctx context.Context, o analyze.Options, ad analyze.Deps) (*report.Report, error) {
		return analyze.Fallback(cfg, o.Seq, "agy_failed", o.Facts), errors.New("analyze: agy failed")
	}
	return d
}

// degradedSent counts the delivered messages that are about the analyzer
// outage. The daily heartbeat also lands on the recorder and says nothing
// about it.
func degradedSent(rec *appriseRecorder) int {
	n := 0
	for _, r := range rec.all() {
		if bytes.Contains(r.body, []byte("Analyzer unavailable")) {
			n++
		}
	}
	return n
}

// A single failed tick is a blip, not an incident: nothing is sent.
func TestDegraded_SingleFailureSendsNothing(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	d := degradedDeps(t, cfg, baseDeps(newStore(t, cfg)))

	Tick(context.Background(), cfg, 1, d)

	if n := degradedSent(rec); n != 0 {
		t.Fatalf("notifications = %d, want 0", n)
	}
}

// Sustained failure is an incident: exactly one alert once the configured
// window has actually elapsed, and no second one while it persists. The
// expected tick number is a literal, not derived from the code under test:
// asserting against the implementation's own formula would make this pass
// vacuously for any self-consistent (but wrong) threshold. cfg here uses
// testConfig's untouched defaults, TICK_INTERVAL=300s and
// DEGRADED_ALERT_AFTER=900s: ticks land at elapsed 0, 300, 600, 900, so the
// 4th tick is the first where `elapsed(900s) < DEGRADED_ALERT_AFTER(900s)`
// is false and the hold releases.
func TestDegraded_AlertsOnceAtThreshold(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	d := degradedDeps(t, cfg, baseDeps(newStore(t, cfg)))

	const wantThresholdTick = 4

	for seq := int64(1); seq <= wantThresholdTick+2; seq++ {
		cfg.Now = tickTime(cfg, seq)
		Tick(context.Background(), cfg, seq, d)
		switch {
		case seq < wantThresholdTick:
			if n := degradedSent(rec); n != 0 {
				t.Fatalf("tick %d: notifications = %d, want 0 before tick %d", seq, n, wantThresholdTick)
			}
		case seq == wantThresholdTick:
			if n := degradedSent(rec); n != 1 {
				t.Fatalf("tick %d: notifications = %d, want exactly 1 (the alert fires on this exact tick)", seq, n)
			}
		}
	}
	if n := degradedSent(rec); n != 1 {
		t.Fatalf("notifications = %d, want exactly 1 (no re-alert while the outage persists within the window)", n)
	}
}

// Recovery below the threshold is silent: no alert was sent, so there is
// nothing to resolve, and the marker is cleared.
func TestDegraded_RecoveryBelowThresholdIsSilent(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)
	d := degradedDeps(t, cfg, baseDeps(store))

	cfg.Now = tickTime(cfg, 1)
	Tick(context.Background(), cfg, 1, d)
	d.AnalyzeRun = stubAnalyzeReturning(okReport())
	cfg.Now = tickTime(cfg, 2)
	Tick(context.Background(), cfg, 2, d)

	if n := degradedSent(rec); n != 0 {
		t.Fatalf("notifications = %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, analyzerFailsFile)); !os.IsNotExist(err) {
		t.Fatalf("marker file survived recovery: %v", err)
	}
}

// The hold is state, not memory: a restart mid-outage must not hand the
// analyzer a fresh grace period.
func TestDegraded_CounterSurvivesRestart(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	d := degradedDeps(t, cfg, baseDeps(newStore(t, cfg)))

	const want = 4
	for seq := int64(1); seq < want; seq++ {
		cfg.Now = tickTime(cfg, seq)
		Tick(context.Background(), cfg, seq, d)
	}
	if n := degradedSent(rec); n != 0 {
		t.Fatalf("notifications = %d before the threshold, want 0", n)
	}
	// same StateDir, fresh store: the process restarted.
	d2 := degradedDeps(t, cfg, baseDeps(newStore(t, cfg)))
	cfg.Now = tickTime(cfg, want)
	Tick(context.Background(), cfg, want, d2)

	if n := degradedSent(rec); n != 1 {
		t.Fatalf("notifications = %d, want 1 (marker reset by restart?)", n)
	}
}

// A sustained outage that finally recovers must resolve through the normal
// all-clear path, reporting a condition a human actually saw (R3.5b):
// "Recovery from a sustained outage resolves through the normal all-clear
// path". Nothing in the existing suite drove an outage past the threshold
// and then back to healthy.
func TestDegraded_RecoveryAfterSustainedOutageSendsAllClear(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)
	d := degradedDeps(t, cfg, baseDeps(store))

	const want = 4
	var seq int64
	for seq = 1; seq <= want; seq++ {
		cfg.Now = tickTime(cfg, seq)
		Tick(context.Background(), cfg, seq, d)
	}
	if n := degradedSent(rec); n != 1 {
		t.Fatalf("notifications = %d after the threshold, want 1", n)
	}

	// analyze.Run's own computeResolved step would produce this key on
	// recovery; stubbed here directly since the injected AnalyzeRun bypasses
	// that step entirely.
	fallbackKey := analyze.Fallback(cfg, seq, "agy_failed", factsClean()).Findings[0].Key
	seq++
	recovered := okReport()
	recovered.Resolved = []string{fallbackKey}
	d.AnalyzeRun = stubAnalyzeReturning(recovered)
	cfg.Now = tickTime(cfg, seq)
	Tick(context.Background(), cfg, seq, d)

	var sawAllClear bool
	for _, r := range rec.all() {
		var p struct{ Title string }
		json.Unmarshal(r.body, &p)
		if strings.Contains(p.Title, "Resolved:") {
			sawAllClear = true
		}
	}
	if !sawAllClear {
		t.Fatal("no all-clear notification after the analyzer recovered from a sustained outage")
	}
}

// R3.5b's central promise: the alert, once it fires, still carries the raw
// kernel lines of the tick that finally sent it, "no hardware event is
// lost" even though the alert was held back. Every other test in this file
// uses factsClean() (zero kernel entries), which cannot exercise that
// promise at all; this fixture seeds real emerg/crit lines.
func TestDegraded_AlertCarriesRawKernelLinesAtThreshold(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL

	kernelFacts := factsWithKernelEntries([]facts.Entry{
		{TS: "2026-08-15T08:59:00Z", Priority: 0, Identifier: "kernel", Message: "EDAC: uncorrectable ECC error"},
		{TS: "2026-08-15T08:59:30Z", Priority: 2, Identifier: "kernel", Message: "nvme0n1: I/O error, dev nvme0n1"},
	})

	d := baseDeps(newStore(t, cfg))
	d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return kernelFacts, nil }
	d.AnalyzeRun = func(ctx context.Context, o analyze.Options, ad analyze.Deps) (*report.Report, error) {
		return analyze.Fallback(cfg, o.Seq, "agy_failed", o.Facts), errors.New("analyze: agy failed")
	}

	const want = 4
	for seq := int64(1); seq <= want; seq++ {
		cfg.Now = tickTime(cfg, seq)
		Tick(context.Background(), cfg, seq, d)
	}

	// The raw-alert path (deterministic, step 1b) also fires on this same
	// crit/emerg content, independently of the analyzer; this test is
	// about the FALLBACK alert specifically, so it picks that one request
	// out rather than assuming it is the only one delivered.
	var body string
	var found int
	for _, r := range rec.all() {
		var p struct{ Title, Body string }
		json.Unmarshal(r.body, &p)
		if strings.Contains(p.Title, "Analyzer unavailable") {
			body = p.Body
			found++
		}
	}
	if found != 1 {
		t.Fatalf("saw %d 'Analyzer unavailable' notifications, want exactly 1", found)
	}
	if !strings.Contains(body, "EDAC: uncorrectable ECC error") || !strings.Contains(body, "nvme0n1: I/O error") {
		t.Errorf("delivered body lost the raw kernel lines: %q", body)
	}
}

// DEGRADED_ALERT_AFTER=0 means no grace period: the very first degraded
// tick already satisfies "the outage lasted >= 0s" (elapsed 0 < 0 is
// false) and must alert immediately, not wait for a second tick.
func TestDegraded_ZeroGraceAlertsOnFirstTick(t *testing.T) {
	cfg := testConfig(t, tick0)
	cfg.DegradedAlertAfter = 0
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	d := degradedDeps(t, cfg, baseDeps(newStore(t, cfg)))

	Tick(context.Background(), cfg, 1, d)

	if n := degradedSent(rec); n != 1 {
		t.Fatalf("notifications = %d, want 1 on the very first degraded tick when DEGRADED_ALERT_AFTER=0", n)
	}
}

// A flapping collector during a genuine, sustained analyzer outage must not
// silence it: the collector-failure path leaves the analyzer-degraded
// marker untouched, so the outage's clock keeps running in wall-clock time
// regardless of how many unrelated collector ticks land in between. A
// design that instead cleared or restarted the marker on a collector
// failure would let unrelated collector flapping bridge or reset a real
// analyzer outage's timer, silencing it for as long as the collector kept
// flapping.
func TestDegraded_CollectorFlapDuringOutageDoesNotSilenceIt(t *testing.T) {
	cfg := testConfig(t, tick0)
	rec := newAppriseRecorder(t, 200)
	cfg.AppriseURL = rec.srv.URL
	store := newStore(t, cfg)
	d := baseDeps(store)
	d.AnalyzeRun = func(ctx context.Context, o analyze.Options, ad analyze.Deps) (*report.Report, error) {
		return analyze.Fallback(cfg, o.Seq, "agy_failed", o.Facts), errors.New("analyze: agy failed")
	}

	// Odd ticks (1, 3, 5, ...) are genuine degraded ticks, at elapsed
	// 0s, 600s, 1200s, ...; even ticks are collector failures, at elapsed
	// 300s, 900s, 1500s, .... With DEGRADED_ALERT_AFTER=900s, the first
	// odd tick past 900s elapsed is seq=5 (t=1200s): that is where the
	// alert must fire, PROVIDED the intervening collector failures at
	// seq=2 and seq=4 did not reset the outage's start time. If a
	// collector failure cleared the marker instead of leaving it alone,
	// seq=4's clear would have restarted the clock at t=900s, and seq=5
	// (only 300s later) would still be held.
	const lastSeq = 6
	var seq int64
	for seq = 1; seq <= lastSeq; seq++ {
		cfg.Now = tickTime(cfg, seq)
		if seq%2 == 0 {
			d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) {
				return nil, errors.New("collect: journalctl failed")
			}
		} else {
			d.CollectRun = func(ctx context.Context, o collect.Options) (*facts.Facts, error) { return factsClean(), nil }
		}
		Tick(context.Background(), cfg, seq, d)
	}

	if n := degradedSent(rec); n != 1 {
		t.Fatalf("notifications = %d after %d ticks (half of them collector failures spanning the outage), want exactly 1: a flapping collector must not silence a sustained analyzer outage", n, lastSeq)
	}
}
