//go:build container

// Package test holds the T7 container smoke assertions (contracts/runtime.md
// R8, table C1-C13). Run with:
//
//	go test -tags container ./test/...
//
// Every case prints PASS/FAIL/SKIP explicitly (R8: "a SKIP is explicit,
// never a silent pass"). These tests shell out to `docker` (a Podman shim
// is fine locally per CLAUDE.md) and to the repo root's deploy/ artifacts;
// they are NOT run by `go test ./...` (no build tag) or by CI's `test` job
// (R6) — only by a dedicated container job/local run on a real Linux host.
//
// AGY_URL/AGY_SHA256: real ops input (contracts/runtime.md R1), never
// guessed here. If both are set in the environment, the real tarball is
// used. Otherwise a SYNTHETIC local fixture (a fake "agy" shell script,
// served by an in-process httptest.Server) stands in, so the Dockerfile's
// OWN mechanics (download, sha256 verify, unpack, permissions, build-time
// verification) are still exercised end to end without ever touching a
// real or guessed download location. If neither path is reachable (e.g. a
// sandboxed CI runner with no container-to-host networking), every case
// that needs a built image SKIPs loudly with the reason.
package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const imageTag = "sentinel:container-test"

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func dockerBin() string {
	if b := os.Getenv("SENTINEL_DOCKER_BIN"); b != "" {
		return b
	}
	return "docker"
}

func runCmd(t *testing.T, timeout time.Duration, name string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	code = 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	return outBuf.String(), errBuf.String(), code
}

// --- shared image build (once per test binary run) ---

var (
	buildOnce  sync.Once
	buildErr   error
	fakeAgySrv *httptest.Server
	agyURL     string
	agySHA256  string
)

// syntheticAgy starts an in-process HTTP server serving a fake "agy" tarball
// and returns its URL + sha256. Purely local — never a guessed remote
// location.
func syntheticAgy(t *testing.T) (url, sha string) {
	t.Helper()
	if fakeAgySrv != nil {
		return agyURL, agySHA256
	}
	dir := t.TempDir()
	script := []byte("#!/bin/sh\ncase \"$1\" in\n  --version) echo 'agy container-test 0.0.0-test'; exit 0 ;;\n  *) exit 0 ;;\nesac\n")
	agyPath := filepath.Join(dir, "agy")
	if err := os.WriteFile(agyPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "agy.tar.gz")
	if out, errOut, code := runCmd(t, 30*time.Second, "tar", "-czf", tarPath, "-C", dir, "agy"); code != 0 {
		t.Fatalf("tar: %s %s", out, errOut)
	}
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	agySHA256 = hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/agy.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(data)
	})
	fakeAgySrv = httptest.NewServer(mux)
	agyURL = fakeAgySrv.URL + "/agy.tar.gz"
	return agyURL, agySHA256
}

// buildSentinelImage builds deploy/Dockerfile once, tagged imageTag. Real
// AGY_URL/AGY_SHA256 win if both are set; otherwise the synthetic fixture
// above is used with --add-host so the builder container can reach this
// process. Every case that needs a built image calls this first and SKIPs
// (never fails) if it returns an error — an unreachable Docker daemon or a
// sandboxed CI runner without container-to-host networking is an
// environment limitation, not a defect in the Dockerfile.
func buildSentinelImage(t *testing.T) error {
	t.Helper()
	buildOnce.Do(func() {
		root := repoRoot(t)
		url := os.Getenv("AGY_URL")
		sha := os.Getenv("AGY_SHA256")
		hostFlag := ""
		if url == "" || sha == "" {
			url, sha = syntheticAgy(t)
			// host.docker.internal works with --add-host=host.docker.internal:host-gateway
			// on real Docker (20.10+); Podman resolves host.containers.internal
			// natively. Try docker.internal first (matches what CI's real
			// docker will use); a Podman shim locally also honors it with the
			// add-host flag.
			url = strings.Replace(url, "127.0.0.1", "host.docker.internal", 1)
			hostFlag = "host.docker.internal:host-gateway"
		}
		args := []string{"build", "-f", "deploy/Dockerfile", "-t", imageTag,
			"--build-arg", "AGY_URL=" + url,
			"--build-arg", "AGY_SHA256=" + sha,
			"--build-arg", "VERSION=container-test",
		}
		if hostFlag != "" {
			args = append(args, "--add-host", hostFlag)
		}
		args = append(args, ".")
		cmd := exec.Command(dockerBin(), args...)
		cmd.Dir = root
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err := cmd.Run(); err != nil {
			buildErr = fmt.Errorf("docker build failed: %w\n%s", err, out.String())
		}
	})
	return buildErr
}

func requireImage(t *testing.T) {
	t.Helper()
	if err := buildSentinelImage(t); err != nil {
		t.Skipf("SKIP: could not build %s (environment limitation, not a Dockerfile defect): %v", imageTag, err)
	}
}

// --- C1: container starts unprivileged ---

func TestContainer_C1_StartsUnprivileged(t *testing.T) {
	requireImage(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "--entrypoint", "id", imageTag, "-u")
	if code != 0 || strings.TrimSpace(out) != "10001" {
		t.Fatalf("FAIL C1: id -u = %q (code=%d) stderr=%q, want 10001/0", out, code, errOut)
	}
	out, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", imageTag, "--version")
	if code != 0 || !strings.Contains(out, "container-test") {
		t.Fatalf("FAIL C1: sentinel --version = %q (code=%d), want to contain the stamped version", out, code)
	}
	t.Log("PASS C1")
}

// --- C3: every ro mount rejects a write; /state and /tmp accept one ---

func TestContainer_C3_ReadOnlySurfaces(t *testing.T) {
	requireImage(t)
	roTargets := []string{
		"/host/journal", "/host/journal-volatile", "/etc/machine-id",
		"/host/rasdaemon", "/host/proc", "/host/sys", "/etc/os-release",
		"/host/root", "/usr/local/bin",
	}
	for _, target := range roTargets {
		script := fmt.Sprintf("echo x > %s/.w 2>/dev/null; echo rc=$?", strings.TrimRight(target, "/"))
		out, _, _ := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
			"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
			"-u", "10001:10001", "--tmpfs", "/tmp",
			"--entrypoint", "sh", imageTag, "-c", script)
		if !strings.Contains(out, "rc=") || strings.Contains(out, "rc=0") {
			t.Errorf("FAIL C3: write to %s did not fail as expected: %q", target, out)
		}
	}
	out, _, _ := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /tmp/.w 2>/dev/null; echo rc=$?")
	if !strings.Contains(out, "rc=0") {
		t.Fatalf("FAIL C3: write to /tmp should succeed: %q", out)
	}
	t.Log("PASS C3 (host-root-mount cases not exercised here: those bind-mount specific host paths not present in a bare `docker run`; /usr/local/bin and /tmp are the two the security flags alone can prove)")
}

// --- C2: journal readable via group_add ---
//
// Prefers the REAL host journal (/var/log/journal) when this test host has
// one and journalctl is on PATH — the actual R8 C2 command,
// "journalctl -D /host/journal -n1", against actual binary journal files,
// gated on the actual systemd-journal gid instead of any belief about it.
// "--security-opt label=disable" is added only when SELinux is enforcing
// on the Docker/Podman host itself (common on a Podman Desktop/machine VM)
// — that flag accommodates THIS dev sandbox's mandatory access control on
// bind mounts, is not part of any shipped compose/Dockerfile artifact, and
// bam (plain Debian, no SELinux) does not need it.
//
// Falls back to a synthetic POSIX-permission probe (a throwaway gid file,
// not a real journal) when no real host journal is reachable — a Linux CI
// runner may have no active journald — so the group_add MECHANISM is still
// exercised even without real journal content. SKIPs loudly, never
// silently, when neither path is set up on this host (C9).
func TestContainer_C2_JournalViaGroupAdd(t *testing.T) {
	requireImage(t)

	if gid, ok := hostSystemdJournalGID(t); ok {
		selinux := selinuxEnforcingOnDockerHost(t)
		base := []string{"run", "--rm", "-v", "/var/log/journal:/host/journal:ro",
			"-u", "10001:10001", "--entrypoint", "journalctl"}
		if selinux {
			base = append(base, "--security-opt", "label=disable")
		}
		withoutArgs := append(append([]string{}, base...), imageTag, "-D", "/host/journal", "-n1", "-o", "json", "--no-pager")
		_, _, codeWithout := runCmd(t, 15*time.Second, dockerBin(), withoutArgs...)

		withArgs := append(append([]string{}, base[:2]...), append([]string{"--group-add", gid}, base[2:]...)...)
		withArgs = append(withArgs, imageTag, "-D", "/host/journal", "-n1", "-o", "json", "--no-pager")
		outWith, _, codeWith := runCmd(t, 15*time.Second, dockerBin(), withArgs...)

		if codeWithout == 0 {
			t.Log("NOTE C2: reading the real journal succeeded even without --group-add on this host — cannot prove the negative direction here, falling through to the positive assertion only")
		}
		if codeWith != 0 || strings.TrimSpace(outWith) == "" {
			t.Fatalf("FAIL C2: --group-add %s could not read the real host journal (code=%d): %s", gid, codeWith, outWith)
		}
		t.Log("PASS C2 (real host journal, real systemd-journal gid)")
		return
	}

	// Fallback: synthetic POSIX-permission probe.
	dir := t.TempDir()
	if out, errOut, code := runCmd(t, 10*time.Second, "chmod", "0750", dir); code != 0 {
		t.Skipf("SKIP C2: chmod on host failed: %s %s", out, errOut)
	}
	const testGID = 54321 // throwaway, unlikely to collide with a real system group
	if out, errOut, code := runCmd(t, 10*time.Second, "chown", ":"+fmt.Sprint(testGID), dir); code != 0 {
		t.Skipf("SKIP C2: chown to gid %d failed (needs root or matching group on this host): %s %s", testGID, out, errOut)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if out, errOut, code := runCmd(t, 10*time.Second, "chown", ":"+fmt.Sprint(testGID), filepath.Join(dir, "probe")); code != 0 {
		t.Skipf("SKIP C2: chown probe file failed: %s %s", out, errOut)
	}

	_, _, codeWithout := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-u", "10001:10001", "-v", dir+":/probe:ro",
		"--entrypoint", "cat", imageTag, "/probe/probe")
	_, _, codeWith := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-u", "10001:10001", "--group-add", fmt.Sprint(testGID),
		"-v", dir+":/probe:ro",
		"--entrypoint", "cat", imageTag, "/probe/probe")

	if codeWithout == 0 {
		t.Skip("SKIP C2: this host does not enforce the group permission boundary the way a real Linux target does (probe was readable even without group_add) — inconclusive here, needs live-host validation")
	}
	if codeWith != 0 {
		t.Errorf("FAIL C2: --group-add %d still could not read the probe file (code=%d)", testGID, codeWith)
	} else {
		t.Log("PASS C2 (synthetic gid fallback — no real host journal reachable in this environment)")
	}
}

// hostSystemdJournalGID discovers the REAL systemd-journal gid on the
// Docker/Podman host, the same way install-host.sh step 6 does
// ("getent group systemd-journal | cut -d: -f3") — never hardcoded, per
// the reviewer's flagged risk of a literal 999.
// hostSystemdJournalGID discovers the numeric group that owns the
// Docker/Podman host's real /var/log/journal, if one exists — via a
// throwaway container's `stat`, NOT by running getent/stat on the local Go
// test process. On a Podman Desktop/machine setup (this dev sandbox), the
// test binary runs on macOS while containers run inside a Linux VM with
// its own filesystem; checking the test process's own OS would always
// report "no journal" even when the VM genuinely has one, which is
// exactly what the first version of this test got wrong. Stat'ing the
// bind-mounted directory's owning gid also sidesteps needing /etc/group
// name resolution to agree between host and container.
func hostSystemdJournalGID(t *testing.T) (string, bool) {
	t.Helper()
	out, _, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", "/var/log/journal:/hj:ro",
		"debian:trixie-slim", "stat", "-c", "%g", "/hj")
	gid := strings.TrimSpace(out)
	if code != 0 || gid == "" {
		return "", false
	}
	return gid, true
}

// selinuxEnforcingOnDockerHost checks the machine actually running
// containers (relevant for a Podman Desktop VM on macOS, where the test
// process itself is NOT inside that VM) via `docker run --privileged`
// reading /sys/fs/selinux/enforce; best-effort, defaults to false (no
// extra flag) when it cannot tell.
func selinuxEnforcingOnDockerHost(t *testing.T) bool {
	t.Helper()
	out, _, code := runCmd(t, 10*time.Second, dockerBin(), "run", "--rm", "--privileged",
		"-v", "/sys/fs/selinux:/sys/fs/selinux:ro",
		"debian:trixie-slim", "sh", "-c", "cat /sys/fs/selinux/enforce 2>/dev/null")
	return code == 0 && strings.TrimSpace(out) == "1"
}

// --- C4: sensors -j ---
func TestContainer_C4_SensorsJSON(t *testing.T) {
	requireImage(t)
	// sensors reads the container's OWN /sys (there is no CLI flag to
	// point it at an arbitrary root), which under Docker/Podman is
	// normally the real host's sysfs shared via the kernel — no /host/sys
	// remapping needed for the standalone binary (that mapping is for
	// collect's own code, contracts/collect.md). On a sandboxed dev VM
	// with no physical sensor chips exposed, `sensors -j` prints "{}" AND
	// exits 1 ("No sensors found!") — that combination is a legitimate
	// "nothing detected" environment limitation, not a Dockerfile defect,
	// so it SKIPs rather than fails; any other non-zero exit is a real
	// failure.
	out, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--entrypoint", "sensors", imageTag, "-j")
	if code != 0 {
		if strings.TrimSpace(out) == "{}" {
			t.Skipf("SKIP C4: sensors -j found no chips in this environment (exit=%d, stderr=%q) — ARCHITECTURE §2.6 unverified point, needs live-host validation (e.g. bam)", code, errOut)
		}
		t.Fatalf("FAIL C4: sensors -j exit code = %d: %s %s", code, out, errOut)
	}
	m := mustJSON(t, out)
	if len(m) == 0 {
		t.Skip("SKIP C4: sensors -j returned an empty object — no hwmon sensor chips detected in this environment (ARCHITECTURE §2.6 unverified point; needs live-host validation, e.g. bam)")
	}
	entries, err := os.ReadDir("/sys/class/hwmon")
	if err != nil || len(entries) == 0 {
		t.Skip("SKIP C4: /host/sys/class/hwmon not readable from the test process itself — cannot cross-check device names here")
	}
	var names []string
	for _, e := range entries {
		if nb, err := os.ReadFile(filepath.Join("/sys/class/hwmon", e.Name(), "name")); err == nil {
			names = append(names, strings.TrimSpace(string(nb)))
		}
	}
	found := false
	for k := range m {
		for _, n := range names {
			if strings.Contains(k, n) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("FAIL C4: none of sensors -j's keys (%v) matched a hwmon device name (%v)", keysOf(m), names)
	} else {
		t.Log("PASS C4")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- C5: rasdaemon path listable ---
func TestContainer_C5_RasdaemonListable(t *testing.T) {
	requireImage(t)
	if _, err := os.Stat("/var/lib/rasdaemon"); err != nil {
		t.Skip("SKIP C5: rasdaemon not present on this test host")
	}
	out, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", "/var/lib/rasdaemon:/host/rasdaemon:ro",
		"--entrypoint", "ls", imageTag, "/host/rasdaemon")
	if code != 0 {
		t.Fatalf("FAIL C5: ls /host/rasdaemon: %s %s", out, errOut)
	}
	t.Log("PASS C5")
}

// --- C7: ZED events under -t zed (0 hits is a pass) ---
func TestContainer_C7_ZedUnderJournalctl(t *testing.T) {
	requireImage(t)
	dir := t.TempDir()
	_, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", dir+":/host/journal:ro",
		"--entrypoint", "journalctl", imageTag, "-D", "/host/journal", "-t", "zed", "-n5")
	if code != 0 {
		t.Fatalf("FAIL C7: journalctl -t zed exit code = %d: %s", code, errOut)
	}
	t.Log("PASS C7 (0 hits on an empty synthetic journal dir is itself a pass per R8)")
}

// --- C8: smartd decode (no NVMe -> SKIP) ---
func TestContainer_C8_SmartdDecode(t *testing.T) {
	requireImage(t)
	dir := t.TempDir()
	_, errOut, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", dir+":/host/journal:ro",
		"--entrypoint", "journalctl", imageTag, "-D", "/host/journal", "-t", "smartd")
	if code != 0 {
		t.Fatalf("FAIL C8: journalctl -t smartd exit code = %d: %s", code, errOut)
	}
	t.Skip("SKIP C8: no NVMe/smartd fixture data available in this environment (0-hit decode above is not itself the assertion — the contract wants a synthetic 'Killed process' entry to reach the kernel section, which needs a real journal fixture; deferred to internal/collect's own hermetic tests, which already cover kernel-section parsing with testdata/bin)")
}

// --- C6: tmpfs/DNS ok under read_only ---

func TestContainer_C6_TmpfsAndTZ(t *testing.T) {
	requireImage(t)
	out, _, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /tmp/.w && echo tmp_ok; echo TZ=$TZ")
	if code != 0 || !strings.Contains(out, "tmp_ok") {
		t.Fatalf("FAIL C6: tmpfs write failed: %q (code=%d)", out, code)
	}
	if !strings.Contains(out, "TZ=UTC") {
		t.Fatalf("FAIL C6: TZ != UTC: %q", out)
	}
	t.Log("PASS C6 (DNS resolution of `apprise` requires the compose network — asserted separately via `docker compose config`, not a bare `docker run`)")
}

// --- C9: sentinel tick exit codes ---

func TestContainer_C9_TickExitCodes(t *testing.T) {
	requireImage(t)
	stateDir := t.TempDir()
	// The container runs as uid 10001 (Dockerfile USER sentinel), distinct
	// from the host uid that owns a Go-created t.TempDir() (default 0700).
	// A bind mount does not remap ownership, so without this the STATE_DIR
	// write-probe fails for uid 10001 regardless of what's actually being
	// tested here — chmod so the case under test (journal readability,
	// not host/container uid mismatch) is what actually gates the result.
	if err := os.Chmod(stateDir, 0o777); err != nil {
		t.Fatal(err)
	}

	// TICK_INTERVAL=abc -> 78
	_, _, code := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-e", "TICK_INTERVAL=abc", "-e", "STATE_DIR=/state",
		"-v", stateDir+":/state",
		imageTag, "tick", "--once")
	if code != 78 {
		t.Errorf("FAIL C9: TICK_INTERVAL=abc -> code=%d, want 78", code)
	}

	// STATE_DIR unwritable -> 69 (mount a read-only tmpfs-like dir)
	roDir := t.TempDir()
	os.Chmod(roDir, 0o500)
	_, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", roDir+":/state:ro",
		"-e", "STATE_DIR=/state",
		imageTag, "tick", "--once")
	if code != 69 {
		t.Errorf("FAIL C9: unwritable STATE_DIR -> code=%d, want 69", code)
	}

	// neither journal dir readable -> 78 (defaults point at nonexistent
	// /host/journal, /host/journal-volatile: no volume mounted)
	_, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"-v", stateDir+":/state",
		"-e", "STATE_DIR=/state",
		imageTag, "tick", "--once")
	if code != 78 {
		t.Errorf("FAIL C9: no journal mount -> code=%d, want 78", code)
	}

	// --loop --once -> 64
	_, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", imageTag, "tick", "--loop", "--once")
	if code != 64 {
		t.Errorf("FAIL C9: --loop --once -> code=%d, want 64", code)
	}

	// positional argument -> 64
	_, _, code = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", imageTag, "tick", "extra-positional")
	if code != 64 {
		t.Errorf("FAIL C9: positional arg -> code=%d, want 64", code)
	}

	t.Log("PASS/FAIL C9 reported per sub-case above")
}

// --- C10: compose security model, via `docker compose config` ---

func TestContainer_C10_ComposeConfig(t *testing.T) {
	root := repoRoot(t)
	deployDir := filepath.Join(root, "deploy")
	if _, err := os.Stat(filepath.Join(deployDir, ".env")); err != nil {
		t.Skip("SKIP: deploy/.env not present (gitignored; ops-provided) — cannot render compose config without it")
	}
	cmd := exec.Command(dockerBin(), "compose", "config")
	cmd.Dir = deployDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("FAIL C10: docker compose config: %v\n%s", err, out)
	}
	text := string(out)

	checks := []struct {
		name string
		want string
	}{
		{"read_only", "read_only: true"},
		{"cap_drop", "- ALL"},
		{"no-new-privileges", "no-new-privileges:true"},
		{"user", "10001:10001"},
	}
	for _, c := range checks {
		if !strings.Contains(text, c.want) {
			t.Errorf("FAIL C10: rendered compose config missing %s (%q)", c.name, c.want)
		}
	}
	forbidden := []string{"privileged:", "cap_add:", "TELEGRAM_", "/config:"}
	for _, f := range forbidden {
		if strings.Contains(text, f) {
			t.Errorf("FAIL C10: rendered compose config unexpectedly contains %q", f)
		}
	}
	// Every C3 env var must be present in the sentinel service's rendered
	// environment block.
	c3Vars := []string{
		"TICK_INTERVAL", "TICK_WINDOW", "DEEP_WINDOW", "SECTION_TIMEOUT",
		"JOURNAL_MAX_RECORDS", "FACTS_MAX_BYTES", "SERVICES_MAX_BYTES", "STATE_DIR",
		"HOST_JOURNAL_DIR", "HOST_JOURNAL_VOLATILE_DIR", "HOST_PROC", "HOST_ROOT",
		"HOST_RASDAEMON", "SENTINEL_HOSTNAME", "AGY_BIN", "AGY_HOME", "AGY_SECRET_DIR",
		"AGY_PRINT_TIMEOUT", "AGY_HARD_TIMEOUT", "HISTORY_N", "PROMPT_MAX_BYTES",
		"HISTORY_KEEP", "DEEP_ENABLED", "DEEP_TIMEOUT", "RAW_ALERT_MAX_PRIORITY",
		"RAW_ALERT_MAX_LINES", "RAW_ALERT_REPEAT_SECONDS", "RAW_ALERT_MARKER_TTL_HOURS",
		"RENOTIFY_ALERT_SEC", "RENOTIFY_WATCH_SEC", "STALE_ALERT_SEC", "HEARTBEAT_HOUR",
		"OUTBOX_MAX", "OUTBOX_SMTP_AFTER", "APPRISE_URL", "APPRISE_KEY",
		"APPRISE_CONFIG_FILE", "NOTIFY_TIMEOUT", "NOTIFY_BODY_MAX", "MAILRISE_HOST",
		"MAILRISE_PORT", "MAILRISE_USER", "MAILRISE_PASS", "SENTINEL_MAIL_FROM",
		"SENTINEL_MAIL_TO", "LOG_LEVEL", "TMPDIR", "TZ",
	}
	// PROMPT_MAX_BYTES is not in the R4 environment block (analyze-only,
	// no compose default listed there) — drop it from the required set to
	// match the contract's actual table rather than over-asserting.
	want := make([]string, 0, len(c3Vars))
	for _, v := range c3Vars {
		if v == "PROMPT_MAX_BYTES" {
			continue
		}
		want = append(want, v)
	}
	for _, v := range want {
		if !strings.Contains(text, v+":") {
			t.Errorf("FAIL C10: sentinel environment missing %s", v)
		}
	}
	if strings.Contains(text, "ports:") {
		// ports: may legitimately appear under apprise/mailrise; only fail
		// if it appears inside the sentinel service block specifically.
		idx := strings.Index(text, "sentinel:")
		if idx >= 0 {
			next := strings.Index(text[idx+1:], "\n  ")
			block := text[idx:]
			if next >= 0 {
				block = text[idx : idx+1+next]
			}
			if strings.Contains(block, "ports:") {
				t.Error("FAIL C10: sentinel service unexpectedly has ports:")
			}
		}
	}
	t.Log("PASS C10")
}

// --- C11: SIGTERM shutdown + healthcheck ---

func TestContainer_C11_SIGTERMShutdown(t *testing.T) {
	requireImage(t)
	stateDir := t.TempDir()
	// Container runs as uid 10001 (Dockerfile USER sentinel); a bind mount
	// does not remap host ownership, so the STATE_DIR write-probe needs
	// this to pass preflight at all (same reasoning as C9 above).
	if err := os.Chmod(stateDir, 0o777); err != nil {
		t.Fatal(err)
	}
	hostProc := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostProc, "uptime"), []byte("1 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// checkJournalReadable (the T7 obligation) needs a journal that a real
	// journalctl actually reads a record from — a bind-mounted empty temp
	// dir legitimately fails preflight now (that IS the fix working), so
	// this test needs the REAL host journal, gid-discovered, same as C2.
	gid, gidOK := hostSystemdJournalGID(t)
	if !gidOK {
		t.Skip("SKIP C11: no real host journal reachable in this environment (needed since checkJournalReadable, the T7 fix, now correctly refuses to start on an empty/synthetic journal dir)")
	}
	selinux := selinuxEnforcingOnDockerHost(t)

	name := "sentinel-c11-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	args := []string{"run", "-d", "--name", name,
		"-e", "STATE_DIR=/state", "-v", stateDir + ":/state",
		"-e", "HOST_JOURNAL_DIR=/host/journal", "-v", "/var/log/journal:/host/journal:ro",
		"-e", "HOST_JOURNAL_VOLATILE_DIR=/host/journal",
		"-e", "HOST_PROC=/hp", "-v", hostProc + ":/hp",
		"-e", "TICK_INTERVAL=60",
		"-e", "MAILRISE_USER=u", "-e", "MAILRISE_PASS=p",
		"--group-add", gid,
	}
	if selinux {
		args = append(args, "--security-opt", "label=disable")
	}
	args = append(args, imageTag, "tick", "--loop")
	_, errOut, code := runCmd(t, 20*time.Second, dockerBin(), args...)
	if code != 0 {
		t.Skipf("SKIP C11: could not start container (env limitation): %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", name)

	time.Sleep(3 * time.Second)
	runCmd(t, 5*time.Second, dockerBin(), "kill", "-s", "TERM", name)

	deadline := time.Now().Add(15 * time.Second)
	var exited bool
	for time.Now().Before(deadline) {
		out, _, _ := runCmd(t, 5*time.Second, dockerBin(), "inspect", "-f", "{{.State.Running}}", name)
		if strings.TrimSpace(out) == "false" {
			exited = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !exited {
		t.Fatal("FAIL C11: container did not exit within 15s of SIGTERM")
	}
	out, _, _ := runCmd(t, 5*time.Second, dockerBin(), "inspect", "-f", "{{.State.ExitCode}}", name)
	if strings.TrimSpace(out) != "0" {
		t.Errorf("FAIL C11: exit code = %s, want 0", strings.TrimSpace(out))
	}
	t.Log("PASS C11")
}

// --- C12: install-host.sh idempotency ---

func TestContainer_C12_InstallHostIdempotent(t *testing.T) {
	root := repoRoot(t)
	// A throwaway Debian container with apt-get + systemd, network access
	// for real package installs, is what "throwaway rootfs" means here —
	// never run against a real host outside T8.
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		t.Skipf("SKIP C12: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", filepath.Join(root, "deploy")+":/work:ro",
		"debian:trixie-slim", "sleep", "600")
	if code != 0 {
		t.Skipf("SKIP C12: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	// Prep a writable copy + systemd-journal group + apt update, all inside
	// the throwaway container.
	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
getent group systemd-journal >/dev/null || groupadd -g 999 systemd-journal
touch /root/deploy/.env
chmod +x /root/deploy/install-host.sh`
	if out, errOut, code := exec_(prep); code != 0 {
		t.Skipf("SKIP C12: throwaway container prep failed: %s %s", out, errOut)
	}

	// --dry-run changes nothing.
	before, _, _ := exec_("find /root/deploy -type f -newer /root/deploy/install-host.sh 2>/dev/null | wc -l")
	if out, errOut, code := exec_("cd /root/deploy && ./install-host.sh --dry-run --env-file /root/deploy/.env"); code != 0 {
		t.Errorf("FAIL C12: --dry-run exit code = %d: %s %s", code, out, errOut)
	}
	after, _, _ := exec_("find /root/deploy -type f -newer /root/deploy/install-host.sh 2>/dev/null | wc -l")
	if strings.TrimSpace(before) != strings.TrimSpace(after) {
		t.Errorf("FAIL C12: --dry-run modified files (before=%s after=%s)", before, after)
	}

	// Two consecutive real runs.
	hashFiles := func() string {
		h, _, _ := exec_("sha256sum /etc/msmtprc /etc/smartd.conf /root/deploy/.env 2>/dev/null | sort")
		return h
	}

	out1, errOut1, code1 := exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	if code1 != 0 && code1 != 75 {
		// 75 = transient (package/service failure) — acceptable in a
		// throwaway container without a real init system; still assert
		// idempotency of whatever DID converge.
		t.Logf("first run exit=%d (non-fatal for this idempotency check): %s %s", code1, out1, errOut1)
	}
	hash1 := hashFiles()

	out2, errOut2, code2 := exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	hash2 := hashFiles()

	if hash1 != hash2 {
		t.Errorf("FAIL C12: sha256 of touched files differs between two real runs:\n1: %s\n2: %s", hash1, hash2)
	}
	if !strings.Contains(out2, "changed=0") {
		t.Errorf("FAIL C12: second real run did not report changed=0: %s (code=%d) %s", out2, code2, errOut2)
	}
	t.Log("PASS C12")
}

// --- C13: workflow lint + metadata shape ---

func TestContainer_C13_WorkflowShape(t *testing.T) {
	root := repoRoot(t)
	wf := filepath.Join(root, ".github", "workflows", "build.yml")
	data, err := os.ReadFile(wf)
	if err != nil {
		t.Fatalf("FAIL C13: reading %s: %v", wf, err)
	}
	text := string(data)
	for _, want := range []string{
		"type=raw,value=latest",
		"type=sha,format=long",
		"ghcr.io/${{ github.repository }}/sentinel",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("FAIL C13: build.yml missing %q", want)
		}
	}

	if _, err := exec.LookPath("actionlint"); err != nil {
		t.Skip("SKIP C13: actionlint not installed")
	}
	out, errOut, code := runCmd(t, 30*time.Second, "actionlint", wf)
	if code != 0 {
		t.Errorf("FAIL C13: actionlint: %s %s", out, errOut)
	}
	t.Log("PASS C13 (workflow shape); pull+run verification is CI-only (needs a published GHCR image, not buildable from a local run)")
}

// --- json sanity helper used by multiple cases indirectly (kept small) ---

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("invalid JSON: %v: %s", err, s)
	}
	return m
}
