// Package test also holds a few static, non-container checks on
// install.sh's own source: assertions that need no docker, no network,
// and no throwaway rootfs, so they run under plain `go test ./...`
// alongside every other hermetic suite instead of only under the
// container-tagged run.
package test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// installShPath locates install.sh relative to this test file rather
// than via `git rev-parse` (used by the container suite's repoRoot):
// this file has no build tag and must stay hermetic and offline (C9),
// and runtime.Caller needs neither git nor a working directory
// assumption.
func installShPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "install.sh")
}

func readInstallSh(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(installShPath(t))
	if err != nil {
		t.Fatalf("reading install.sh: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("install.sh read as empty, this guard exists so a broken path silently passes nothing rather than asserting on an empty string")
	}
	return string(b)
}

// TestInstallSh_AptInstallNeverRemoves is what actually would have
// stopped the OMV incident regardless of any MTA-specific reasoning
// being right or wrong: `apt-get install -y` alone lets apt resolve a
// dependency conflict by removing whatever it likes (openmediavault,
// postfix, ten plugins) to satisfy the requested packages, silently,
// with no distinct warning from the ordinary "installing N packages"
// case. `--no-remove` makes apt fail loudly instead. Read from the
// script's own source rather than asserted against one known call site
// (step1's msmtp/msmtp-mta install): a fix that only touched that one
// line would still leave ensure_curl's `apt-get install curl` free to
// remove something to make room for curl.
func TestInstallSh_AptInstallNeverRemoves(t *testing.T) {
	src := readInstallSh(t)
	// A real invocation, not a comment explaining one and not an echoed
	// preview string ("would run: apt-get install ...", "apt-get install
	// failed"): those do not touch the package database, so requiring
	// --no-remove on their text would assert nothing about apt's actual
	// behavior. Matched by shape (the line, trimmed, actually calls the
	// command) rather than by line number, so a call moved to a new line
	// is still caught.
	re := regexp.MustCompile(`(?m)^\s*(?:if !\s*)?apt-get install\b.*$`)
	calls := re.FindAllString(src, -1)
	if len(calls) == 0 {
		t.Fatal("no `apt-get install` invocation found in install.sh; either the script moved this logic or the regex above needs updating, either way this test is currently asserting nothing")
	}
	for _, call := range calls {
		if !strings.Contains(call, "--no-remove") {
			t.Errorf("apt-get install call missing --no-remove, apt is free to remove unrelated packages to satisfy this request: %q", call)
		}
	}
}

// TestInstallSh_AptWaitsForLock: unattended-upgrades runs daily on a
// Debian or OpenMediaVault host and holds the dpkg frontend lock for
// the duration of its own run. apt-get's default is to fail at once
// when the lock is taken ("Could not get lock /var/lib/dpkg/lock-
// frontend. It is held by process N (unattended-upgr)"), which turned a
// timing coincidence into a whole failed install run, both in the VM
// gate and on a real host. `-o DPkg::Lock::Timeout=N` makes apt wait
// instead. Every real invocation is checked, including the simulated
// one (`apt-get install -s` still takes the lock when run as root,
// which this script always is), for the same reason
// TestInstallSh_AptInstallNeverRemoves checks every call rather than
// one known site.
func TestInstallSh_AptWaitsForLock(t *testing.T) {
	src := readInstallSh(t)
	// A real invocation only, at the start of a statement, an `if !`
	// test, or a `var="$(` capture. Comments, the dry-run preview line
	// and the "apt-get install failed" message do not touch the lock.
	re := regexp.MustCompile(`(?m)^\s*(?:if !\s*|\w+="\$\(\s*)?apt-get (?:install|update)\b.*$`)
	calls := re.FindAllString(src, -1)
	if len(calls) < 5 {
		t.Fatalf("found %d apt-get invocations in install.sh, want at least 5 (curl update+install, step1 update, simulation, step1 install); the regex above no longer matches the script's shape: %q", len(calls), calls)
	}
	for _, call := range calls {
		if !strings.Contains(call, `-o DPkg::Lock::Timeout="$APT_LOCK_TIMEOUT"`) {
			t.Errorf("apt-get call does not wait for the dpkg lock, a concurrent unattended-upgrades run fails it outright: %q", call)
		}
	}
	if !regexp.MustCompile(`(?m)^APT_LOCK_TIMEOUT=\d+$`).MatchString(src) {
		t.Error("APT_LOCK_TIMEOUT is not defined once as a plain integer constant at top level")
	}
}
