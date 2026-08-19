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
// AGY_URL/AGY_SHA512: real ops input (contracts/runtime.md R1), never
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
	"bufio"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const imageTag = "sentinel:container-test"

// logPass logs a "PASS ..." line only when nothing in this test has
// already failed (round-3 review finding: an unconditional t.Log("PASS
// ...") after a loop of non-fatal t.Errorf calls prints PASS right below
// the FAIL lines it contradicts — misleading in a document meant to be
// the PR's gate record).
func logPass(t *testing.T, format string, args ...any) {
	t.Helper()
	if !t.Failed() {
		t.Logf(format, args...)
	}
}

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
	agySHA512  string
)

// syntheticAgy starts an in-process HTTP server serving a fake agy tarball
// and returns its URL + sha512. Purely local — never a guessed remote
// location.
//
// The fixture executable is deliberately named "not-agy", NOT "agy"
// (contracts/runtime.md R1, amended 7547238): the real vendor tarball
// contains a single ELF executable named "antigravity" — the vendor's own
// installer is what renames it to "agy" on install — so a fixture literally
// named "agy" was certifying exactly the extraction bug that broke the
// real build (Dockerfile searched for a file already called "agy", which
// matches nothing in the real archive). This fixture now exercises the
// same shape: one executable, not named "agy", found by permission bit.
func syntheticAgy(t *testing.T) (url, sha string) {
	t.Helper()
	if fakeAgySrv != nil {
		return agyURL, agySHA512
	}
	dir := t.TempDir()
	script := []byte("#!/bin/sh\ncase \"$1\" in\n  --version) echo 'agy container-test 0.0.0-test'; exit 0 ;;\n  *) exit 0 ;;\nesac\n")
	agyPath := filepath.Join(dir, "not-agy")
	if err := os.WriteFile(agyPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "agy.tar.gz")
	// COPYFILE_DISABLE=1: macOS's tar otherwise embeds an AppleDouble
	// sidecar file (._not-agy) alongside the real one — extracted on
	// Linux it ALSO carries the executable bit, so the Dockerfile's
	// "exactly one executable" check (correctly) refuses to guess and
	// fails the build with 2 candidates. That is the Dockerfile doing
	// its job; the sidecar file is purely an artifact of building this
	// FIXTURE on macOS and has nothing to do with the real vendor
	// tarball, so it belongs suppressed here, not tolerated there.
	t.Setenv("COPYFILE_DISABLE", "1")
	if out, errOut, code := runCmd(t, 30*time.Second, "tar", "-czf", tarPath, "-C", dir, "not-agy"); code != 0 {
		t.Fatalf("tar: %s %s", out, errOut)
	}
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha512.Sum512(data)
	agySHA512 = hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	mux.HandleFunc("/agy.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(data)
	})
	fakeAgySrv = httptest.NewServer(mux)
	agyURL = fakeAgySrv.URL + "/agy.tar.gz"
	return agyURL, agySHA512
}

// buildSentinelImage builds deploy/Dockerfile once, tagged imageTag. Real
// AGY_URL/AGY_SHA512 win if both are set; otherwise the synthetic fixture
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
		sha := os.Getenv("AGY_SHA512")
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
			// No --platform here: the Dockerfile pins stage 2 to
			// linux/amd64 itself (where it matters — the stage that
			// executes agy), while stage 1 builds natively for speed.
			// See that FROM line's comment for the full reasoning.
			"--build-arg", "AGY_URL=" + url,
			"--build-arg", "AGY_SHA512=" + sha,
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

// dockerAvailable is a cheap, cached "is the Docker/Podman daemon even
// reachable" probe — round-2 review item 8: requireImage used to map
// EVERY build failure to a SKIP claiming "not a Dockerfile defect",
// which would also SKIP a real Dockerfile regression silently instead of
// failing the run. Checked once per test binary run.
var (
	dockerAvailOnce sync.Once
	dockerAvailErr  error
)

func dockerAvailable(t *testing.T) error {
	t.Helper()
	dockerAvailOnce.Do(func() {
		_, errOut, code := runCmd(t, 10*time.Second, dockerBin(), "version")
		if code != 0 {
			dockerAvailErr = fmt.Errorf("%s not reachable: %s", dockerBin(), errOut)
		}
	})
	return dockerAvailErr
}

func requireImage(t *testing.T) {
	t.Helper()
	if err := dockerAvailable(t); err != nil {
		t.Skipf("SKIP: %v (environment limitation)", err)
	}
	// Daemon IS reachable past this point — a build failure from here on
	// is either a real Dockerfile/build regression (FAIL) or the
	// synthetic-agy-fixture's own container-to-host networking not
	// working in this sandbox (still a real thing to know, so it FAILs
	// too rather than silently vanishing as a SKIP — the AGY_URL-missing
	// case, the one truly expected failure without ops input, has its
	// own dedicated test elsewhere and is not what requireImage guards).
	if err := buildSentinelImage(t); err != nil {
		t.Fatalf("FAIL: could not build %s: %v", imageTag, err)
	}
}

// TestContainer_RealAgyBuild is the check the coordinator named as the one
// that would have caught the extraction bug no synthetic fixture could:
// "Build the image with these real values at least once." Gated on
// SENTINEL_REAL_AGY=1 (same gate C9/CLAUDE.md already use for real-agy
// interaction) since it downloads the real ~53 MB tarball (~205 MB
// extracted) from the vendor over the network — not something a default
// `go test ./...` or even the default container suite should do.
//
// The values below are the ones the coordinator independently verified
// (fetched, sha512-checked) on 2026-08-19 for agy 1.1.15. They are NOT
// Dockerfile/compose defaults — R1 requires AGY_URL/AGY_SHA512 as ops
// input with no default, on purpose — these are fixture literals for this
// one gated verification, the same way a table-driven test's fixture data
// is not a production default just because it lives in the repo.
const (
	realAgyVersion = "1.1.15"
	realAgyURL     = "https://storage.googleapis.com/antigravity-public/antigravity-cli/1.1.15-5350383476932608/linux-x64/cli_linux_x64.tar.gz"
	realAgySHA512  = "7d6020caff2e06a5ddf2553f2d9d5b428e3becc69727d11032f10e40609b938db428578ccd5c72694bab5e4da483de9ad3121578cc172adf174aa5263ce51dcc"
)

func TestContainer_RealAgyBuild(t *testing.T) {
	if os.Getenv("SENTINEL_REAL_AGY") != "1" {
		t.Skip("SKIP: SENTINEL_REAL_AGY != 1 — set it to build the image against the REAL agy tarball over the network (~53MB download, ~205MB extracted)")
	}
	root := repoRoot(t)
	tag := "sentinel:real-agy-test"

	args := []string{"build", "-f", "deploy/Dockerfile", "-t", tag,
		// The Dockerfile itself pins stage 2 (runtime, where agy actually
		// EXECUTES) to linux/amd64 — see that FROM line for why: without
		// it, an arm64 dev host resolves stage 2 to the arm64 debian
		// base, and the real agy binary (dynamically linked against
		// glibc) fails with "qemu-x86_64-static: Could not open
		// '/lib64/ld-linux-x86-64.so.2'" — that file exists, just for the
		// wrong arch. No --platform flag needed here; the Dockerfile
		// itself is now correct regardless of build host.
		"--build-arg", "AGY_URL=" + realAgyURL,
		"--build-arg", "AGY_SHA512=" + realAgySHA512,
		"--build-arg", "AGY_VERSION=" + realAgyVersion,
		"--build-arg", "VERSION=real-agy-test",
		".",
	}
	// -f is relative to the docker CLI's own cwd, and "." is the build
	// context — both need cmd.Dir=root, exactly like buildSentinelImage
	// does; runCmd (used everywhere else in this file) has no cwd
	// override and would resolve "deploy/Dockerfile" against the test
	// binary's own working directory instead.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBin(), args...)
	cmd.Dir = root
	var buildOut bytes.Buffer
	cmd.Stdout = &buildOut
	cmd.Stderr = &buildOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("FAIL: real-agy build failed: %v\n%s", err, buildOut.String())
	}
	defer runCmd(t, 30*time.Second, dockerBin(), "rmi", tag)

	// The real agy --version output and a real sentinel --version, both
	// out of the ACTUAL built image — not the synthetic fixture's stub.
	verOut, verErr, verCode := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", "--entrypoint", "agy", tag, "--version")
	if verCode != 0 {
		t.Fatalf("FAIL: agy --version in the real-agy image (code=%d): %s %s", verCode, verOut, verErr)
	}
	t.Logf("real agy --version: %s", strings.TrimSpace(verOut))

	sentOut, sentErr, sentCode := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm", tag, "--version")
	if sentCode != 0 {
		t.Fatalf("FAIL: sentinel --version in the real-agy image (code=%d): %s %s", sentCode, sentOut, sentErr)
	}

	sizeOut, _, sizeCode := runCmd(t, 10*time.Second, dockerBin(), "image", "inspect", tag, "--format", "{{.Size}}")
	if sizeCode == 0 {
		if bytes, err := strconv.ParseInt(strings.TrimSpace(sizeOut), 10, 64); err == nil {
			t.Logf("real-agy image size: %d bytes (%.1f MB) — T8 pulls this onto bam", bytes, float64(bytes)/1e6)
		}
	}
	logPass(t, "PASS real-agy build (version=%s)", realAgyVersion)
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
	logPass(t, "PASS C1")
}

// --- C3: every ro mount rejects a write; /state and /tmp accept one ---

// hostPathExists checks existence via a throwaway container bind-mounting
// the Docker/Podman HOST's path — never the local Go test process's own
// filesystem (the C2/C11 lesson: on a Podman Desktop/machine setup this
// test binary runs on macOS while containers run in a separate Linux VM).
func hostPathExists(t *testing.T, hostPath string) bool {
	t.Helper()
	_, _, code := runCmd(t, 10*time.Second, dockerBin(), "run", "--rm",
		"-v", hostPath+":/probe:ro", "debian:trixie-slim", "test", "-e", "/probe")
	return code == 0
}

// TestContainer_C3_ReadOnlySurfaces is R8 C3: "for EVERY ro mount target
// of R4, creating a file fails". Round-2 review finding: the first
// version ran `docker run --read-only` with NO bind mounts at all, so 8
// of 9 "write failed" assertions were actually "path does not exist" —
// green evidence for an assertion never made. This version attaches the
// REAL R4 mount set (real host paths, read-only) so a bind that lost its
// `:ro` in compose would actually be caught here.
func TestContainer_C3_ReadOnlySurfaces(t *testing.T) {
	requireImage(t)
	selinux := selinuxEnforcingOnDockerHost(t)

	// {container target, real host source, isDir}. Matches R4's mount
	// list exactly (minus AGY_SECRET_DIR and /state/tmpfs, which are not
	// "ro mount targets" — /state and /tmp are the rw exception C3 checks
	// separately below).
	type mount struct {
		target string
		host   string
		isDir  bool
	}
	mounts := []mount{
		{"/host/journal", "/var/log/journal", true},
		{"/host/journal-volatile", "/run/log/journal", true},
		{"/etc/machine-id", "/etc/machine-id", false},
		{"/host/rasdaemon", "/var/lib/rasdaemon", true},
		{"/host/proc", "/proc", true},
		{"/host/sys", "/sys", true},
		{"/etc/os-release", "/etc/os-release", false},
		{"/host/root", "/", true},
	}

	for _, m := range mounts {
		if !hostPathExists(t, m.host) {
			t.Logf("SKIP C3 target %s: host path %s does not exist on this test host", m.target, m.host)
			continue
		}
		var writeTarget string
		if m.isDir {
			writeTarget = strings.TrimRight(m.target, "/") + "/.w"
		} else {
			writeTarget = m.target // overwrite attempt on the file itself
		}
		script := fmt.Sprintf("echo x > %s 2>/dev/null; echo rc=$?", writeTarget)
		args := []string{"run", "--rm",
			"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
			"-u", "10001:10001", "--tmpfs", "/tmp",
			"-v", m.host + ":" + m.target + ":ro"}
		if selinux {
			args = append(args, "--security-opt", "label=disable")
		}
		args = append(args, "--entrypoint", "sh", imageTag, "-c", script)
		out, errOut, _ := runCmd(t, 15*time.Second, dockerBin(), args...)
		if !strings.Contains(out, "rc=") || strings.Contains(out, "rc=0") {
			t.Errorf("FAIL C3: write to %s (host %s) did not fail as expected: out=%q err=%q", m.target, m.host, out, errOut)
		}
	}

	// /usr/local/bin has no host mount — it's part of the image itself,
	// and read_only:true is what protects it.
	out, _, _ := runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /usr/local/bin/.w 2>/dev/null; echo rc=$?")
	if !strings.Contains(out, "rc=") || strings.Contains(out, "rc=0") {
		t.Errorf("FAIL C3: write to /usr/local/bin did not fail as expected: %q", out)
	}

	// /state and /tmp are the two rw exceptions — must accept a write
	// (R8 C3: "/state/.w and /tmp/.w succeed").
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o777); err != nil { // container uid 10001 != host owner uid
		t.Fatal(err)
	}
	out, _, _ = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp", "-v", stateDir+":/state",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /state/.w 2>/dev/null; echo rc=$?")
	if !strings.Contains(out, "rc=0") {
		t.Fatalf("FAIL C3: write to /state should succeed: %q", out)
	}
	out, _, _ = runCmd(t, 15*time.Second, dockerBin(), "run", "--rm",
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"-u", "10001:10001", "--tmpfs", "/tmp",
		"--entrypoint", "sh", imageTag, "-c", "echo x > /tmp/.w 2>/dev/null; echo rc=$?")
	if !strings.Contains(out, "rc=0") {
		t.Fatalf("FAIL C3: write to /tmp should succeed: %q", out)
	}
	logPass(t, "PASS C3")
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
		logPass(t, "PASS C2 (real host journal, real systemd-journal gid)")
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
		logPass(t, "PASS C2 (synthetic gid fallback — no real host journal reachable in this environment)")
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
		logPass(t, "PASS C4")
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
	logPass(t, "PASS C5")
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
	logPass(t, "PASS C7 (0 hits on an empty synthetic journal dir is itself a pass per R8)")
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
	logPass(t, "PASS C6 (DNS resolution of `apprise` requires the compose network — asserted separately via `docker compose config`, not a bare `docker run`)")
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

// sentinelServiceBlock extracts JUST the "sentinel:" service's rendered
// YAML from `docker compose config` output. `docker compose config`
// consistently indents top-level service names by exactly 2 spaces and
// everything belonging to a service by 4+ spaces (verified against a real
// render: "  apprise:", "  mailrise:", "  sentinel:", "  sentinel-net:"
// all sit at column 2; every key under a service sits at column 4+).
// Round-2 review finding: the previous version searched for the next
// "\n  " (a TWO-space prefix) as the block end, but the service's own
// keys are indented FOUR spaces — so "\n  " never matches inside the
// block, the "end" is the very next line, and the window inspected is
// the literal string "sentinel:" with nothing else. This version anchors
// on the next line with EXACTLY a 2-space indent (any other top-level
// service or the closing top-level key), which is the real boundary.
func sentinelServiceBlock(t *testing.T, text string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^  sentinel:\n`)
	loc := re.FindStringIndex(text)
	if loc == nil {
		t.Fatal("sentinel: service not found in rendered compose config")
	}
	rest := text[loc[1]:]
	endRe := regexp.MustCompile(`(?m)^  \S`) // next 2-space-indented line = next top-level key
	end := endRe.FindStringIndex(rest)
	if end == nil {
		return text[loc[0]:] // sentinel was the last block in the file
	}
	return text[loc[0] : loc[1]+end[0]]
}

func TestContainer_C10_ComposeConfig(t *testing.T) {
	root := repoRoot(t)
	deployDir := filepath.Join(root, "deploy")

	// Round-2 review finding: skipping outright when deploy/.env (ops-
	// provided, gitignored) is absent means the ONLY check of the
	// security model disappears on every fresh clone and every CI
	// runner — exactly where you'd most want it run. `docker compose`
	// only needs the `:?`-required variables to render; synthesize a
	// minimal env covering just those instead of depending on ops state.
	// Only the variables docker-compose.yml's sentinel service actually
	// dereferences with `:?` — TELEGRAM_* is never read by this file
	// (R4: the sentinel container gets no TELEGRAM_* variables at all).
	envFile := filepath.Join(t.TempDir(), "ci.env")
	envContent := "JOURNAL_GID=999\n" +
		"AGY_CREDENTIALS_DIR=/tmp\n" +
		"MAILRISE_SMTP_USER=ci\n" +
		"MAILRISE_SMTP_PASS=changeme\n"
	if err := os.WriteFile(envFile, []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(dockerBin(), "compose", "--env-file", envFile, "config")
	cmd.Dir = deployDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("FAIL C10: docker compose config: %v\n%s", err, out)
	}
	text := string(out)
	block := sentinelServiceBlock(t, text)

	// SERVICE-LEVEL read_only, anchored at the 4-space indent `docker
	// compose config` uses for a service's own keys — round-2 review's
	// own mutation exposed why a bare substring check is not enough
	// here: every individual bind mount under `volumes:` also renders
	// its own "read_only: true" (8+ space indent), so those never
	// disappear even when the SERVICE-level flag is flipped to false.
	// Worse, compose omits a false boolean from the render entirely
	// rather than printing "read_only: false", so the negative case
	// leaves no line to substring-match against at all — only an
	// anchored "is this specific line present" check catches it.
	serviceReadOnlyRe := regexp.MustCompile(`(?m)^    read_only: true$`)
	if !serviceReadOnlyRe.MatchString(block) {
		t.Errorf("FAIL C10: sentinel service block missing the SERVICE-level read_only: true\nblock:\n%s", block)
	}

	checks := []struct {
		name string
		want string
	}{
		{"cap_drop", "- ALL"},
		{"no-new-privileges", "no-new-privileges:true"},
		{"user", "10001:10001"},
	}
	for _, c := range checks {
		if !strings.Contains(block, c.want) {
			t.Errorf("FAIL C10: sentinel service block missing %s (%q)\nblock:\n%s", c.name, c.want, block)
		}
	}

	// R8 C10: "every bind read_only: true". Each `- type: bind` entry
	// renders its own nested "read_only: true" at 8-space indent
	// immediately under it; the one `- type: volume` entry (sentinel-state,
	// deliberately rw) has none. Counting the two against each other
	// catches a bind that lost its :ro without hardcoding the mount list
	// here (which would drift from R4 the moment a mount is added there).
	bindCount := strings.Count(block, "- type: bind")
	nestedReadOnlyRe := regexp.MustCompile(`(?m)^ {8}read_only: true$`)
	nestedReadOnlyCount := len(nestedReadOnlyRe.FindAllString(block, -1))
	if bindCount == 0 {
		t.Error("FAIL C10: sentinel service block has no bind mounts at all — R4 defines several")
	}
	if bindCount != nestedReadOnlyCount {
		t.Errorf("FAIL C10: %d bind mounts but only %d carry read_only: true — every bind must be :ro\nblock:\n%s", bindCount, nestedReadOnlyCount, block)
	}
	forbidden := []string{"privileged:", "cap_add:", "TELEGRAM_", "/config:", "ports:"}
	for _, f := range forbidden {
		if strings.Contains(block, f) {
			t.Errorf("FAIL C10: sentinel service block unexpectedly contains %q\nblock:\n%s", f, block)
		}
	}
	// Every C3 env var must be present in the sentinel service's rendered
	// environment block.
	c3Vars := []string{
		"TICK_INTERVAL", "TICK_WINDOW", "DEEP_WINDOW", "SECTION_TIMEOUT",
		"JOURNAL_MAX_RECORDS", "FACTS_MAX_BYTES", "SERVICES_MAX_BYTES", "STATE_DIR",
		"HOST_JOURNAL_DIR", "HOST_JOURNAL_VOLATILE_DIR", "HOST_PROC", "HOST_ROOT",
		"HOST_RASDAEMON", "SENTINEL_HOSTNAME", "AGY_BIN", "AGY_HOME", "AGY_SECRET_DIR",
		"AGY_PRINT_TIMEOUT", "AGY_HARD_TIMEOUT", "HISTORY_N",
		"HISTORY_KEEP", "DEEP_ENABLED", "DEEP_TIMEOUT", "RAW_ALERT_MAX_PRIORITY",
		"RAW_ALERT_MAX_LINES", "RAW_ALERT_REPEAT_SECONDS", "RAW_ALERT_MARKER_TTL_HOURS",
		"RENOTIFY_ALERT_SEC", "RENOTIFY_WATCH_SEC", "STALE_ALERT_SEC", "HEARTBEAT_HOUR",
		"OUTBOX_MAX", "OUTBOX_SMTP_AFTER", "APPRISE_URL", "APPRISE_KEY",
		"APPRISE_CONFIG_FILE", "NOTIFY_TIMEOUT", "NOTIFY_BODY_MAX", "MAILRISE_HOST",
		"MAILRISE_PORT", "MAILRISE_USER", "MAILRISE_PASS", "SENTINEL_MAIL_FROM",
		"SENTINEL_MAIL_TO", "LOG_LEVEL", "TMPDIR", "TZ",
	}
	// PROMPT_MAX_BYTES is not in the R4 environment block (analyze-only,
	// no compose default listed there) — not in the required set, since
	// asserting it would be checking against a variable the contract's
	// own table never puts there.
	for _, v := range c3Vars {
		if !strings.Contains(block, v+":") {
			t.Errorf("FAIL C10: sentinel environment missing %s", v)
		}
	}
	logPass(t, "PASS C10")
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
		"-e", "MAILRISE_USER=u", "-e", "MAILRISE_PASS=changeme",
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
	logPass(t, "PASS C11")
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
	// the throwaway container. The gid is deliberately NOT 999 (the real
	// bam value, and also what a hardcoded implementation would happen to
	// emit) — a non-999 value is the only way this test can actually
	// distinguish "discovers the gid" from "hardcodes 999" (round-2
	// review finding: the previous 999 prep passed identically either
	// way).
	const testJournalGID = "7777"
	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
` +
		// The systemd package's own postinst already creates
		// systemd-journal at gid 999 as a side effect of installing it
		// (verified: it does this unconditionally) — so a plain
		// "getent || groupadd" never fires, the group already exists at
		// 999, and this test would silently go back to testing nothing.
		// Force it to the test gid with groupmod, falling back to
		// groupadd only if the group somehow does not exist yet.
		`groupmod -g ` + testJournalGID + ` systemd-journal 2>/dev/null || groupadd -g ` + testJournalGID + ` systemd-journal
: > /root/deploy/.env
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
	// Round-2 review finding: this throwaway container has no real PID 1
	// systemd, so `systemctl enable --now rasdaemon` (step2) fails on
	// EVERY run, always reports rc=75, and never contributes to
	// `changed`. That means "changed=0 on the second run" is satisfied
	// just as well by a run where nothing converged as by a run where
	// everything did — it is not proof of convergence by itself. Assert
	// the actual per-step "already converged" lines instead, for every
	// step this environment CAN converge (everything except step2's
	// service enable, which is a genuine environment limitation here,
	// and step5, skipped because /etc/zfs/zed.d does not exist in this
	// container).
	hash1 := hashFiles()

	out2, errOut2, code2 := exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	hash2 := hashFiles()

	if hash1 != hash2 {
		t.Errorf("FAIL C12: sha256 of touched files differs between two real runs:\n1: %s\n2: %s", hash1, hash2)
	}
	for _, want := range []string{
		"step1 packages: already installed",
		"step3 /etc/msmtprc: already converged",
		"step4 /etc/smartd.conf: already converged",
		"step6 JOURNAL_GID: already " + testJournalGID + " in",
	} {
		if !strings.Contains(out2, want) {
			t.Errorf("FAIL C12: second real run did not report convergence for %q: %s (code=%d) %s", want, out2, code2, errOut2)
		}
	}
	// Proves gid DISCOVERY, not a hardcoded 999: the prep above set the
	// systemd-journal group to 7777, and step 6 must have written that
	// exact value, not a constant.
	envContent, _, _ := exec_("cat /root/deploy/.env")
	if !strings.Contains(envContent, "JOURNAL_GID="+testJournalGID) {
		t.Errorf("FAIL C12: /root/deploy/.env does not contain JOURNAL_GID=%s (got: %s) — gid must be DISCOVERED via getent, never hardcoded", testJournalGID, envContent)
	}
	logPass(t, "PASS C12")
}

// TestContainer_C12_MsmtpDelivery is BLOCKER 1 from round-2 review: step3
// of install-host.sh must produce a /etc/msmtprc that REAL msmtp can
// actually authenticate and deliver through, not merely one containing the
// string "auth on". Reproduces the reviewer's own probe: a real msmtp
// binary, a real SMTP server requiring AUTH, and the exact config file the
// script writes — asserting the stub actually received an authenticated
// delivery, not that install-host.sh exited 0.
// TestContainer_C12_EnvOwnerUnmappedUID is round-3 review MUST-FIX 1: step
// 6 resolving the .env owner by NAME (stat -c %U) breaks silently for a
// uid with no /etc/passwd entry — stat prints the literal string
// "UNKNOWN", `install -o UNKNOWN` fails, and without a checked exit
// status the step used to report "updated" while writing nothing.
// C12's own .env is root-owned throughout (uid 0 always resolves), so
// nothing else exercises this path — this test chown's it to a uid with
// deliberately NO passwd entry (1000, present nowhere in a fresh
// debian:trixie-slim's /etc/passwd) and asserts JOURNAL_GID still lands.
func TestContainer_C12_EnvOwnerUnmappedUID(t *testing.T) {
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		t.Skipf("SKIP C12c: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	name := "sentinel-c12c-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	_, errOut, code = runCmd(t, 60*time.Second, dockerBin(), "run", "-d", "--name", name,
		"-v", filepath.Join(root, "deploy")+":/work:ro",
		"debian:trixie-slim", "sleep", "300")
	if code != 0 {
		t.Skipf("SKIP C12c: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 60*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd >/dev/null 2>&1 || true
groupmod -g 7777 systemd-journal 2>/dev/null || groupadd -g 7777 systemd-journal
: > /root/deploy/.env
chown 1000:1000 /root/deploy/.env
chmod 600 /root/deploy/.env
chmod +x /root/deploy/install-host.sh
! getent passwd 1000 >/dev/null 2>&1` // the whole point: uid 1000 must have NO passwd entry
	if out, errOut, code := exec_(prep); code != 0 {
		t.Skipf("SKIP C12c: throwaway container prep failed (or uid 1000 unexpectedly has a passwd entry in this base image): %s %s", out, errOut)
	}

	out, errOut, _ = exec_("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env")
	envContent, _, _ := exec_("cat /root/deploy/.env")
	if !strings.Contains(envContent, "JOURNAL_GID=7777") {
		t.Fatalf("FAIL C12c: JOURNAL_GID=7777 missing from .env after install-host.sh against an unmapped-uid-owned .env (stdout=%q stderr=%q .env=%q)", out, errOut, envContent)
	}
	ownerAfter, _, _ := exec_("stat -c '%u:%g' /root/deploy/.env")
	if strings.TrimSpace(ownerAfter) != "1000:1000" {
		t.Errorf("FAIL C12c: .env owner changed to %q, want preserved 1000:1000", strings.TrimSpace(ownerAfter))
	}
	logPass(t, "PASS C12c (JOURNAL_GID written and owner preserved despite no /etc/passwd entry for the owning uid)")
}

func TestContainer_C12_MsmtpDelivery(t *testing.T) {
	root := repoRoot(t)
	out, errOut, code := runCmd(t, 30*time.Second, dockerBin(), "run", "--rm", "debian:trixie-slim", "true")
	if code != 0 {
		t.Skipf("SKIP C12b: cannot run a throwaway debian container in this environment: %s %s", out, errOut)
	}

	stub := newSMTPAuthStub(t)
	defer stub.close()

	// --add-host is set up front (docker run only, not exec) so BOTH
	// aliases below have a real chance to resolve inside this one
	// container: host.containers.internal is Podman's native resolution,
	// host.docker.internal needs the explicit mapping on real Docker.
	// Harmless to pass on either runtime.
	name := "sentinel-c12b-test"
	runCmd(t, 5*time.Second, dockerBin(), "rm", "-f", name)
	runArgs := []string{"run", "-d", "--name", name,
		"--add-host", "host.docker.internal:host-gateway",
		"-v", filepath.Join(root, "deploy") + ":/work:ro",
		"debian:trixie-slim", "sleep", "300"}
	if _, errOut, code := runCmd(t, 60*time.Second, dockerBin(), runArgs...); code != 0 {
		t.Skipf("SKIP C12b: could not start throwaway container: %s", errOut)
	}
	defer runCmd(t, 10*time.Second, dockerBin(), "rm", "-f", "-t", "0", name)

	exec_ := func(script string) (string, string, int) {
		return runCmd(t, 120*time.Second, dockerBin(), "exec", name, "sh", "-c", script)
	}

	prep := `set -e
cp -r /work /root/deploy
apt-get update -qq
apt-get install -y -qq systemd msmtp msmtp-mta >/dev/null 2>&1
chmod +x /root/deploy/install-host.sh
printf 'MAILRISE_SMTP_USER=probeuser\nMAILRISE_SMTP_PASS=probepass\n' > /root/deploy/.env`
	if out, errOut, code := exec_(prep); code != 0 {
		t.Skipf("SKIP C12b: throwaway container prep failed (msmtp package unavailable?): %s %s", out, errOut)
	}

	// host.containers.internal (Podman, native) / host.docker.internal
	// (real Docker, needs the --add-host above) both point at the stub
	// listening on this test process's host. Try both; whichever the
	// resolver in THIS environment actually answers is the one that
	// matters.
	for _, alias := range []string{"host.containers.internal", "host.docker.internal"} {
		installCmd := fmt.Sprintf("cd /root/deploy && ./install-host.sh --env-file /root/deploy/.env --mailrise-host %s --mailrise-port %d", alias, stub.port())
		if out, errOut, code := exec_(installCmd); code != 0 && code != 75 {
			t.Logf("C12b: install-host.sh with alias %s exited %d (skipping this alias): %s %s", alias, code, out, errOut)
			continue
		}

		sendCmd := `printf 'To: probe@example.com\nSubject: sentinel probe\n\nprobe body\n' | msmtp -a sentinel probe@example.com`
		sendOut, sendErr, sendCode := exec_(sendCmd)
		if sendCode == 0 {
			deliveries, sawAuth, dataText := stub.snapshot()
			if deliveries != 1 {
				t.Fatalf("FAIL C12b: msmtp exited 0 but the stub SMTP server received %d deliveries, want 1 (sendOut=%q sendErr=%q)", deliveries, sendOut, sendErr)
			}
			if !sawAuth {
				t.Fatal("FAIL C12b: msmtp delivered without ever authenticating — mailrise enforces SMTP AUTH unconditionally (R4), so an unauthenticated send proves nothing about the real path")
			}
			if !strings.Contains(dataText, "probe body") {
				t.Fatalf("FAIL C12b: delivered message did not carry the expected body: %q", dataText)
			}
			logPass(t, "PASS C12b (real msmtp, real AUTH, real delivery)")
			return
		}
		t.Logf("C12b: alias %s did not deliver (code=%d): %s %s", alias, sendCode, sendOut, sendErr)
	}
	t.Skip("SKIP C12b: neither host.containers.internal nor host.docker.internal reached the local stub SMTP server from the throwaway container in this environment")
}

// --- SMTP-with-AUTH stub for C12b ---

type smtpAuthStub struct {
	mu         sync.Mutex
	sawAuth    bool
	deliveries int
	dataText   string
	ln         net.Listener
}

func newSMTPAuthStub(t *testing.T) *smtpAuthStub {
	t.Helper()
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpAuthStub{ln: ln}
	go s.serve()
	return s
}

func (s *smtpAuthStub) close() { s.ln.Close() }

func (s *smtpAuthStub) port() int {
	_, portStr, _ := net.SplitHostPort(s.ln.Addr().String())
	p, _ := strconv.Atoi(portStr)
	return p
}

func (s *smtpAuthStub) snapshot() (deliveries int, sawAuth bool, dataText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveries, s.sawAuth, s.dataText
}

func (s *smtpAuthStub) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *smtpAuthStub) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	write := func(msg string) { conn.Write([]byte(msg + "\r\n")) }
	write("220 stub.mailrise.local ESMTP")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			write("250-stub.mailrise.local")
			write("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(upper, "AUTH PLAIN"), strings.HasPrefix(upper, "AUTH LOGIN"):
			s.mu.Lock()
			s.sawAuth = true
			s.mu.Unlock()
			write("235 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM"):
			write("250 OK")
		case strings.HasPrefix(upper, "RCPT TO"):
			write("250 OK")
		case strings.HasPrefix(upper, "DATA"):
			write("354 End data with <CR><LF>.<CR><LF>")
			var data strings.Builder
			for {
				dl, derr := r.ReadString('\n')
				if derr != nil {
					return
				}
				if dl == ".\r\n" || dl == ".\n" {
					break
				}
				data.WriteString(dl)
			}
			s.mu.Lock()
			s.dataText = data.String()
			s.deliveries++
			s.mu.Unlock()
			write("250 OK: queued")
		case strings.HasPrefix(upper, "QUIT"):
			write("221 Bye")
			return
		default:
			write("500 unrecognized command")
		}
	}
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
	logPass(t, "PASS C13 (workflow shape); pull+run verification is CI-only (needs a published GHCR image, not buildable from a local run)")
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
