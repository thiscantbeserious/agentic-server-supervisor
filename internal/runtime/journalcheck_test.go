package runtime

// journalcheck_test.go: the T7 obligation recorded in PLAN.md above the T8
// entry — "R2's preflight must verify the journal is actually readable
// before the first tick". A stale JOURNAL_GID, an ineffective group_add,
// or a wrong HOST_JOURNAL_DIR mount all produce the SAME symptom: the
// container starts, the filesystem-level preflight (dir exists, has
// files) already passed, but journalctl itself returns nothing — which
// reads as "quiet system" forever. These tests drive checkJournalReadable
// directly with hand-written stubs (not the shared testdata/bin fixture
// keys) because the discriminating cases here are about exit code and
// byte-empty-vs-non-empty stdout, not about section routing.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writeStubJournalctl drops an executable "journalctl" stub at dir/journalctl
// implementing exactly the body given, and returns dir for prepending to
// PATH.
func writeStubJournalctl(t *testing.T, body string) string {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	path := filepath.Join(bin, "journalctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func withStubJournalctlOnPath(t *testing.T, body string) {
	t.Helper()
	bin := writeStubJournalctl(t, body)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestCheckJournalReadable_RealRecord is the happy path: journalctl exits 0
// and emits one real journal-export JSON record.
func TestCheckJournalReadable_RealRecord(t *testing.T) {
	withStubJournalctlOnPath(t, `echo '{"MESSAGE":"boot"}'`)
	cfg := testConfig(t, tick0)

	if err := checkJournalReadable(context.Background(), cfg); err != nil {
		t.Fatalf("checkJournalReadable() = %v, want nil on a real record", err)
	}
}

// TestCheckJournalReadable_EmptyExitZero is the exact trap the reviewer
// measured against real journalctl in debian:trixie-slim: an unreadable
// (or genuinely empty) journal exits 0 and, WITHOUT -o json, prints the
// sentinel line "-- No entries --" to stdout — which a naive
// exit-code-only OR non-empty-stdout check both wave through. The stub
// exits 0 and writes literally nothing, modeling the "-o json" case the
// real check must use.
func TestCheckJournalReadable_EmptyExitZero(t *testing.T) {
	withStubJournalctlOnPath(t, `exit 0`)
	cfg := testConfig(t, tick0)

	err := checkJournalReadable(context.Background(), cfg)
	if err == nil {
		t.Fatal("checkJournalReadable() = nil, want an error on zero entries (exit 0, empty stdout)")
	}
}

// TestCheckJournalReadable_NoEntriesSentinelLine is the SAME trap as above
// but modeling the real journalctl behavior precisely: exit 0 with
// "-- No entries --\n" on stdout (not -o json). A check that only tests
// "stdout non-empty" is fooled by this line; checkJournalReadable must use
// -o json so this line never appears in the first place, and even if the
// stub sends it anyway (defense in depth for this test), the check must
// not accept it as a real record because it must have been invoked with
// -o json in the first place — a JSON decoder would reject this line.
func TestCheckJournalReadable_NoEntriesSentinelLine(t *testing.T) {
	withStubJournalctlOnPath(t, `echo -- No entries --`)
	cfg := testConfig(t, tick0)

	err := checkJournalReadable(context.Background(), cfg)
	if err == nil {
		t.Fatal("checkJournalReadable() = nil, want an error: the real journalctl's own \"-- No entries --\" line must never be read as a record")
	}
}

// TestCheckJournalReadable_NonZeroExit: an outright journalctl failure
// (e.g. permission denied reading the journal files despite the directory
// listing succeeding) must also fail the check.
func TestCheckJournalReadable_NonZeroExit(t *testing.T) {
	withStubJournalctlOnPath(t, `echo "journalctl: Failed to open" >&2; exit 1`)
	cfg := testConfig(t, tick0)

	err := checkJournalReadable(context.Background(), cfg)
	if err == nil {
		t.Fatal("checkJournalReadable() = nil, want an error on a non-zero exit")
	}
}

// TestCheckJournalReadable_UsesJSONFlag asserts the real fix for the
// reviewer's point 2: the invocation must pass "-o json" (or equivalent),
// proven here by a stub that only emits its record when it sees that flag
// and otherwise prints the real journalctl's "no -o json" empty-journal
// text. If checkJournalReadable ever stops passing -o json, this stub
// starts returning the plain-text sentinel instead of JSON and the check
// must reject it.
func TestCheckJournalReadable_UsesJSONFlag(t *testing.T) {
	withStubJournalctlOnPath(t, `
for a in "$@"; do
  if [ "$a" = "json" ]; then
    echo '{"MESSAGE":"boot"}'
    exit 0
  fi
done
echo -- No entries --
exit 0
`)
	cfg := testConfig(t, tick0)

	if err := checkJournalReadable(context.Background(), cfg); err != nil {
		t.Fatalf("checkJournalReadable() = %v, want nil — the stub only emits a record when -o json is passed", err)
	}
}

// TestCheckJournalReadable_UsesConfiguredDir proves the check queries the
// CONFIGURED journal mount (HOST_JOURNAL_DIR / HOST_JOURNAL_VOLATILE_DIR),
// not a bare invocation reading the test host's own /var/log/journal
// (which does not exist in this mount layout at all — R4 mounts it at
// /host/journal). A stub that only succeeds when it is invoked with
// exactly the configured directory (via -D) proves this.
func TestCheckJournalReadable_UsesConfiguredDir(t *testing.T) {
	withStubJournalctlOnPath(t, `
want="$JOURNALCTL_TEST_WANT_DIR"
prev=""
for a in "$@"; do
  if [ "$prev" = "-D" ] && [ "$a" = "$want" ]; then
    echo '{"MESSAGE":"boot"}'
    exit 0
  fi
  prev="$a"
done
exit 1
`)
	cfg := testConfig(t, tick0)
	t.Setenv("JOURNALCTL_TEST_WANT_DIR", cfg.HostJournalDir)

	if err := checkJournalReadable(context.Background(), cfg); err != nil {
		t.Fatalf("checkJournalReadable() = %v, want nil — the stub only succeeds when -D targets cfg.HostJournalDir", err)
	}
}

// TestStartupPreflight_JournalUnreadable_Exit78 is the integration point:
// the filesystem-level preflight (dir exists, non-empty listing) already
// passes in testConfig (a placeholder ".keep" file satisfies it), so the
// only thing that can still fail here is the new journalctl-level check —
// proving it is actually wired into StartupPreflight, not just a helper
// nobody calls.
func TestStartupPreflight_JournalUnreadable_Exit78(t *testing.T) {
	withStubJournalctlOnPath(t, `exit 0`) // exits clean, emits nothing: the GID-drift symptom
	cfg := testConfig(t, tick0)

	code, err := StartupPreflight(cfg)
	if code != 78 {
		t.Errorf("code = %d, want 78", code)
	}
	if err == nil {
		t.Error("err = nil, want non-nil naming the journal readability failure")
	}
}

// TestStartupPreflight_JournalReadable_Passes is the mirror: a healthy
// journalctl must not block startup.
func TestStartupPreflight_JournalReadable_Passes(t *testing.T) {
	withStubJournalctlOnPath(t, `echo '{"MESSAGE":"boot"}'`)
	cfg := testConfig(t, tick0)

	code, err := StartupPreflight(cfg)
	if code != 0 || err != nil {
		t.Errorf("StartupPreflight() = %d, %v, want 0, nil", code, err)
	}
}

// TestCheckJournalReadable_RealJournalctl_Gated is C9's gated-test
// requirement: this is the one test in this file that talks to a REAL
// journalctl rather than a stub belief about one, proving the "-o json
// exits 0 with 0 bytes on an empty/unreadable journal" behavior this
// whole check is built on actually holds outside our own fixtures — the
// exact behavior the reviewer measured directly in debian:trixie-slim and
// the coordinator confirmed independently (PLAN.md amendment 29c385a).
// Gated on SENTINEL_CONTAINER=1 (this repo's real-infrastructure marker,
// C9) AND a present journalctl binary; skips LOUDLY (t.Skip with a named
// reason) rather than silently passing when either is absent — this dev
// machine (macOS) has no journalctl at all, so unconditionally requiring
// it would fail every local run instead of proving anything.
func TestCheckJournalReadable_RealJournalctl_Gated(t *testing.T) {
	if os.Getenv("SENTINEL_CONTAINER") != "1" {
		t.Skip("SKIP: SENTINEL_CONTAINER=1 not set — this test requires a real journalctl binary (container/Linux only)")
	}
	if _, err := exec.LookPath("journalctl"); err != nil {
		t.Skip("SKIP: journalctl not found on PATH — this test requires a real journalctl binary")
	}

	cfg := testConfig(t, tick0)
	// An empty tempdir mounted as HOST_JOURNAL_DIR: no journal files at
	// all, which is the "unreadable/empty" case in miniature — real
	// journalctl must exit 0 and write 0 bytes under -o json, exactly the
	// behavior checkJournalReadable relies on to fail loud rather than
	// silently reading forever from nothing.
	cfg.HostJournalDir = t.TempDir()
	cfg.HostJournalVolatileDir = t.TempDir()

	if err := checkJournalReadable(context.Background(), cfg); err == nil {
		t.Error("checkJournalReadable() = nil against a real journalctl with no journal files — want an error (empty journal must fail loud)")
	}
}
