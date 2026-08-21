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
